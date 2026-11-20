package judge

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"sync/atomic"
	"testing"
	"time"

	"github.com/alessiosavi/go-knowledge-graph/internal/config"
	"github.com/alessiosavi/go-knowledge-graph/internal/ingest"
)

func retryTestClient(t *testing.T, handler http.HandlerFunc) (*Client, *[]time.Duration) {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	var cfg config.Config
	cfg.Judge.BaseURL = srv.URL
	j := NewClient(cfg)
	var waits []time.Duration
	j.after = func(delay time.Duration) <-chan time.Time {
		waits = append(waits, delay)
		done := make(chan time.Time)
		close(done)
		return done
	}
	return j, &waits
}

func TestVerifyRetryPolicyRetryAfter(t *testing.T) {
	now := time.Date(2026, time.November, 20, 8, 0, 0, 250*int(time.Millisecond), time.UTC)
	for _, tc := range []struct {
		name   string
		status int
		header string
		want   time.Duration
	}{
		{"seconds", http.StatusTooManyRequests, "7", 7 * time.Second},
		{"HTTP date", http.StatusServiceUnavailable, now.Add(12 * time.Second).Format(http.TimeFormat), 11750 * time.Millisecond},
		{"zero seconds", http.StatusTooManyRequests, "0", time.Second},
		{"past HTTP date", http.StatusServiceUnavailable, now.Add(-time.Hour).Format(http.TimeFormat), time.Second},
		{"high seconds", http.StatusTooManyRequests, "120", time.Minute},
		{"overflowing seconds", http.StatusTooManyRequests, "999999999999999999999999", time.Minute},
		{"far HTTP date", http.StatusServiceUnavailable, now.Add(time.Hour).Format(http.TimeFormat), time.Minute},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var calls atomic.Int32
			j, waits := retryTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
				if calls.Add(1) == 1 {
					w.Header().Set("Retry-After", tc.header)
					w.WriteHeader(tc.status)
					return
				}
				_, _ = io.WriteString(w, `{"choices":[{"message":{"content":"{\"verdict\":\"confirmed\"}"}}]}`)
			})
			j.now = func() time.Time { return now }
			j.jitter = func(time.Duration) time.Duration {
				t.Fatal("valid Retry-After used jitter")
				return 0
			}
			if verdict, err := j.VerifyFact(t.Context(), ingest.FactInput{}); err != nil || verdict.Status != "confirmed" {
				t.Fatalf("VerifyFact = (%+v, %v), want confirmed verdict", verdict, err)
			}
			if !slices.Equal(*waits, []time.Duration{tc.want}) || calls.Load() != 2 {
				t.Errorf("waits = %v, calls = %d; want [%s], 2", *waits, calls.Load(), tc.want)
			}
		})
	}
}

func TestVerifyRetryPolicyJitterAndRetryLimit(t *testing.T) {
	for _, tc := range []struct {
		name   string
		header string
	}{
		{"absent header", ""},
		{"invalid header", "https://provider.invalid/?key=judge-fixture"},
		// RFC 9110 delta-seconds contain only digits.
		{"negative delta-seconds", "-5"},
		{"plus-signed delta-seconds", "+5"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var calls atomic.Int32
			j, waits := retryTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
				calls.Add(1)
				if tc.header != "" {
					w.Header().Set("Retry-After", tc.header)
				}
				w.WriteHeader(http.StatusTooManyRequests)
			})
			wantBounds := []time.Duration{time.Second, 2 * time.Second, 4 * time.Second, 8 * time.Second, 16 * time.Second, 30 * time.Second}
			wantWaits := []time.Duration{0, time.Second, 4 * time.Second, 3 * time.Second, 0, 30 * time.Second}
			var bounds []time.Duration
			j.jitter = func(bound time.Duration) time.Duration {
				bounds = append(bounds, bound)
				if len(bounds) > len(wantWaits) {
					t.Fatal("jitter called after six retries")
				}
				return wantWaits[len(bounds)-1]
			}
			_, err := j.VerifyFact(t.Context(), ingest.FactInput{})
			if err == nil || err.Error() != "judge: retries exhausted: judge: HTTP 429" {
				t.Errorf("VerifyFact error = %v, want HTTP 429 retry exhaustion", err)
			}
			if !slices.Equal(bounds, wantBounds) || !slices.Equal(*waits, wantWaits) || calls.Load() != 7 {
				t.Errorf("bounds = %v, waits = %v, calls = %d; want %v, %v, 7", bounds, *waits, calls.Load(), wantBounds, wantWaits)
			}
		})
	}
}

func TestNewClientJitterBounds(t *testing.T) {
	var cfg config.Config
	j := NewClient(cfg)
	for _, bound := range []time.Duration{time.Second, 30 * time.Second} {
		for range 256 {
			if delay := j.jitter(bound); delay < 0 || delay > bound {
				t.Fatalf("jitter(%s) = %s, want a delay in [0, %s]", bound, delay, bound)
			}
		}
	}
}

func TestVerifyRetryPolicyWaitLimit(t *testing.T) {
	var calls atomic.Int32
	j, waits := retryTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.Header().Set("Retry-After", "60")
		w.WriteHeader(http.StatusServiceUnavailable)
	})
	gates := 0
	j.SetAttemptGate(func() error { gates++; return nil })
	_, err := j.VerifyFact(t.Context(), ingest.FactInput{})
	if err == nil || err.Error() != "judge: retries exhausted: judge: HTTP 503" {
		t.Errorf("VerifyFact error = %v, want HTTP 503 retry exhaustion", err)
	}
	if !slices.Equal(*waits, []time.Duration{time.Minute, time.Minute, time.Minute}) || calls.Load() != 4 || gates != 4 {
		t.Errorf("waits = %v, calls = %d, gates = %d; want [1m 1m 1m], 4, 4", *waits, calls.Load(), gates)
	}
}

func TestVerifyRetryPolicyBackoffOverride(t *testing.T) {
	for _, backoff := range [][]time.Duration{nil, {0, 5 * time.Second}} {
		var calls atomic.Int32
		j, waits := retryTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
			calls.Add(1)
			w.Header().Set("Retry-After", "60")
			w.WriteHeader(http.StatusTooManyRequests)
		})
		j.SetBackoff(backoff)
		j.jitter = func(time.Duration) time.Duration {
			t.Fatal("explicit backoff used jitter")
			return 0
		}
		_, err := j.VerifyFact(t.Context(), ingest.FactInput{})
		if err == nil || err.Error() != "judge: retries exhausted: judge: HTTP 429" {
			t.Errorf("VerifyFact error = %v, want HTTP 429 retry exhaustion", err)
		}
		if !slices.Equal(*waits, backoff) || calls.Load() != int32(len(backoff)+1) {
			t.Errorf("waits = %v, calls = %d; want %v, %d", *waits, calls.Load(), backoff, len(backoff)+1)
		}
	}
}

func TestVerifyRetryPolicyBadRequest(t *testing.T) {
	var calls atomic.Int32
	j, waits := retryTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.Header().Set("Retry-After", "60")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = io.WriteString(w, "rejected")
	})
	_, err := j.VerifyFact(t.Context(), ingest.FactInput{})
	if err == nil || err.Error() != "judge: HTTP 400: rejected" || calls.Load() != 1 || len(*waits) != 0 {
		t.Fatalf("error = %v, calls = %d, waits = %v; want HTTP 400, 1, no waits", err, calls.Load(), *waits)
	}
}

func TestVerifyRetryPolicyAttemptGate(t *testing.T) {
	var calls atomic.Int32
	j, waits := retryTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.Header().Set("Retry-After", "1")
		w.WriteHeader(http.StatusTooManyRequests)
	})
	stop := errors.New("attempt budget reached")
	gates := 0
	j.SetAttemptGate(func() error {
		if got := int(calls.Load()); got != gates {
			t.Errorf("provider calls before gate %d = %d, want %d", gates+1, got, gates)
		}
		gates++
		if gates == 3 {
			return stop
		}
		return nil
	})
	_, err := j.VerifyFact(t.Context(), ingest.FactInput{})
	if err != stop || calls.Load() != 2 || gates != 3 || len(*waits) != 2 {
		t.Fatalf("error = %v, calls = %d, gates = %d, waits = %v; want gate error, 2, 3, two waits", err, calls.Load(), gates, *waits)
	}
}

func TestVerifyRetryPolicyCancelWait(t *testing.T) {
	var calls atomic.Int32
	j, _ := retryTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.Header().Set("Retry-After", "60")
		w.WriteHeader(http.StatusTooManyRequests)
	})
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	waiting := make(chan struct{})
	timer := make(chan time.Time)
	defer close(timer)
	j.after = func(time.Duration) <-chan time.Time {
		close(waiting)
		return timer
	}
	result := make(chan error, 1)
	go func() {
		_, err := j.VerifyFact(ctx, ingest.FactInput{})
		result <- err
	}()
	timeout := time.After(5 * time.Second)
	select {
	case <-waiting:
		cancel()
	case err := <-result:
		t.Fatalf("VerifyFact returned before waiting: %v", err)
	case <-timeout:
		t.Fatal("VerifyFact did not start waiting")
	}
	select {
	case err := <-result:
		if err != context.Canceled || calls.Load() != 1 {
			t.Errorf("error = %v, calls = %d; want context.Canceled, 1", err, calls.Load())
		}
	case <-timeout:
		t.Fatal("cancel did not interrupt the retry wait")
	}
}
