package extract

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/alessiosavi/go-knowledge-graph/internal/config"
)

func newTestSidecar(t *testing.T, handler http.HandlerFunc) *Sidecar {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	cfg := config.Config{}
	cfg.Extractor.Sidecar.URL = srv.URL
	cfg.Extractor.Sidecar.Model = "glinerel-314m"
	return NewSidecar(cfg)
}

func TestSidecarTimeout(t *testing.T) {
	for _, seconds := range []int{0, 300, 42} {
		cfg := config.Config{}
		cfg.Extractor.Sidecar.TimeoutS = seconds
		want := seconds
		if want == 0 {
			want = 300
		}
		if got := NewSidecar(cfg).client.Timeout; got != time.Duration(want)*time.Second {
			t.Errorf("timeout %d: got %v, want %ds", seconds, got, want)
		}
	}
}

func TestSidecarParsesContract(t *testing.T) {
	s := newTestSidecar(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/healthz":
			w.Write([]byte(`{"status":"ok","model":"glinerel-314m"}`))
		case "/v1/extract":
			var body struct {
				Chunks []struct {
					ID   string `json:"id"`
					Text string `json:"text"`
				} `json:"chunks"`
				Entities []struct {
					Label     string  `json:"label"`
					Threshold float64 `json:"threshold"`
				} `json:"entities"`
				Relations []struct {
					Label     string  `json:"label"`
					Threshold float64 `json:"threshold"`
				} `json:"relations"`
			}
			decoder := json.NewDecoder(r.Body)
			decoder.DisallowUnknownFields()
			if err := decoder.Decode(&body); err != nil {
				t.Errorf("decode request: %v", err)
			}
			if len(body.Chunks) != 1 || body.Chunks[0].ID != "c1" {
				t.Errorf("chunks = %+v", body.Chunks)
			}
			if len(body.Entities) != 1 || body.Entities[0].Label != "person" || body.Entities[0].Threshold != 0.4 {
				t.Errorf("entities = %+v", body.Entities)
			}
			if len(body.Relations) != 1 || body.Relations[0].Label != "wrote" || body.Relations[0].Threshold != 0.25 {
				t.Errorf("relations = %+v", body.Relations)
			}
			w.Write([]byte(`{"results":[{"chunk_id":"c1","entities":[
				{"label":"person","text":"Ada","start":0,"end":3,"score":0.9}],
				"relations":[]}]}`))
		default:
			http.NotFound(w, r)
		}
	})

	if s.Version() != "sidecar:glinerel-314m" {
		t.Errorf("Version = %q", s.Version())
	}
	res, err := s.Extract(context.Background(), Request{
		Chunks:    []RequestChunk{{ID: "c1", Text: "Ada"}},
		Entities:  []Label{{Name: "person", Threshold: 0.4, Scope: "document"}},
		Relations: []Label{{Name: "wrote", Threshold: 0.25, Scope: "document"}},
	})
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	if len(res) != 1 || res[0].ChunkID != "c1" || len(res[0].Mentions) != 1 || res[0].Mentions[0].Text != "Ada" {
		t.Fatalf("results = %+v", res)
	}
	if len(res[0].Diagnostics) != 0 {
		t.Errorf("diagnostics = %+v, want empty for a legacy response", res[0].Diagnostics)
	}
}

func TestSidecarCarriesAmbiguousRelationDiagnostic(t *testing.T) {
	s := newTestSidecar(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/healthz":
			w.Write([]byte(`{"status":"ok"}`))
		case "/v1/extract":
			w.Write([]byte(`{"results":[{"chunk_id":"c1","entities":[
				{"label":"unrequested","text":"Città","start":0,"end":6},
				{"label":"place","text":"Città","start":0,"end":6},
				{"label":"work","text":"Città","start":0,"end":6},
				{"label":"work","text":"Notes","start":13,"end":18}],
				"relations":[],"diagnostics":[{"type":"ambiguous_relation","label":" CITES WORK ","score":0.7,
				"head_candidates":[0,1,2],"tail_candidates":[3]}]}]}`))
		default:
			http.NotFound(w, r)
		}
	})
	res, err := s.Extract(t.Context(), Request{
		Chunks:    []RequestChunk{{ID: "c1", Text: "Città cites Notes"}},
		Entities:  []Label{{Name: "place"}, {Name: "work"}},
		Relations: []Label{{Name: "cites_work", Text: "cites work"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(res) != 1 || len(res[0].Mentions) != 3 || len(res[0].Candidates) != 0 {
		t.Fatalf("results = %+v, want three validated mentions and no candidates", res)
	}
	want := []Diagnostic{{
		Type: "ambiguous_relation", Label: "cites_work", Score: 0.7,
		HeadCandidates: []int{0, 1}, TailCandidates: []int{2},
	}}
	if got := res[0].Diagnostics; !reflect.DeepEqual(got, want) {
		t.Errorf("diagnostics = %+v, want %+v", got, want)
	}
}

func TestSidecarFiltersAmbiguousRelationDiagnostics(t *testing.T) {
	requested := []Label{{Name: "cites_work", Text: "cites work"}}
	for _, tc := range []struct {
		name      string
		label     string
		head      string
		tail      string
		relations []Label
		wantLabel string
	}{
		{"unrequested label", "related_to", "[0,1]", "[2]", requested, ""},
		{"no relation filter", "related_to", "[0,1]", "[2]", nil, "related_to"},
		{"blank label", " ", "[0,1]", "[2]", nil, ""},
		{"empty head after validation", "Cites Work", "[0]", "[2]", requested, ""},
		{"empty tail after validation", "Cites Work", "[1]", "[0]", requested, ""},
		{"single survivors stay diagnostic", "Cites Work", "[0,1]", "[2]", requested, "cites_work"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := newTestSidecar(t, func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/healthz":
					w.Write([]byte(`{"status":"ok"}`))
				case "/v1/extract":
					fmt.Fprintf(w, `{"results":[{"chunk_id":"c1","entities":[
						{"label":"unrequested","text":"Ada","start":0,"end":3},
						{"label":"person","text":"Ada","start":0,"end":3},
						{"label":"work","text":"Notes","start":10,"end":15}],
						"relations":[],"diagnostics":[{"type":"ambiguous_relation","label":%q,
						"head_candidates":%s,"tail_candidates":%s}]}]}`, tc.label, tc.head, tc.tail)
				default:
					http.NotFound(w, r)
				}
			})
			res, err := s.Extract(t.Context(), Request{
				Chunks:    []RequestChunk{{ID: "c1", Text: "Ada cites Notes"}},
				Entities:  []Label{{Name: "person"}, {Name: "work"}},
				Relations: tc.relations,
			})
			if err != nil {
				t.Fatal(err)
			}
			if len(res) != 1 || len(res[0].Mentions) != 2 || len(res[0].Candidates) != 0 {
				t.Fatalf("results = %+v, want two validated mentions and no candidates", res)
			}
			var want []Diagnostic
			if tc.wantLabel != "" {
				want = []Diagnostic{{
					Type: "ambiguous_relation", Label: tc.wantLabel, Score: 0.5,
					HeadCandidates: []int{0}, TailCandidates: []int{1},
				}}
			}
			if got := res[0].Diagnostics; !reflect.DeepEqual(got, want) {
				t.Errorf("diagnostics = %+v, want %+v", got, want)
			}
		})
	}
}

func TestChunkResultDiagnosticsJSON(t *testing.T) {
	for _, diagnostics := range [][]Diagnostic{nil, {}, {{Type: "ambiguous_relation"}}} {
		data, err := json.Marshal(ChunkResult{Diagnostics: diagnostics})
		if err != nil {
			t.Fatal(err)
		}
		var fields map[string]json.RawMessage
		if err := json.Unmarshal(data, &fields); err != nil {
			t.Fatal(err)
		}
		if _, ok := fields["diagnostics"]; ok != (len(diagnostics) > 0) {
			t.Errorf("diagnostics = %+v, JSON = %s", diagnostics, data)
		}
	}
}

func TestSidecarSendsLabelText(t *testing.T) {
	s := newTestSidecar(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/healthz" {
			w.Write([]byte(`{"status":"ok"}`))
			return
		}
		var body sidecarRequest
		decoder := json.NewDecoder(r.Body)
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&body); err != nil {
			t.Errorf("decode request: %v", err)
		}
		if want := []sidecarLabel{{Label: "data store", Threshold: 0.4}}; !reflect.DeepEqual(body.Entities, want) {
			t.Errorf("entities = %+v, want %+v", body.Entities, want)
		}
		want := []sidecarLabel{{Label: "depends on", Threshold: 0.25}, {Label: "uses", Threshold: 0.3}}
		if !reflect.DeepEqual(body.Relations, want) {
			t.Errorf("relations = %+v, want %+v", body.Relations, want)
		}
		w.Write([]byte(`{"results":[]}`))
	})
	_, err := s.Extract(context.Background(), Request{
		Chunks:    []RequestChunk{{ID: "c1", Text: "Cache uses Store"}},
		Entities:  []Label{{Name: "data_store", Text: "data store", Threshold: 0.4, Scope: "document"}},
		Relations: []Label{{Name: "depends_on", Text: "depends on", Threshold: 0.25}, {Name: "uses", Threshold: 0.3}},
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestSidecarMapsAnswersBackToName(t *testing.T) {
	for _, label := range []string{"depends on", "Depends On", "depends_on", "  DEPENDS ON  ", " DEPENDS_ON ", " Unknown Label "} {
		t.Run(label, func(t *testing.T) {
			s := newTestSidecar(t, func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/healthz" {
					w.Write([]byte(`{"status":"ok"}`))
					return
				}
				score := 0.9
				json.NewEncoder(w).Encode(wireResponse{Results: []wireChunk{{
					ChunkID: "c1",
					Entities: []wireEntity{
						{Label: label, Text: "Cache", Start: 0, End: 5, Score: &score},
						{Label: label, Text: "Store", Start: 11, End: 16, Score: &score},
					},
					Relations: []wireRelation{{Label: label, Head: 0, Tail: 1, Score: &score}},
				}}})
			})
			labels := []Label{{Name: "depends_on", Text: "depends on"}}
			res, err := s.Extract(context.Background(), Request{
				Chunks: []RequestChunk{{ID: "c1", Text: "Cache uses Store"}}, Entities: labels, Relations: labels,
			})
			if err != nil {
				t.Fatal(err)
			}
			if label == " Unknown Label " {
				// Never requested: dropped (with the relation between the
				// dropped mentions), never stored as a new type.
				if len(res) != 1 || len(res[0].Mentions) != 0 || len(res[0].Candidates) != 0 {
					t.Fatalf("results = %+v, want unknown labels dropped", res)
				}
				return
			}
			if len(res) != 1 || len(res[0].Mentions) != 2 || len(res[0].Candidates) != 1 {
				t.Fatalf("results = %+v, want two mentions and one candidate", res)
			}
			want := "depends_on"
			for _, m := range res[0].Mentions {
				if m.Label != want {
					t.Errorf("mention label = %q, want %q", m.Label, want)
				}
			}
			if got := res[0].Candidates[0].Label; got != want {
				t.Errorf("candidate label = %q, want %q", got, want)
			}
		})
	}
}

func TestSidecarHealthGating(t *testing.T) {
	var posts atomic.Int32
	s := newTestSidecar(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/healthz" {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		posts.Add(1)
	})
	_, err := s.Extract(context.Background(), Request{Chunks: []RequestChunk{{ID: "c1", Text: "Ada"}}})
	if err == nil {
		t.Fatal("unhealthy sidecar: want error")
	}
	if posts.Load() != 0 {
		t.Errorf("extraction attempted against an unhealthy sidecar: %d posts", posts.Load())
	}
}

func TestSidecarConnectionRefusedIsUnhealthy(t *testing.T) {
	cfg := config.Config{}
	cfg.Extractor.Sidecar.URL = "http://127.0.0.1:1"
	cfg.Extractor.Sidecar.Model = "glinerel-314m"
	s := NewSidecar(cfg)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if _, err := s.Extract(ctx, Request{Chunks: []RequestChunk{{ID: "c1", Text: "Ada"}}}); err == nil {
		t.Fatal("unreachable sidecar: want error")
	}
}

func TestSidecarSynthesizesMissingResults(t *testing.T) {
	s := newTestSidecar(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/healthz" {
			w.Write([]byte(`{"status":"ok"}`))
			return
		}
		w.Write([]byte(`{"results":[]}`))
	})
	res, err := s.Extract(context.Background(), Request{Chunks: []RequestChunk{{ID: "c1", Text: "Ada"}, {ID: "c2", Text: "Bea"}}})
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	if len(res) != 2 || res[0].ChunkID != "c1" || res[1].ChunkID != "c2" {
		t.Errorf("results = %+v, want one (empty) entry per submitted chunk", res)
	}
}

func TestSidecarSendsSnakeCaseChunkKeys(t *testing.T) {
	s := newTestSidecar(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/healthz" {
			w.Write([]byte(`{"status":"ok"}`))
			return
		}
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatalf("decode: %v", err)
		}
		chunks, ok := body["chunks"].([]any)
		if !ok || len(chunks) != 1 {
			t.Fatalf("chunks = %v", body["chunks"])
		}
		chunk := chunks[0].(map[string]any)
		if _, ok := chunk["id"]; !ok {
			t.Errorf("chunk keys = %v, want lowercase wire keys (id/text)", chunk)
		}
		if _, ok := chunk["text"]; !ok {
			t.Errorf("chunk keys = %v, want lowercase wire keys (id/text)", chunk)
		}
		if _, ok := chunk["ID"]; ok {
			t.Errorf("chunk keys = %v, capitalized keys break the Python contract", chunk)
		}
		w.Write([]byte(`{"results":[]}`))
	})
	if _, err := s.Extract(context.Background(), Request{Chunks: []RequestChunk{{ID: "c1", Text: "Ada"}}}); err != nil {
		t.Fatalf("Extract: %v", err)
	}
}

func TestSidecarSkipsEmptyRequestsEntirely(t *testing.T) {
	var hits atomic.Int32
	s := newTestSidecar(t, func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.Write([]byte(`{"status":"ok"}`))
	})
	res, err := s.Extract(context.Background(), Request{})
	if err != nil {
		t.Fatalf("Extract(empty): %v", err)
	}
	if len(res) != 0 {
		t.Errorf("results = %+v, want none", res)
	}
	if hits.Load() != 0 {
		t.Errorf("HTTP hits = %d, want 0 for an empty request", hits.Load())
	}
}

func TestSidecarRejectsOversizedResponse(t *testing.T) {
	s := newTestSidecar(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/healthz" {
			w.Write([]byte(`{"status":"ok"}`))
			return
		}
		big := make([]byte, (32<<20)+1024)
		for i := range big {
			big[i] = 'x'
		}
		w.Write(big)
	})
	_, err := s.Extract(context.Background(), Request{Chunks: []RequestChunk{{ID: "c1", Text: "Ada"}}})
	if err == nil || !strings.Contains(err.Error(), "exceeds") {
		t.Fatalf("oversized sidecar response error = %v, want an explicit cap error", err)
	}
}

func TestSidecarBatchesBy8Chunks(t *testing.T) {
	var sizes []int
	s := newTestSidecar(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/healthz" {
			w.Write([]byte(`{"status":"ok"}`))
			return
		}
		var body struct {
			Chunks []struct {
				ID   string `json:"id"`
				Text string `json:"text"`
			} `json:"chunks"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatalf("decode: %v", err)
		}
		sizes = append(sizes, len(body.Chunks))
		w.Write([]byte(`{"results":[]}`))
	})
	chunks := make([]RequestChunk, 17)
	for i := range chunks {
		chunks[i] = RequestChunk{ID: string(rune('a' + i)), Text: "text"}
	}
	res, err := s.Extract(context.Background(), Request{Chunks: chunks})
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	if len(sizes) != 3 || sizes[0] != 8 || sizes[1] != 8 || sizes[2] != 1 {
		t.Errorf("batch sizes = %v, want [8 8 1]", sizes)
	}
	if len(res) != 17 {
		t.Errorf("results = %d, want 17", len(res))
	}
}

func TestSidecarExtractReplayRetriesInRequestOrder(t *testing.T) {
	var mu sync.Mutex
	var batches [][]string
	s := newTestSidecar(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/v1/extract" {
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
			http.NotFound(w, r)
			return
		}
		var req sidecarRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Errorf("decode request: %v", err)
			http.Error(w, "invalid request", http.StatusBadRequest)
			return
		}
		var ids []string
		for _, chunk := range req.Chunks {
			ids = append(ids, chunk.ID)
		}
		mu.Lock()
		batches = append(batches, ids)
		mu.Unlock()
		if slices.Contains(ids, "c2") {
			http.Error(w, "synthetic c2 failure", http.StatusServiceUnavailable)
			return
		}
		var results []map[string]any
		for _, chunk := range slices.Backward(req.Chunks) {
			results = append(results, map[string]any{
				"chunk_id": chunk.ID,
				"entities": []map[string]any{{"label": "kind", "text": chunk.Text, "start": 0, "end": len(chunk.Text), "score": .9}},
			})
		}
		if err := json.NewEncoder(w).Encode(map[string]any{"results": results}); err != nil {
			t.Errorf("encode response: %v", err)
		}
	})
	req := Request{Entities: []Label{{Name: "kind", Threshold: .5}}}
	for i := range 11 {
		req.Chunks = append(req.Chunks, RequestChunk{ID: fmt.Sprintf("c%d", i), Text: fmt.Sprintf("Zorvax%d", i)})
	}
	results, err := s.ExtractReplay(t.Context(), req)
	if err != nil || len(results) != len(req.Chunks) {
		t.Fatalf("ExtractReplay returned %d results, error %v", len(results), err)
	}
	for i, result := range results {
		chunk := req.Chunks[i]
		if result.ChunkID != chunk.ID {
			t.Errorf("result %d has chunk ID %q, want %q", i, result.ChunkID, chunk.ID)
		}
		if chunk.ID == "c2" {
			if result.Err == nil || !strings.Contains(result.Err.Error(), "HTTP 503: synthetic c2 failure") || len(result.Mentions) != 0 {
				t.Errorf("failed result = %+v", result)
			}
		} else if result.Err != nil || len(result.Mentions) != 1 || result.Mentions[0].Text != chunk.Text {
			t.Errorf("result %s = %+v, want mention %q", chunk.ID, result, chunk.Text)
		}
	}
	want := [][]string{{"c0", "c1", "c2", "c3", "c4", "c5", "c6", "c7"},
		{"c0"}, {"c1"}, {"c2"}, {"c3"}, {"c4"}, {"c5"}, {"c6"}, {"c7"}, {"c8", "c9", "c10"}}
	mu.Lock()
	defer mu.Unlock()
	if !reflect.DeepEqual(batches, want) {
		t.Errorf("batches = %v, want %v", batches, want)
	}
}

func TestSidecarHealthFailureSurfacesBody(t *testing.T) {
	s := newTestSidecar(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
		w.Write([]byte(`{"detail":"model load failed: weights missing"}`))
	})
	_, err := s.Extract(context.Background(), Request{Chunks: []RequestChunk{{ID: "c1", Text: "Ada"}}})
	if err == nil || !strings.Contains(err.Error(), "weights missing") {
		t.Fatalf("health error = %v, want the service's diagnostic body", err)
	}
}

func TestSidecarServerErrorPropagates(t *testing.T) {
	s := newTestSidecar(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/healthz" {
			w.Write([]byte(`{"status":"ok"}`))
			return
		}
		w.WriteHeader(http.StatusInternalServerError)
		w.Write([]byte(`{"detail":"model exploded"}`))
	})
	_, err := s.Extract(context.Background(), Request{Chunks: []RequestChunk{{ID: "c1", Text: "Ada"}}})
	if err == nil {
		t.Fatal("sidecar 500: want error")
	}
}
