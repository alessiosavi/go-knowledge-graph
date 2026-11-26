package ingest_test

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/alessiosavi/go-knowledge-graph/internal/ingest"
)

func TestFetchThroughConfiguredProxy(t *testing.T) {
	const body = "Città italiana"
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, body)
	}))
	t.Cleanup(target.Close)
	for _, mode := range []string{"default transport", "caller transport"} {
		t.Run(mode, func(t *testing.T) {
			var hits atomic.Int32
			handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				hits.Add(1)
				if r.Method != http.MethodGet || r.RequestURI != target.URL+"/guide" || !r.URL.IsAbs() {
					t.Errorf("proxy got %s %s, want GET with the absolute target URI", r.Method, r.RequestURI)
				}
				if r.Header.Get("Proxy-Authorization") != "Basic dXNlcjpwYXNz" {
					t.Error("proxy did not receive the configured userinfo")
				}
				resp, err := target.Client().Get(target.URL + "/guide")
				if err != nil {
					t.Errorf("fetch simulated target: %v", err)
					w.WriteHeader(http.StatusBadGateway)
					return
				}
				defer resp.Body.Close()
				if _, err := io.Copy(w, resp.Body); err != nil {
					t.Errorf("forward simulated target body: %v", err)
				}
			})
			proxy := httptest.NewUnstartedServer(handler)
			f := &ingest.Fetcher{Guard: stubDialGuard{}}
			switch mode {
			case "caller transport":
				proxy.Start()
				transport := &http.Transport{}
				f.Client = &http.Client{Transport: transport}
				t.Cleanup(func() {
					if transport.Proxy != nil {
						t.Error("Fetch changed the caller's transport")
					}
				})
			default:
				proxy.Start()
			}
			t.Cleanup(proxy.Close)
			f.Proxy = mustURL(t, proxy.URL)
			f.Proxy.User = url.UserPassword("user", "pass")
			res, err := f.Fetch(t.Context(), target.URL+"/guide")
			if err != nil || string(res.Body) != body || res.FinalURL != target.URL+"/guide" || hits.Load() != 1 {
				t.Fatalf("Fetch returned %q from %q, proxy hits %d, error %v", res.Body, res.FinalURL, hits.Load(), err)
			}
		})
	}
}

func TestFetchProxyGuardChecksTargetsAndDial(t *testing.T) {
	var hits atomic.Int32
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		http.Redirect(w, r, "http://10.0.0.1/blocked", http.StatusFound)
	}))
	t.Cleanup(proxy.Close)
	f := &ingest.Fetcher{Proxy: mustURL(t, proxy.URL)}
	for _, target := range []string{"http://127.0.0.1/", "http://10.0.0.1/"} {
		if _, err := f.Fetch(t.Context(), target); err == nil || !strings.Contains(err.Error(), "blocked address") {
			t.Fatalf("Fetch of blocked target: got %v", err)
		}
	}
	var dials atomic.Int32
	dial := func(context.Context, string, string) (net.Conn, error) {
		dials.Add(1)
		return nil, fmt.Errorf("unexpected dial")
	}
	f.Client = &http.Client{Transport: &http.Transport{DialContext: dial, DialTLSContext: dial}}
	for _, scheme := range []string{"http", "https"} {
		f.Proxy.Scheme = scheme
		// The numeric public target requires no DNS. The transport above
		// prevents any real network access if the dial guard regresses.
		if _, err := f.Fetch(t.Context(), "http://8.8.8.8/"); err == nil || !strings.Contains(err.Error(), "blocked dial target") {
			t.Fatalf("Fetch with blocked %s proxy: got %v", scheme, err)
		}
	}
	if hits.Load() != 0 || dials.Load() != 0 {
		t.Fatalf("blocked requests reached proxy %d times and dialer %d times", hits.Load(), dials.Load())
	}

	f.Proxy.Scheme = "http"
	f.Client = nil
	f.Guard = stubGuard{blocked: map[string]bool{"10.0.0.1": true}}
	if _, err := f.Fetch(t.Context(), proxy.URL+"/start"); err == nil || !strings.Contains(err.Error(), "redirect blocked") || hits.Load() != 1 {
		t.Fatalf("Fetch redirect: proxy hits %d, error %v", hits.Load(), err)
	}
}

func TestFetchWithoutProxyIgnoresProxyEnvironment(t *testing.T) {
	var proxyHits atomic.Int32
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		proxyHits.Add(1)
		w.WriteHeader(http.StatusBadGateway)
	}))
	t.Cleanup(proxy.Close)
	t.Setenv("HTTP_PROXY", proxy.URL)
	t.Setenv("HTTPS_PROXY", proxy.URL)
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, "direct")
	}))
	t.Cleanup(target.Close)
	var proxyCalls atomic.Int32
	transport := &http.Transport{
		Proxy: func(r *http.Request) (*url.URL, error) {
			proxyCalls.Add(1)
			return http.ProxyFromEnvironment(r)
		},
		DialContext: func(ctx context.Context, network, address string) (net.Conn, error) {
			if address == "192.0.2.1:80" {
				address = target.Listener.Addr().String()
			}
			if address != target.Listener.Addr().String() && address != proxy.Listener.Addr().String() {
				return nil, fmt.Errorf("unexpected dial address: %s", address)
			}
			return (&net.Dialer{}).DialContext(ctx, network, address)
		},
	}
	for _, tc := range []struct {
		client *http.Client
		url    string
	}{
		{nil, target.URL},
		{&http.Client{Transport: transport}, "http://192.0.2.1/"},
	} {
		f := &ingest.Fetcher{Guard: stubDialGuard{}, Client: tc.client}
		res, err := f.Fetch(t.Context(), tc.url)
		if err != nil || string(res.Body) != "direct" || proxyHits.Load() != 0 || proxyCalls.Load() != 0 {
			t.Fatalf("Fetch returned %q, proxy hits %d, proxy lookups %d, error %v", res.Body, proxyHits.Load(), proxyCalls.Load(), err)
		}
	}
}

func TestFetchProxyErrorOmitsCredentials(t *testing.T) {
	// CONNECT rejection reason phrases are returned as transport errors by
	// net/http, unlike status messages on a regular GET response.
	var hits atomic.Int32
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		if r.Method != http.MethodConnect {
			t.Errorf("proxy method = %s, want CONNECT", r.Method)
		}
		conn, rw, err := http.NewResponseController(w).Hijack()
		if err != nil {
			t.Errorf("hijack test connection: %v", err)
			return
		}
		defer conn.Close()
		fmt.Fprint(rw, "HTTP/1.1 407 Rejected http://proxy-user:proxy-password@proxy.example\r\nContent-Length: 0\r\n\r\n")
		if err := rw.Flush(); err != nil {
			t.Errorf("send CONNECT rejection: %v", err)
		}
	}))
	t.Cleanup(proxy.Close)
	f := &ingest.Fetcher{Guard: stubDialGuard{}, Proxy: mustURL(t, proxy.URL), FetchRetries: 3}
	f.Proxy.User = url.UserPassword("proxy-user", "proxy-password")
	_, err := f.Fetch(t.Context(), "https://192.0.2.1/")
	if err == nil || !strings.Contains(err.Error(), "HTTP 407") || strings.Contains(err.Error(), "proxy-user") ||
		strings.Contains(err.Error(), "proxy-password") || hits.Load() != 1 {
		t.Fatalf("Fetch error = %v, proxy hits = %d, want HTTP 407 without retry or credentials", err, hits.Load())
	}
}
