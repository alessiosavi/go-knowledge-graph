package judge

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/alessiosavi/go-knowledge-graph/internal/config"
	"github.com/alessiosavi/go-knowledge-graph/internal/ingest"
)

func TestVerifyClassifiesProviderRequestFailures(t *testing.T) {
	tlsServer := httptest.NewTLSServer(http.NotFoundHandler())
	t.Cleanup(tlsServer.Close)
	hangingServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		<-r.Context().Done()
	}))
	t.Cleanup(hangingServer.Close)

	const query = "/v1?token=judge-io-fixture"
	for _, tc := range []struct {
		name, baseURL, want string
		timeout             time.Duration
	}{
		{"DNS", "http://judge-lookup-fixture.invalid.", "host lookup failed", 0},
		{"TLS certificate", tlsServer.URL, "TLS handshake failed", 0},
		{"client timeout", hangingServer.URL, "request timed out", 200 * time.Millisecond},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := config.Config{}
			cfg.Judge.BaseURL = tc.baseURL + query
			j := NewClient(cfg)
			transport := &http.Transport{} // Bypass environment proxies for the .invalid lookup.
			t.Cleanup(transport.CloseIdleConnections)
			j.client.Transport = transport
			if tc.timeout != 0 {
				j.client.Timeout = tc.timeout
			}
			j.SetBackoff(nil)

			_, err := j.VerifyFact(t.Context(), ingest.FactInput{})
			if want := "judge: retries exhausted: judge: provider request failed: " + tc.want; err == nil || !strings.HasPrefix(err.Error(), want) {
				t.Fatalf("VerifyFact error = %v, want diagnostic beginning %q", err, want)
			}
			if strings.Contains(err.Error(), "judge-io-fixture") || strings.Contains(err.Error(), tc.baseURL) {
				t.Errorf("request error exposed a URL or credential: %v", err)
			}
			if tc.timeout != 0 && !errors.Is(err, context.DeadlineExceeded) {
				t.Errorf("VerifyFact error = %v, want context.DeadlineExceeded", err)
			}
		})
	}
}
