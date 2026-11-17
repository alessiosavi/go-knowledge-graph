package judge_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"math"
	"net/http"
	"testing"
	"time"

	"github.com/alessiosavi/go-knowledge-graph/internal/graph"
	"github.com/alessiosavi/go-knowledge-graph/internal/ingest"
	"github.com/alessiosavi/go-knowledge-graph/internal/judge"
)

func TestRequestKey(t *testing.T) {
	in := ingest.FactInput{Head: "Acme-Max", Rel: "uses", Tail: "Zorbo", Evidence: "e", DocContext: "d", Confidence: 0.75}
	b := ingest.CanonicalJudgeInput(in)
	key := judge.RequestKey(b, "test-model")
	want := sha256.Sum256(append(append([]byte{}, b...), []byte("\ntest-model\n1")...))
	if key != hex.EncodeToString(want[:]) || key != judge.RequestKey(b, "test-model") {
		t.Fatalf("unstable key or wrong formula: %s", key)
	}
	for _, field := range []string{"head", "rel", "tail", "evidence", "doc_context", "confidence", "model", "prompt_version"} {
		t.Run(field, func(t *testing.T) {
			changed, model := in, "test-model"
			switch field {
			case "head":
				changed.Head += "x"
			case "rel":
				changed.Rel += "x"
			case "tail":
				changed.Tail += "x"
			case "evidence":
				changed.Evidence += "x"
			case "doc_context":
				changed.DocContext += "x"
			case "confidence":
				changed.Confidence = 0
			case "model":
				model += "x"
			case "prompt_version":
				// PromptVersion is a constant: verify the digest against a different version.
				other := sha256.Sum256(append(append([]byte{}, b...), []byte("\ntest-model\n2")...))
				if key == hex.EncodeToString(other[:]) {
					t.Fatal("prompt version did not affect key")
				}
				return
			}
			if key == judge.RequestKey(ingest.CanonicalJudgeInput(changed), model) {
				t.Fatal("input change did not affect key")
			}
		})
	}
}

func TestVerifyFactCanonicalPayload(t *testing.T) {
	in := ingest.FactInput{Head: "Acme-Max \"星\"", Rel: "uses", Tail: "Zorbo", Evidence: "Unicode è <>&", DocContext: "Quillon Ltd", Confidence: 0}
	var captured string
	j := newTestJudge(t, func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Messages []struct{ Role, Content string }
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
			return
		}
		if len(body.Messages) != 2 || body.Messages[1].Role != "user" {
			t.Errorf("messages = %+v", body.Messages)
			return
		}
		captured = body.Messages[1].Content
		// The old map-based payload had these exact keys and values.
		var got any
		if err := json.Unmarshal([]byte(captured), &got); err != nil {
			t.Error(err)
		}
		want := map[string]any{"fact": map[string]any{"head": in.Head, "rel": in.Rel, "tail": in.Tail, "confidence": in.Confidence}, "evidence": in.Evidence, "doc_context": in.DocContext}
		gotJSON, _ := json.Marshal(got)
		wantJSON, _ := json.Marshal(want)
		if string(gotJSON) != string(wantJSON) {
			t.Errorf("payload values changed: %s", captured)
		}
		_, _ = w.Write([]byte(chatBody(`{"verdict":"confirmed","reason":"supported"}`)))
	})
	if _, err := j.VerifyFact(t.Context(), in); err != nil {
		t.Fatal(err)
	}
	if captured != string(ingest.CanonicalJudgeInput(in)) {
		t.Fatalf("wire payload differs from canonical: %s", captured)
	}
}

type memoryVerdicts struct {
	values            map[string]graph.JudgeVerdict
	readErr, writeErr error
	puts              int
}

func (s *memoryVerdicts) JudgeVerdict(_ context.Context, key string) (graph.JudgeVerdict, bool, error) {
	v, ok := s.values[key]
	return v, ok, s.readErr
}

func (s *memoryVerdicts) PutJudgeVerdict(_ context.Context, v graph.JudgeVerdict) error {
	s.puts++
	if s.writeErr != nil {
		return s.writeErr
	}
	s.values[v.Key] = v
	return nil
}

type countingJudge struct {
	calls   int
	verdict ingest.Verdict
	err     error
}

func (j *countingJudge) VerifyFact(context.Context, ingest.FactInput) (ingest.Verdict, error) {
	j.calls++
	return j.verdict, j.err
}

func TestCachedHit(t *testing.T) {
	in := ingest.FactInput{Head: "Acme-Max"}
	key := judge.RequestKey(ingest.CanonicalJudgeInput(in), "test-model")
	st := &memoryVerdicts{values: map[string]graph.JudgeVerdict{key: {Key: key, Status: "refuted", Reason: "contradicted", Confidence: 0.6}}}
	inner := &countingJudge{}
	v, err := judge.NewCached(inner, st, "test-model").VerifyFact(t.Context(), in)
	if err != nil || v.Status != "refuted" || v.Reason != "contradicted" || v.Confidence != 0.6 || inner.calls != 0 || st.puts != 0 {
		t.Fatalf("hit: verdict=%+v err=%v calls=%d puts=%d", v, err, inner.calls, st.puts)
	}
}

func TestCachedMissPersists(t *testing.T) {
	for _, status := range []string{"confirmed", "unclear", "refuted"} {
		t.Run(status, func(t *testing.T) {
			st := &memoryVerdicts{values: map[string]graph.JudgeVerdict{}}
			inner := &countingJudge{verdict: ingest.Verdict{Status: status, Reason: "evidence", Confidence: 0.8}}
			in := ingest.FactInput{Head: "Acme-Max"}
			before := time.Now()
			v, err := judge.NewCached(inner, st, "test-model").VerifyFact(t.Context(), in)
			if err != nil || v != inner.verdict {
				t.Fatalf("miss = %+v, %v", v, err)
			}
			key := judge.RequestKey(ingest.CanonicalJudgeInput(in), "test-model")
			saved := st.values[key]
			if st.puts != 1 || saved.Key != key || saved.Status != status || saved.Reason != "evidence" || saved.Confidence != 0.8 || saved.Model != "test-model" || saved.PromptVersion != judge.PromptVersion || saved.At.Before(before) || saved.At.After(time.Now()) {
				t.Fatalf("saved = %+v, puts=%d", saved, st.puts)
			}
			// Recreate the wrapper to prove the hit comes from persistence.
			v, err = judge.NewCached(inner, st, "test-model").VerifyFact(t.Context(), in)
			if err != nil || v.Status != status || v.Reason != "evidence" || v.Confidence != 0.8 || inner.calls != 1 || st.puts != 1 {
				t.Fatalf("second call = %+v, %v calls=%d puts=%d", v, err, inner.calls, st.puts)
			}
		})
	}
}

func TestCachedErrorsNotPersisted(t *testing.T) {
	for _, tc := range []struct {
		name        string
		inner       *countingJudge
		input       ingest.FactInput
		calls, puts int
	}{
		{name: "provider", inner: &countingJudge{err: errors.New("offline")}, calls: 1},
		{name: "invalid_status", inner: &countingJudge{verdict: ingest.Verdict{Status: "maybe"}}, calls: 1},
		{name: "nonfinite", inner: &countingJudge{}, input: ingest.FactInput{Confidence: math.NaN()}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st := &memoryVerdicts{values: map[string]graph.JudgeVerdict{}}
			_, err := judge.NewCached(tc.inner, st, "test-model").VerifyFact(t.Context(), tc.input)
			if err == nil || len(st.values) != 0 || tc.inner.calls != tc.calls || st.puts != tc.puts {
				t.Fatalf("err=%v values=%v calls=%d puts=%d", err, st.values, tc.inner.calls, st.puts)
			}
		})
	}
}

type cacheResults map[string]int

func (r cacheResults) JudgeCache(result string) { r[result]++ }

// TestCachedStoreErrorsFallBack pins the cache as an optimization: a failed
// lookup asks the provider as on a miss, and a failed write still returns
// the verdict that was paid for. Both are counted.
func TestCachedStoreErrorsFallBack(t *testing.T) {
	want := ingest.Verdict{Status: "confirmed", Reason: "supported", Confidence: 0.7}
	for _, tc := range []struct {
		name              string
		readErr, writeErr error
		stored            int
		results           cacheResults
	}{
		{name: "read", readErr: errors.New("read failed"), stored: 1, results: cacheResults{"read_error": 1}},
		{name: "write", writeErr: errors.New("write failed"), stored: 0, results: cacheResults{"miss": 1, "write_error": 1}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st := &memoryVerdicts{values: map[string]graph.JudgeVerdict{}, readErr: tc.readErr, writeErr: tc.writeErr}
			inner := &countingJudge{verdict: want}
			got := cacheResults{}
			c := judge.NewCached(inner, st, "test-model")
			c.SetCacheMetrics(got)
			v, err := c.VerifyFact(t.Context(), ingest.FactInput{Head: "Acme-Max"})
			if err != nil || v != want || inner.calls != 1 || st.puts != 1 || len(st.values) != tc.stored {
				t.Fatalf("verdict=%+v err=%v calls=%d puts=%d stored=%d", v, err, inner.calls, st.puts, len(st.values))
			}
			if len(got) != len(tc.results) {
				t.Fatalf("results = %v, want %v", got, tc.results)
			}
			for k, n := range tc.results {
				if got[k] != n {
					t.Fatalf("results = %v, want %v", got, tc.results)
				}
			}
		})
	}
}

// TestCachedReadErrorOnCancelledContext keeps cancellation an error: a
// lookup that failed because the caller gave up must not reach the provider.
func TestCachedReadErrorOnCancelledContext(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	st := &memoryVerdicts{values: map[string]graph.JudgeVerdict{}, readErr: context.Canceled}
	inner := &countingJudge{verdict: ingest.Verdict{Status: "confirmed"}}
	if _, err := judge.NewCached(inner, st, "test-model").VerifyFact(ctx, ingest.FactInput{Head: "Acme-Max"}); !errors.Is(err, context.Canceled) || inner.calls != 0 {
		t.Fatalf("err=%v calls=%d, want context.Canceled and no provider call", err, inner.calls)
	}
}

func TestCachedCountsHitsAndMisses(t *testing.T) {
	st := &memoryVerdicts{values: map[string]graph.JudgeVerdict{}}
	inner := &countingJudge{verdict: ingest.Verdict{Status: "unclear"}}
	got := cacheResults{}
	c := judge.NewCached(inner, st, "test-model")
	c.SetCacheMetrics(got)
	for range 3 {
		if _, err := c.VerifyFact(t.Context(), ingest.FactInput{Head: "Acme-Max"}); err != nil {
			t.Fatal(err)
		}
	}
	if got["miss"] != 1 || got["hit"] != 2 || len(got) != 2 {
		t.Fatalf("results = %v, want 1 miss and 2 hits", got)
	}
}
