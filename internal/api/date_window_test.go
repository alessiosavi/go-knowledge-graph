package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/alessiosavi/go-knowledge-graph/internal/config"
	"github.com/alessiosavi/go-knowledge-graph/internal/dedup"
	"github.com/alessiosavi/go-knowledge-graph/internal/extract"
	"github.com/alessiosavi/go-knowledge-graph/internal/graph"
	"github.com/alessiosavi/go-knowledge-graph/internal/graph/graphtest"
	"github.com/alessiosavi/go-knowledge-graph/internal/ingest"
)

func TestDateWindowREST(t *testing.T) {
	f := graphtest.DatedEvidence(t)
	h := apiHandler(t, config.Config{}, f.Store, nil)
	for _, route := range []struct {
		name, path, field, idKey string
		body                     map[string]string
		query                    url.Values
	}{
		{name: "search", path: "/v1/search", field: "hits", idKey: "document_id",
			body: map[string]string{"query": f.Domain, "domain": f.Domain, "kind": "chunk"}},
		{name: "get_entity", path: "/v1/entities/" + f.EntityID, field: "facts", idKey: "doc_id"},
		{name: "traverse", path: "/v1/traverse", field: "edges", idKey: "doc_id",
			body: map[string]string{"entity": f.EntityID}},
		{name: "list_documents", path: "/v1/documents", field: "documents", idKey: "id",
			query: url.Values{"domain": {f.Domain}}},
	} {
		t.Run(route.name, func(t *testing.T) {
			call := func(t *testing.T, since, until string) *httptest.ResponseRecorder {
				t.Helper()
				if route.body != nil {
					body := map[string]string{"since": since, "until": until}
					for k, v := range route.body {
						body[k] = v
					}
					raw, err := json.Marshal(body)
					if err != nil {
						t.Fatal(err)
					}
					return doJSON(t, h, http.MethodPost, route.path, string(raw))
				}
				query := url.Values{"since": {since}, "until": {until}}
				for k, v := range route.query {
					query[k] = v
				}
				return do(t, h, http.MethodGet, route.path+"?"+query.Encode(), nil, nil)
			}
			for _, tc := range []struct {
				name, since, until, wantID, wantError string
			}{
				{name: "since", since: "2026-03-01", wantID: f.NewID},
				{name: "until", until: "2024-03-01", wantID: f.OldID},
				{name: "both", since: "2024-03-01T18:00:00+01:00", until: "2024-03-01", wantID: f.OldID},
				{name: "bad_since", since: "yesterday", wantError: "since must be YYYY-MM-DD"},
				{name: "bad_until", until: "yesterday", wantError: "until must be YYYY-MM-DD"},
				{name: "inverted", since: "2026-03-01", until: "2024-03-01", wantError: "since must be at or before until"},
			} {
				t.Run(tc.name, func(t *testing.T) {
					rr := call(t, tc.since, tc.until)
					if tc.wantError != "" {
						out := decode[errorBody](t, rr)
						if rr.Code != http.StatusBadRequest || out.Error != "invalid_request" || !strings.Contains(out.Detail, tc.wantError) {
							t.Fatalf("status = %d, body = %s, want 400 invalid_request with %q", rr.Code, rr.Body, tc.wantError)
						}
						return
					}
					if rr.Code != http.StatusOK {
						t.Fatalf("status = %d, body = %s", rr.Code, rr.Body)
					}
					out := decode[map[string]json.RawMessage](t, rr)
					var rows []map[string]any
					if err := json.Unmarshal(out[route.field], &rows); err != nil {
						t.Fatal(err)
					}
					if len(rows) != 1 || rows[0][route.idKey] != tc.wantID {
						t.Fatalf("%s = %v, want one row from %s", route.field, rows, tc.wantID)
					}
				})
			}
		})
	}
}

func TestSearchDateWindowClosureREST(t *testing.T) {
	st := graphtest.Store(t)
	ctx := t.Context()
	token := apiToken(t)
	passage := "The " + token + " catalogue passage appears verbatim in both editions."
	sources := [2]string{token + "-a", token + "-b"}
	dates := [2]string{"2024-03-01", "2026-03-01"}
	var ids [2]string
	t.Cleanup(func() {
		if _, err := st.DeleteEmptyDomain(context.Background(), token); err != nil {
			t.Error(err)
		}
	})
	for _, order := range [][2]int{{0, 1}, {1, 0}} {
		t.Run(fmt.Sprint(order), func(t *testing.T) {
			cfg, err := config.Load("")
			if err != nil {
				t.Fatal(err)
			}
			cfg.Ingest.SpoolDir = t.TempDir()
			cfg.Ingest.ChunkDedup = "exact"
			cfg.Ingest.ChunkWords = 100
			mock := &extract.Mock{}
			p := ingest.NewPipeline(cfg, st, mock, dedup.New(), func(_ context.Context, in ingest.Input) (ingest.Loaded, error) {
				return ingest.Loaded{Text: in.Text}, nil
			})
			h := apiHandler(t, config.Config{}, st, p)
			submit := func(source, date string) ingestResponse {
				t.Helper()
				rr := doJSON(t, h, http.MethodPost, "/v1/ingest", fmt.Sprintf(
					`{"source":%q,"text":%q,"document_date":%q}`, source, passage, date))
				if rr.Code != http.StatusAccepted && rr.Code != http.StatusOK {
					t.Fatalf("ingest status = %d, body = %s", rr.Code, rr.Body)
				}
				return decode[ingestResponse](t, rr)
			}
			for i, at := range order {
				doc := submit(sources[at], dates[i])
				id := doc.DocumentID
				t.Cleanup(func() {
					if err := st.DeleteDocument(context.Background(), id); err != nil {
						t.Error(err)
					}
				})
				if doc.Noop || (ids[at] != "" && id != ids[at]) {
					t.Fatalf("ingest = %+v, want fresh submission with stable id %q", doc, ids[at])
				}
				ids[at] = id
				if err := st.AssignDomain(ctx, graph.Assignment{DocID: id, Domain: token}); err != nil {
					t.Fatal(err)
				}
				if err := p.Process(ctx, id); err != nil {
					t.Fatal(err)
				}
			}
			chunks, err := st.Chunks(ctx, ids[order[1]])
			if err != nil || len(chunks) != 1 || chunks[0].DuplicateOf != ids[order[0]]+"-c0" || mock.Calls() != 1 {
				t.Fatalf("duplicate chunk = %+v, err = %v, extraction calls = %d", chunks, err, mock.Calls())
			}

			search := func(bounds string) []hitDTO {
				t.Helper()
				rr := doJSON(t, h, http.MethodPost, "/v1/search", fmt.Sprintf(
					`{"query":%q,"domain":%q,"kind":"chunk"%s}`, token, token, bounds))
				if rr.Code != http.StatusOK {
					t.Fatalf("search status = %d, body = %s", rr.Code, rr.Body)
				}
				return decode[searchResponse](t, rr).Hits
			}
			check := func(bounds, id, date string) {
				t.Helper()
				hits := search(bounds)
				if len(hits) != 1 || hits[0].DocumentID != id || hits[0].DocumentDate == nil ||
					hits[0].DocumentDate.Format(time.DateOnly) != date {
					t.Fatalf("hits for %q = %+v, want one from %s on %s", bounds, hits, id, date)
				}
			}
			unwindowed := func() []hitDTO {
				t.Helper()
				hits := search("")
				if len(hits) != 2 || hits[0].DocumentID == hits[1].DocumentID ||
					!slices.Contains(ids[:], hits[0].DocumentID) || !slices.Contains(ids[:], hits[1].DocumentID) {
					t.Fatalf("unwindowed hits = %+v, want both documents", hits)
				}
				return hits
			}
			const since, until = `,"since":"2025-01-01"`, `,"until":"2024-12-31"`
			baseline := unwindowed()
			check(until, ids[order[0]], dates[0])
			check(since, ids[order[1]], dates[1])
			for i, at := range order {
				doc := submit(sources[at], dates[1-i])
				if !doc.Noop || doc.DocumentID != ids[at] {
					t.Fatalf("redating ingest = %+v, want no-op on %s", doc, ids[at])
				}
			}
			check(until, ids[order[1]], dates[0])
			check(since, ids[order[0]], dates[1])
			if after := unwindowed(); !slices.EqualFunc(after, baseline, func(a, b hitDTO) bool {
				return a.ID == b.ID && a.Score == b.Score
			}) {
				t.Fatalf("redating changed unwindowed ranking from %v to %v", baseline, after)
			}
		})
	}
}
