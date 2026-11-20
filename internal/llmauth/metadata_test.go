package llmauth

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"cloud.google.com/go/compute/metadata"
)

func fakeMetadata(t *testing.T, handler http.HandlerFunc) *metadataProvider {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/computeMetadata/v1/"+tokenPath || r.Header.Get("Metadata-Flavor") != "Google" {
			t.Errorf("unexpected metadata request %s %s", r.Method, r.URL)
		}
		w.Header().Set("Metadata-Flavor", "Google")
		handler(w, r)
	}))
	t.Cleanup(srv.Close)
	t.Setenv("GCE_METADATA_HOST", strings.TrimPrefix(srv.URL, "http://"))
	return newMetadataProvider(metadata.NewClient(srv.Client()))
}

func TestMetadataRefreshAndCancellation(t *testing.T) {
	var calls atomic.Int32
	started, release := make(chan struct{}), make(chan struct{})
	defer func() {
		select {
		case <-release:
		default:
			close(release)
		}
	}()
	p := fakeMetadata(t, func(w http.ResponseWriter, r *http.Request) {
		n := calls.Add(1)
		if n == 1 {
			close(started)
			select {
			case <-r.Context().Done():
				return
			case <-release:
			}
		}
		fmt.Fprintf(w, `{"access_token":"fake-token-%d","expires_in":120,"token_type":"Bearer"}`, n)
	})
	clock := time.Unix(2000, 0)
	var elapsed atomic.Int64
	p.now = func() time.Time { return clock.Add(time.Duration(elapsed.Load())) }
	ctx, stop := context.WithCancel(t.Context())
	defer stop()
	canceled := make(chan error, 1)
	go func() {
		_, _, err := p.Token(ctx)
		canceled <- err
	}()
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("metadata request did not start")
	}
	const concurrent = 8
	results := make(chan error, concurrent)
	for range concurrent {
		go func() {
			token, expiry, err := p.Token(t.Context())
			if err == nil && (token != "fake-token-1" || !expiry.Equal(clock.Add(120*time.Second))) {
				err = fmt.Errorf("wrong credential: token=%q expiry=%v", token, expiry)
			}
			results <- err
		}()
	}
	stop()
	if err := <-canceled; !errors.Is(err, context.Canceled) {
		t.Fatalf("caller cancellation = %v", err)
	}
	close(release)
	for range concurrent {
		if err := <-results; err != nil {
			t.Error(err)
		}
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("concurrent metadata calls = %d, want 1", got)
	}
	elapsed.Store(int64(59 * time.Second))
	if token, _, err := p.Token(t.Context()); err != nil || token != "fake-token-1" {
		t.Fatalf("cached token=%q err=%v", token, err)
	}
	elapsed.Store(int64(60 * time.Second))
	if token, expiry, err := p.Token(t.Context()); err != nil || token != "fake-token-2" || !expiry.Equal(clock.Add(180*time.Second)) {
		t.Fatalf("renewed token=%q expiry=%v err=%v", token, expiry, err)
	}
	if got := calls.Load(); got != 2 {
		t.Fatalf("metadata calls after renewal = %d, want 2", got)
	}
}

func TestMetadataFailureClasses(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
		body   string
		class  string
	}{
		{"missing account", 404, "fake-sensitive-body", PermanentClass},
		{"unauthorized", 401, "fake-sensitive-body", PermanentClass},
		{"denied", 403, "fake-sensitive-body", PermanentClass},
		{"server failure", 500, "fake-sensitive-body", TransientClass},
		{"metadata timeout", 0, "", TransientClass},
		{"malformed JSON", 200, "fake-sensitive-body", TransientClass},
		{"blank token", 200, `{"access_token":"","expires_in":3600}`, TransientClass},
		{"invalid bearer", 200, `{"access_token":"fake-sensitive-body\r\n","expires_in":3600}`, TransientClass},
		{"missing expiry", 200, `{"access_token":"fake-sensitive-body"}`, TransientClass},
		{"duration overflow", 200, `{"access_token":"fake-sensitive-body","expires_in":9223372036854775807}`, TransientClass},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := fakeMetadata(t, func(w http.ResponseWriter, r *http.Request) {
				if tc.status == 0 {
					<-r.Context().Done()
					return
				}
				w.WriteHeader(tc.status)
				fmt.Fprint(w, tc.body)
			})
			if tc.status == 0 || tc.status == 500 {
				p.timeout = 40 * time.Millisecond
			}
			token, expiry, err := p.Token(t.Context())
			if Class(err) != tc.class || token != "" || !expiry.IsZero() || strings.Contains(err.Error(), "fake-sensitive-body") {
				t.Errorf("token=%q expiry=%v err=%v class=%q, want %s", token, expiry, err, Class(err), tc.class)
			}
		})
	}
}

func TestGCEMetadataOverrideIsTestOnly(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Error("metadata transport reached the override host")
	}))
	t.Cleanup(server.Close)
	req := httptest.NewRequest(http.MethodGet, server.URL+"/computeMetadata/v1/"+tokenPath, nil)
	transport := metadataTransport{transport: &http.Transport{}}
	resp, err := transport.RoundTrip(req)
	if resp != nil {
		resp.Body.Close()
	}
	if resp != nil || err == nil || err.Error() != "unexpected metadata endpoint" {
		t.Fatalf("response=%v error=%v, want endpoint rejection", resp, err)
	}
}

// UseFakeGCE installs a loopback metadata server for external test callers.
func UseFakeGCE(t *testing.T, handler http.HandlerFunc) {
	previous := gceProvider
	gceProvider = fakeMetadata(t, handler)
	t.Cleanup(func() { gceProvider = previous })
}
