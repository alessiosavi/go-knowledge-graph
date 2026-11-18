package judge

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/alessiosavi/go-knowledge-graph/internal/config"
	"github.com/alessiosavi/go-knowledge-graph/internal/ingest"
)

type providerFixtureError string

func (err providerFixtureError) Error() string { return string(err) }

type providerOtherFixtureError string

func (err providerOtherFixtureError) Error() string { return string(err) }

func TestProviderIOError(t *testing.T) {
	const message = "judge: provider request failed"
	const secret = "judge-io-fixture"
	wrap := func(err error) error {
		return &url.Error{Op: "Post", URL: "https://provider.invalid/" + secret, Err: err}
	}
	_, invalidURL := url.Parse("https://provider.invalid:bad/" + secret)
	if invalidURL == nil {
		t.Fatal("invalid port fixture parsed successfully")
	}
	for _, tc := range []struct {
		name string
		err  error
		want string
	}{
		{"DNS", wrap(&net.OpError{Op: "dial", Net: "tcp", Err: &net.DNSError{Err: "no such host", Name: secret}}), "host lookup failed"},
		{"EOF", io.EOF, "connection closed by provider"},
		{"unexpected EOF", wrap(io.ErrUnexpectedEOF), "connection closed by provider"},
		{"connection reset", wrap(syscall.ECONNRESET), "connection reset"},
		{"HTTP response to HTTPS", wrap(http.ErrSchemeMismatch), "provider returned HTTP to an HTTPS client"},
		{"nested URL parse failure", wrap(invalidURL), "provider URL or redirect target is invalid"},
		{"unknown cause", wrap(fmt.Errorf("outer %s: %w", secret, providerFixtureError(secret))), "judge.providerFixtureError"},
		{"joined causes", wrap(errors.Join(
			providerOtherFixtureError(secret),
			fmt.Errorf("wrapped %s: %w", secret, errors.Join(providerFixtureError(secret), providerFixtureError(secret+"-second"))),
		)), "judge.providerFixtureError, judge.providerOtherFixtureError"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := providerIOError(message, tc.err).Error()
			if strings.Contains(got, secret) {
				t.Fatal("diagnostic exposed a credential")
			}
			if want := message + ": " + tc.want; got != want {
				t.Errorf("diagnostic = %q, want %q", got, want)
			}
		})
	}
}

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
		{"TLS certificate", tlsServer.URL, "TLS handshake failed", 0},
		{"client timeout", hangingServer.URL, "request timed out", 200 * time.Millisecond},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := config.Config{}
			cfg.Judge.BaseURL = tc.baseURL + query
			j := NewClient(cfg)
			transport := &http.Transport{}
			t.Cleanup(transport.CloseIdleConnections)
			j.client.Transport = transport
			if tc.timeout != 0 {
				j.client.Timeout = tc.timeout
			}
			j.SetBackoff(nil)

			_, err := j.VerifyFact(t.Context(), ingest.FactInput{})
			if err == nil {
				t.Fatal("VerifyFact succeeded, want request error")
			}
			got := err.Error()
			if strings.Contains(got, "judge-io-fixture") || strings.Contains(got, tc.baseURL) {
				t.Fatal("request error exposed a URL or credential")
			}
			if want := "judge: retries exhausted: judge: provider request failed: " + tc.want; !strings.HasPrefix(got, want) {
				t.Errorf("diagnostic = %q, want prefix %q", got, want)
			}
			if tc.timeout != 0 && !errors.Is(err, context.DeadlineExceeded) {
				t.Error("VerifyFact error did not wrap context.DeadlineExceeded")
			}
		})
	}
}
