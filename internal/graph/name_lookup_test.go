package graph

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/neo4j/neo4j-go-driver/v6/neo4j"
)

func TestFoldNameLookup(t *testing.T) {
	st := foldIntegrationStore(t)
	ctx := t.Context()
	docKey := ScopedEntityKey("doc", "Vorq-un")
	foldTestWrite(t, st, `CREATE
(:Entity {id:'bare', key:'quav-ulon', name:'Quav-ulon', name_key:'quav-ulon'}),
(:Entity {id:'tie-winner', key:'@velq:quav-ulon', domain:'velq', name:'Quav-ulon', name_key:'quav-ulon', mention_count:5}),
(:Entity {id:'tie-loser', key:'@velq:quavulón', domain:'velq', name:'Quavulón', name_key:'quavulón', mention_count:5}),
(:Entity {id:'lower', key:'@velq:qua-vulon', domain:'velq', name:'Qua-vulon', name_key:'qua-vulon', mention_count:2}),
(:Entity {id:'exact', key:'@xelq:quavulon', domain:'xelq', name:'Quavulon', name_key:'quavulon'}),
(:Entity {id:'mismatch', key:'@velq:q-uavulon', domain:'xelq', name:'Q-uavulon', name_key:'q-uavulon', mention_count:99}),
(:Entity {id:'bare-mismatch', key:'q-uavulon', domain:'velq', name:'Q-uavulon', name_key:'q-uavulon', mention_count:99}),
(:Entity {id:'veto-match', key:'@velq:zin-vox', domain:'velq', name:'Zin-vox', name_key:'zin-vox'}),
(:Entity {id:'doc-bare', key:$bare_doc, name:'Vorq-un', name_key:'vorq-un'}),
(:Entity {id:'doc-domain', key:$scoped_doc, domain:'velq', name:'Vorq-un', name_key:'vorq-un'}),
(:Entity {id:'stale', key:'@velq:xol-bix', domain:'velq', name:'Xol-bix', name_key:'xol-bix'})`,
		map[string]any{"bare_doc": docKey, "scoped_doc": DomainKey("velq", docKey)})
	ex := foldTestExceptions(t, "@velq:zinvox")
	foldTestRebuild(t, st, &ex, 9, 0)
	foldTestRebuild(t, st, nil, 0, 0)
	if got := foldTestRecord(t, st).Scopes; !slices.Equal(got, []string{"", "@velq", "@xelq"}) {
		t.Fatalf("rebuild scopes = %v", got)
	}
	foldTestWrite(t, st, `MATCH (e:Entity {id:'doc-bare'}) SET e.fold_key = '|vorqun'`, nil)
	foldTestWrite(t, st, `MATCH (e:Entity {id:'doc-domain'}) SET e.fold_key = '@velq|vorqun'`, nil)
	foldTestWrite(t, st, `MATCH (e:Entity {id:'stale'}) SET e.name = 'Xolvox'`, nil)
	for _, tc := range []struct {
		label, name, slug string
		want              []string
		match             string
	}{
		{"exact suppresses other domains", "Quavulon", "", []string{"exact"}, EntityMatchExact},
		{"unregistered scope, canonical tie break, domain mismatch", "Quavulon", "velq", []string{"tie-winner"}, EntityMatchFold},
		{"bare and unregistered scopes", "Qua.vulon", "", []string{"tie-winner", "exact", "bare"}, EntityMatchFold},
		{"query veto", "Zinvox", "velq", nil, ""},
		{"no document folds", "Vorqun", "", nil, ""},
		{"exact document hit", "Vorq-un", "velq", []string{"doc-domain"}, EntityMatchExact},
		{"stale index hit", "Xolbix", "velq", nil, ""},
	} {
		t.Run(tc.label, func(t *testing.T) {
			got, err := st.EntityCandidatesByName(ctx, tc.name, tc.slug)
			if err != nil || len(got) != len(tc.want) {
				t.Fatalf("candidates = %+v, %v, want %v", got, err, tc.want)
			}
			for i, id := range tc.want {
				if got[i].ID != id || got[i].Match != tc.match {
					t.Errorf("candidate %d = %+v, want %s with match %s", i, got[i], id, tc.match)
				}
			}
		})
	}
	for _, selector := range []string{"Qua.vulon", "@velq:quavulon", EntityID("@velq:quavulon")} {
		if _, err := st.EntityViewOpts(ctx, selector, ViewOptions{Domain: "velq"}); !errors.Is(err, ErrNotFound) {
			t.Errorf("view selector %q: %v, want ErrNotFound", selector, err)
		}
		if _, err := st.TraverseOpts(ctx, selector, "", "out", 1, 10, ViewOptions{Domain: "velq"}); !errors.Is(err, ErrNotFound) {
			t.Errorf("traverse selector %q: %v, want ErrNotFound", selector, err)
		}
	}
}

func TestFoldNameLookupBatchesAndSkips(t *testing.T) {
	st := foldIntegrationStore(t)
	ctx := t.Context()
	foldTestWrite(t, st, `CREATE
(:Entity {id:'bare', key:'quer-vix', name:'Quer-vix', name_key:'quer-vix'}),
(:Entity {id:'scoped', key:'@zurx:quer-vix', domain:'zurx', name:'Quer-vix', name_key:'quer-vix'})`, nil)
	check := func(name, domain string, wantHits int, wantFolds ...string) {
		t.Helper()
		queries := CaptureReadQueries(st, func() {
			hits, err := st.EntityCandidatesByName(ctx, name, domain)
			if err != nil || len(hits) != wantHits {
				t.Fatalf("query %q/%q = %+v, %v, want %d hits", name, domain, hits, err, wantHits)
			}
		})
		var batches int
		for _, query := range queries {
			if query.Query == foldRecordQuery && name == "Quer-vix" {
				t.Error("exact hit read fold metadata")
			}
			if query.Query != foldCandidatesQuery {
				continue
			}
			batches++
			folds, _ := query.Params["folds"].([]string)
			if !slices.Equal(folds, wantFolds) {
				t.Errorf("batched folds = %v, want %v", folds, wantFolds)
			}
		}
		if batches > 1 || (batches == 1) != (len(wantFolds) > 0) {
			t.Errorf("query %q/%q used %d batches, want a batch for %v", name, domain, batches, wantFolds)
		}
	}
	check("Quervix", "", 0) // Missing fold record.
	foldTestRebuild(t, st, &FoldExceptions{}, 2, 0)
	check("Quer-vix", "", 2)
	check("™", "", 0) // Empty fold, even though the normalized key is nonempty.
	check("Quervix", "", 2, "|quervix", "@zurx|quervix")
	check("Quervix", "zurx", 1, "@zurx|quervix")
	check("Quervix", "volx", 0)
	rows, err := st.runRead(ctx, "EXPLAIN "+foldCandidatesQuery, map[string]any{
		"folds": []string{"|quervix", "@zurx|quervix"}, "limit": 200,
	})
	if err != nil {
		t.Fatal(err)
	}
	var indexed, limited bool
	for _, row := range rows {
		line := getString(row, "QUERY PLAN")
		t.Log(line)
		if strings.Contains(line, "ScanAllByLabelProperties") && strings.Contains(line, ":Entity {fold_key}") {
			indexed = true
		} else if strings.Contains(line, "ScanAll") {
			t.Errorf("unexpected scan in batched query: %s", line)
		}
		if strings.Contains(line, "Limit") {
			limited = true
		}
	}
	if !indexed {
		t.Fatal("batched fold lookup did not use the Entity(fold_key) index")
	}
	if !limited {
		t.Fatal("batched fold lookup lacked a server-side limit")
	}
	// A T2a record has no scopes until rebuilt. Exact lookup still works.
	foldTestWrite(t, st, `MATCH (m:GkgMeta {id:'fold'}) REMOVE m.scopes`, nil)
	check("Quervix", "", 0)
	check("Quervix", "zurx", 0)
	check("Quer-vix", "", 2)
	foldTestRebuild(t, st, nil, 0, 0)
	check("Quervix", "zurx", 1, "@zurx|quervix")
	foldTestWrite(t, st, `MATCH (m:GkgMeta {id:'fold'}) SET m.fold_version = $version`, map[string]any{"version": FoldKeyVersion + 1})
	check("Quervix", "", 0)
}

func TestFoldCandidatesTxCapsDuplicateFolds(t *testing.T) {
	st := foldIntegrationStore(t)
	ctx := t.Context()
	foldTestWrite(t, st, `UNWIND range(1, 205) AS i
CREATE (:Entity {id:'cap-'+toString(i), key:'cap-'+toString(i), name:'Cap-Probe'})`, nil)
	foldTestWrite(t, st, `CREATE (:Entity {id:'spare', key:'spare', name:'Spare-Probe'})`, nil)
	foldTestRebuild(t, st, &FoldExceptions{}, 206, 0)
	for _, tc := range []struct {
		folds []string
		want  int
	}{
		{[]string{"|capprobe"}, 100},
		{[]string{"|capprobe", "|spareprobe"}, 200},
	} {
		err := st.runReadTx(ctx, func(tx neo4j.ManagedTransaction) error {
			record, err := txReadFoldRecord(ctx, tx)
			if err != nil {
				return err
			}
			hits, err := foldCandidatesTx(ctx, tx, tc.folds, record)
			if err != nil {
				return err
			}
			if len(hits) != tc.want {
				t.Errorf("folds %v returned %d candidates, want %d", tc.folds, len(hits), tc.want)
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
}

func TestChooseCanonical(t *testing.T) {
	a := foldCandidate{EntityRef: EntityRef{ID: "z-id", Key: "a"}, MentionCount: 3}
	b := foldCandidate{EntityRef: EntityRef{ID: "a-id", Key: "b"}, MentionCount: 3}
	c := foldCandidate{EntityRef: EntityRef{Key: "0"}, MentionCount: 1}
	if _, ok := chooseCanonical(nil); ok {
		t.Fatal("empty group has a winner")
	}
	for _, order := range [][]foldCandidate{{a, b, c}, {c, b, a}} {
		if got, ok := chooseCanonical(order); !ok || got != a {
			t.Errorf("winner = %+v, %t, want %+v", got, ok, a)
		}
	}
	var hits []foldCandidate
	for i := 0; i < nameCandidateLimit+2; i++ {
		domain := fmt.Sprintf("z%03d", i)
		hits = append(hits, foldCandidate{EntityRef: EntityRef{Domain: domain, Key: DomainKey(domain, "z")}, MentionCount: i})
	}
	hits = append(hits, foldCandidate{EntityRef: EntityRef{Domain: "z101", Key: "@z101:za"}})
	winners := canonicalNameCandidates(hits)
	if len(winners) != nameCandidateLimit || winners[0].Domain != "z101" || winners[len(winners)-1].Domain != "z002" {
		t.Fatalf("cap or ordering after domain deduplication: %+v", winners)
	}
}

type foldTestTx struct {
	run func(string, map[string]any) (neo4j.Result, error)
}

func (tx foldTestTx) Run(_ context.Context, query string, params map[string]any) (neo4j.Result, error) {
	return tx.run(query, params)
}

type foldTestRows struct {
	neo4j.Result
	rows []*neo4j.Record
}

func (result foldTestRows) Collect(context.Context) ([]*neo4j.Record, error) {
	return result.rows, nil
}

func TestFoldCandidatesTx(t *testing.T) {
	ctx := t.Context()
	record := &FoldRecord{Version: FoldKeyVersion, Exceptions: foldTestExceptions(t, "@zurx:veto")}
	const fold = "@zurx|quervix"
	row := func(id, key, name, domain string) *neo4j.Record {
		return &neo4j.Record{
			Keys:   []string{"id", "key", "name", "domain", "fold_key", "mention_count"},
			Values: []any{id, key, name, domain, fold, int64(2)},
		}
	}
	var calls int
	tx := foldTestTx{run: func(q string, p map[string]any) (neo4j.Result, error) {
		calls++
		if q != foldCandidatesQuery || !slices.Equal(p["folds"].([]string), []string{fold}) {
			t.Fatalf("unexpected query %q with %v", q, p)
		}
		return foldTestRows{rows: []*neo4j.Record{
			row("live", "@zurx:quer-vix", "Quer-vix", "zurx"),
			row("veto", "@zurx:veto", "Quer-vix", "zurx"),
			row("renamed", "@zurx:old", "Quervox", "zurx"),
			row("other", "@zurx:other", "Quer-vix", "other"),
			row("key-scope", "@other:quer-vix", "Quer-vix", "zurx"),
			row("doc", "@zurx:#doc:quer-vix", "Quer-vix", "zurx"),
			row("", "@zurx:idless", "Quer-vix", "zurx"),
		}}, nil
	}}
	got, err := foldCandidatesTx(ctx, tx, []string{fold}, record)
	if err != nil || len(got) != 1 || got[0].ID != "live" || got[0].MentionCount != 2 || calls != 1 {
		t.Fatalf("verified candidates = %+v, %v, calls=%d", got, err, calls)
	}
}
