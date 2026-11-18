package judge_test

import (
	"crypto/sha256"
	"fmt"
	"io"
	"net/http"
	"testing"

	"github.com/alessiosavi/go-knowledge-graph/internal/graph"
	"github.com/alessiosavi/go-knowledge-graph/internal/ingest"
	"github.com/alessiosavi/go-knowledge-graph/internal/judge"
)

func TestVerifyFactV1WireGolden(t *testing.T) {
	in := ingest.FactInput{
		Head: "Acme \"星\"", Rel: "uses", Tail: "Zorbo", Evidence: "è <>&",
		DocContext: "Quillon Ltd", Confidence: 0.75, DocID: "doc-zorbo",
	}
	j := newTestJudge(t, func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Error(err)
			return
		}
		const want = "6738f4e8778a42b4a550866a9c2ca62196fb350d563a9913354b38e960278a81"
		if got := fmt.Sprintf("%x", sha256.Sum256(body)); got != want {
			t.Errorf("v1 HTTP body hash = %s, want %s (body %s)", got, want, body)
		}
		_, _ = w.Write([]byte(chatBody(`{"verdict":"confirmed","reason":"supported"}`)))
	})
	st := &memoryVerdicts{values: map[string]graph.JudgeVerdict{}}
	if _, err := judge.NewCached(j, st, j.Model()).VerifyFact(t.Context(), in); err != nil {
		t.Fatal(err)
	}
	const wantKey = "7cb4e9d19bc6e30bf9fa74530f3296148f523537dea1220a77d6b273db935949"
	if saved, ok := st.values[wantKey]; !ok || saved.Key != wantKey || saved.PromptVersion != "1" {
		t.Errorf("v1 cache entries = %+v, want key %s and stored version 1", st.values, wantKey)
	}
}
