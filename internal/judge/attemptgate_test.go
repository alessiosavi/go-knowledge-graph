package judge_test

import (
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/alessiosavi/go-knowledge-graph/internal/ingest"
)

func TestAttemptGateCountsRetries(t *testing.T) {
	calls, gates := 0, 0
	j := newTestJudge(t, func(w http.ResponseWriter, _ *http.Request) { calls++; w.WriteHeader(500) })
	j.SetBackoff([]time.Duration{0, 0})
	stop := errors.New("attempt budget reached")
	j.SetAttemptGate(func() error {
		if gates == 2 {
			return stop
		}
		gates++
		return nil
	})
	_, err := j.VerifyFact(t.Context(), ingest.FactInput{Head: "Acme-Max"})
	if !errors.Is(err, stop) || calls != 2 || gates != 2 {
		t.Fatalf("err=%v calls=%d gates=%d", err, calls, gates)
	}
	j.SetAttemptGate(nil)
}
