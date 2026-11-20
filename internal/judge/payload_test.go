package judge_test

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"net/http"
	"testing"

	"github.com/alessiosavi/go-knowledge-graph/internal/graph"
	"github.com/alessiosavi/go-knowledge-graph/internal/ingest"
	"github.com/alessiosavi/go-knowledge-graph/internal/judge"
)

type payloadVerdicts struct {
	memoryVerdicts
	reads int
}

func (s *payloadVerdicts) JudgeVerdict(ctx context.Context, key string) (graph.JudgeVerdict, bool, error) {
	s.reads++
	return s.memoryVerdicts.JudgeVerdict(ctx, key)
}

func TestVerifyPayloadWireAndCache(t *testing.T) {
	bodies := make(chan string, 3)
	client := newTestJudge(t, func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Error(err)
		}
		bodies <- string(body)
		_, _ = io.WriteString(w, chatBody(`{"verdict":"confirmed"}`))
	})
	in := ingest.FactInput{Head: "Acme <星>", Rel: "is used by", Tail: "Zorbo", Evidence: "Acme & Zorbo.", Confidence: 0.75}
	p := ingest.CanonicalJudgeInput(in)
	legacyStore := &memoryVerdicts{values: make(map[string]graph.JudgeVerdict)}
	replayStore := &memoryVerdicts{values: make(map[string]graph.JudgeVerdict)}
	replay := judge.NewCached(client, replayStore, client.Model())
	if _, err := judge.NewCached(client, legacyStore, client.Model()).VerifyFact(t.Context(), in); err != nil {
		t.Fatal(err)
	}
	payload := ingest.JudgePayload{Bytes: p, InputHash: fmt.Sprintf("%x", sha256.Sum256(p))}
	for range 2 {
		if _, err := replay.VerifyPayload(t.Context(), payload, "doc"); err != nil {
			t.Fatal(err)
		}
	}
	if len(bodies) != 2 || <-bodies != <-bodies {
		t.Fatal("expected two byte-identical HTTP bodies and one replay cache hit")
	}
	key := judge.RequestKey(p, client.Model())
	if legacyStore.values[key].Status != "confirmed" || replayStore.values[key].DocID != "doc" || replayStore.puts != 1 {
		t.Fatalf("legacy cache = %+v, replay cache = %+v", legacyStore, replayStore)
	}
}

func TestVerifyPayloadRejectsInvalidBeforeCacheOrProvider(t *testing.T) {
	client := newTestJudge(t, func(w http.ResponseWriter, r *http.Request) {
		t.Error("invalid payload reached the provider")
		_, _ = io.WriteString(w, chatBody(`{"verdict":"confirmed"}`))
	})
	valid := string(ingest.CanonicalJudgeInput(ingest.FactInput{Head: "Acme", Rel: "uses", Tail: "Zorbo"}))
	for _, tc := range []struct {
		name, payload string
		wrongHash     bool
	}{
		{name: "empty-object", payload: `{}`},
		{name: "null-fact", payload: `{"fact":null}`},
		{name: "missing-confidence", payload: `{"doc_context":"","evidence":"","fact":{"head":"Acme","rel":"uses","tail":"Zorbo"}}`},
		{name: "non-canonical", payload: valid + "\n"},
		{name: "hash-mismatch", payload: valid, wrongHash: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := []byte(tc.payload)
			hash := fmt.Sprintf("%x", sha256.Sum256(p))
			if tc.wrongHash {
				hash = fmt.Sprintf("%x", sha256.Sum256([]byte("another payload")))
			}
			key := judge.RequestKey(p, client.Model())
			st := &payloadVerdicts{memoryVerdicts: memoryVerdicts{
				values: map[string]graph.JudgeVerdict{key: {Status: "confirmed"}},
			}}
			payload := ingest.JudgePayload{Bytes: p, InputHash: hash}
			if _, err := client.VerifyPayload(t.Context(), payload); !errors.Is(err, ingest.ErrInvalidJudgePayload) {
				t.Errorf("Client.VerifyPayload error = %v", err)
			}
			if _, err := judge.NewCached(client, st, client.Model()).VerifyPayload(t.Context(), payload, "doc"); !errors.Is(err, ingest.ErrInvalidJudgePayload) {
				t.Errorf("Cached.VerifyPayload error = %v", err)
			}
			if st.reads != 0 || st.puts != 0 {
				t.Errorf("invalid payload touched the cache: %+v", st)
			}
		})
	}
}
