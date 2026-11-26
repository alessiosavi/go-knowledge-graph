package graph

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/neo4j/neo4j-go-driver/v6/neo4j"
	driverconfig "github.com/neo4j/neo4j-go-driver/v6/neo4j/config"
)

func TestResolveIngestDomain(t *testing.T) {
	row := func(id, name string) *neo4j.Record {
		return &neo4j.Record{Keys: []string{"id", "name"}, Values: []any{id, name}}
	}
	down := errors.New("graph unavailable")
	for _, tc := range []struct {
		name, value, want, code, detail string
		exact, names                    []*neo4j.Record
		idErr, namesErr                 error
		wantReads                       int
	}{
		{name: "id before colliding names", value: "exact", want: "exact",
			exact: []*neo4j.Record{row("exact", "Different name")}, wantReads: 1},
		{name: "case insensitive name", value: "RICERCA / CITTÀ", want: "research",
			names: []*neo4j.Record{row("research", "Ricerca / Città")}, wantReads: 2},
		{name: "stored name with outer whitespace", value: "archive / notes", want: "archive",
			names: []*neo4j.Record{row("archive", " Archive / Notes\t")}, wantReads: 2},
		{name: "ambiguous trimmed name", value: "research", code: "ambiguous_domain", detail: "one, two",
			names: []*neo4j.Record{row("two", " RESEARCH "), row("one", "Research")}, wantReads: 2},
		{name: "unknown with punctuation", value: ": /", code: "unknown_domain",
			detail: "GET /v1/domains lists them", wantReads: 2},
		{name: "id lookup failure", value: "exact", idErr: down, wantReads: 1},
		{name: "name lookup failure", value: "research", namesErr: down, wantReads: 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			reads := 0
			got, err := resolveIngestDomain(t.Context(), tc.value, func(_ context.Context, query string, params map[string]any) ([]*neo4j.Record, error) {
				reads++
				if reads == 1 {
					if !strings.Contains(query, "{id:$id}") || params["id"] != tc.value {
						t.Fatalf("first read is not an exact ID lookup: %s %v", query, params)
					}
					return tc.exact, tc.idErr
				}
				if strings.Contains(query, ":Document") || strings.Contains(query, ":Product") {
					t.Fatalf("name lookup counts corpus data: %s", query)
				}
				return tc.names, tc.namesErr
			})
			if got != tc.want || reads != tc.wantReads {
				t.Errorf("resolved %q after %d reads, want %q after %d reads", got, reads, tc.want, tc.wantReads)
			}
			switch {
			case tc.code != "":
				var rejection *IngestDomainError
				if !errors.As(err, &rejection) || rejection.Conflict || rejection.Code != tc.code || !strings.Contains(rejection.Detail, tc.detail) {
					t.Errorf("error = %v, want %s with %q", err, tc.code, tc.detail)
				}
			default:
				want := tc.idErr
				if want == nil {
					want = tc.namesErr
				}
				if !errors.Is(err, want) {
					t.Errorf("error = %v, want %v", err, want)
				}
			}
		})
	}
}

func TestChosenDomainRejectsExistingVersionInTransaction(t *testing.T) {
	row := func(keys []string, values ...any) neo4j.Result {
		return foldTestRows{rows: []*neo4j.Record{{Keys: keys, Values: values}}}
	}
	tx := foldTestTx{run: func(query string, _ map[string]any) (neo4j.Result, error) {
		switch {
		case strings.Contains(query, "MATCH (m:GkgMeta"):
			return row([]string{"version"}, int64(4)), nil
		case strings.Contains(query, "SET d.product_guard"):
			return row([]string{"d.id"}, "chosen"), nil
		case strings.Contains(query, "RETURN d.id AS id, d.status AS status"):
			return row([]string{"id", "status", "domain"}, "old", "superseded", "archive"), nil
		case query == ingestDomainByIDQuery:
			return row([]string{"id", "name"}, "archive", "Old Archive"), nil
		default:
			t.Fatalf("admission reached an unexpected query: %s", query)
			return nil, nil
		}
	}}
	in := DocumentInput{Kind: "text", Source: "source", DomainChosen: true, Domain: "chosen", Force: true}
	_, err := txUpsertDocument(t.Context(), tx, "old", in)
	var rejection *IngestDomainError
	if !errors.As(err, &rejection) || !rejection.Conflict || rejection.Code != "domain_conflict" ||
		!strings.Contains(rejection.Detail, "Old Archive (archive), document old") {
		t.Fatalf("admission error = %v, want a conflict naming the existing version", err)
	}
}

func TestChosenDomainDeletionDuringAdmission(t *testing.T) {
	st := foldIntegrationStore(t)
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	in := DocumentInput{Kind: "text", Source: t.Name(), Hash: "first", Domain: "vanishing", DomainChosen: true}
	id := DocumentID(in.Kind, in.Source, in.Hash)
	foldTestWrite(t, st, `MERGE (m:GkgMeta {id:'extraction_schema'}) SET m.value=$version
CREATE (:Domain {id:$domain})`, map[string]any{"version": SchemaVersionV4, "domain": in.Domain})

	// Observe the first delete attempt while admission is still open. A
	// managed retry can only see the committed document after we release it.
	driver, err := neo4j.NewDriver(os.Getenv("GKG_TEST_MEMGRAPH_URI"), neo4j.NoAuth(), func(c *driverconfig.Config) {
		c.MaxTransactionRetryTime = 0
	})
	if err != nil {
		t.Fatal(err)
	}
	defer driver.Close(context.Background())
	deleter := &Store{driver: driver}
	session := st.driver.NewSession(ctx, neo4j.SessionConfig{AccessMode: neo4j.AccessModeWrite})
	defer session.Close(ctx)
	admission, err := session.BeginTransaction(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer admission.Close(ctx)
	if _, err := txUpsertDocument(ctx, admission, id, in); err != nil {
		t.Fatalf("stage chosen-domain admission: %v", err)
	}
	if deleted, err := deleter.DeleteEmptyDomain(ctx, in.Domain); deleted || !IsTransientWriteError(err) {
		t.Errorf("delete during admission = %t, %v, want a write conflict", deleted, err)
	}
	if err := admission.Commit(ctx); err != nil {
		t.Fatalf("commit admission: %v", err)
	}
	if deleted, err := deleter.DeleteEmptyDomain(ctx, in.Domain); deleted || err != nil {
		t.Fatalf("delete retry = %t, %v, want the occupied domain to remain", deleted, err)
	}
	ids, err := st.DomainDocumentIDs(ctx, in.Domain)
	if err != nil || len(ids) != 1 || ids[0] != id {
		t.Fatalf("final domain membership = %v, %v, want [%s]", ids, err, id)
	}
}
