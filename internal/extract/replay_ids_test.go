package extract

import (
	"encoding/json"
	"net/http"
	"reflect"
	"strings"
	"sync"
	"testing"
)

func TestReplayRejectsInvalidSidecarResultIDs(t *testing.T) {
	req := Request{Chunks: []RequestChunk{{ID: "synthra"}, {ID: "velrix"}}}
	for _, tc := range []struct {
		name, problem string
		batch, single []string
	}{
		{"missing", "missing result", []string{"synthra"}, nil},
		{"duplicate", "duplicate result", []string{"synthra", "velrix", "velrix"}, []string{"velrix", "velrix"}},
		{"unrequested", "unrequested chunk", []string{"synthra", "velrix", "desvyr"}, []string{"velrix", "desvyr"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var mu sync.Mutex
			var calls [][]string
			s := newTestSidecar(t, func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/healthz" {
					_, _ = w.Write([]byte("{}"))
					return
				}
				var request sidecarRequest
				if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
					t.Error(err)
					http.Error(w, "invalid request", http.StatusBadRequest)
					return
				}
				var ids []string
				for _, c := range request.Chunks {
					ids = append(ids, c.ID)
				}
				mu.Lock()
				calls = append(calls, ids)
				mu.Unlock()
				var reply []string
				switch {
				case len(ids) == 2:
					reply = tc.batch
				case ids[0] == "velrix":
					reply = tc.single
				default:
					reply = ids
				}
				results := make([]sidecarChunk, 0, len(reply))
				for _, id := range reply {
					results = append(results, sidecarChunk{wireChunk: wireChunk{ChunkID: id}})
				}
				if err := json.NewEncoder(w).Encode(sidecarResponse{Results: results}); err != nil {
					t.Error(err)
				}
			})
			production, err := s.Extract(t.Context(), req)
			if err != nil || len(production) != 2 || production[0].ChunkID != "synthra" || production[1].ChunkID != "velrix" {
				t.Fatalf("production extraction = %+v, %v", production, err)
			}

			replay, err := s.ExtractReplay(t.Context(), req)
			if err != nil || len(replay) != 2 {
				t.Fatalf("replay extraction = %+v, %v", replay, err)
			}
			if replay[0].ChunkID != "synthra" || replay[0].Err != nil || replay[1].ChunkID != "velrix" ||
				replay[1].Err == nil || !strings.Contains(replay[1].Err.Error(), tc.problem) {
				t.Errorf("replay results = %+v, want velrix error containing %q", replay, tc.problem)
			}
			mu.Lock()
			defer mu.Unlock()
			want := [][]string{{"synthra", "velrix"}, {"synthra", "velrix"}, {"synthra"}, {"velrix"}}
			if !reflect.DeepEqual(calls, want) {
				t.Errorf("requests = %v, want %v", calls, want)
			}
		})
	}
}
