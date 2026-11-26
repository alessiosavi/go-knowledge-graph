package ingest

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/alessiosavi/go-knowledge-graph/internal/graph"
)

const (
	retryRequested = "http://catalog.example/guide"
	retryLanding   = "http://catalog.example/splash"
)

func retryProxy(t *testing.T, handler http.HandlerFunc) (*Fetcher, *atomic.Int32) {
	t.Helper()
	var connections atomic.Int32
	proxy := httptest.NewUnstartedServer(handler)
	proxy.Config.ConnState = func(_ net.Conn, state http.ConnState) {
		if state == http.StateNew {
			connections.Add(1)
		}
	}
	proxy.Start()
	t.Cleanup(proxy.Close)
	proxyURL, err := url.Parse(proxy.URL)
	if err != nil {
		t.Fatal(err)
	}
	return &Fetcher{
		Guard:        dialStub{},
		Proxy:        proxyURL,
		FetchRetries: 3,
		waitRetry:    func(ctx context.Context, _ time.Duration) bool { return ctx.Err() == nil },
	}, &connections
}

func wrongPage(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == "/guide" {
		http.Redirect(w, r, retryLanding, http.StatusFound)
		return
	}
	fmt.Fprint(w, "wrong page")
}

func TestFetchProxyRejectsWrongPageWithoutRetries(t *testing.T) {
	f, connections := retryProxy(t, wrongPage)
	f.FetchRetries = 0
	_, err := f.Fetch(t.Context(), retryRequested)
	if err == nil || !strings.Contains(err.Error(), "landed on "+retryLanding) || !strings.Contains(err.Error(), "after 1 attempt") || connections.Load() != 1 {
		t.Fatalf("Fetch error = %v after %d connections, want a landing mismatch after one attempt", err, connections.Load())
	}
}

func TestSubmitURLThroughRetryProxy(t *testing.T) {
	t.Run("recovers on a fresh connection", func(t *testing.T) {
		const landing = "http://www.catalog.example/guide/"
		const wantedText = "The requested catalogue page."
		var requests atomic.Int32
		f, connections := retryProxy(t, func(w http.ResponseWriter, r *http.Request) {
			switch r.URL.Path {
			case "/guide":
				if requests.Add(1) <= 2 {
					wrongPage(w, r)
				} else {
					http.Redirect(w, r, landing, http.StatusFound)
				}
			case "/guide/":
				fmt.Fprint(w, wantedText)
			default:
				wrongPage(w, r)
			}
		})
		var delays []time.Duration
		f.waitRetry = func(_ context.Context, d time.Duration) bool {
			delays = append(delays, d)
			return true
		}
		p, st := spoolPipeline(t)
		p.load = DefaultLoader(f, nil)
		st.beforeUpsert = func(in graph.DocumentInput) {
			if in.FinalURL != retryRequested || in.Hash != sha256Hex(wantedText) {
				t.Errorf("admitted URL = %q, hash = %s, want submitted URL and requested page content", in.FinalURL, in.Hash)
			}
		}
		res, err := p.Submit(t.Context(), Input{URL: retryRequested})
		if err != nil || res.DocumentID == "" || connections.Load() != 3 || requests.Load() != 3 {
			t.Fatalf("Submit = %+v, %v, connections = %d, requests = %d", res, err, connections.Load(), requests.Load())
		}
		if want := []time.Duration{time.Second, 2 * time.Second}; !slices.Equal(delays, want) {
			t.Errorf("retry delays = %v, want %v", delays, want)
		}
	})

	t.Run("exhaustion rejects the submission", func(t *testing.T) {
		f, connections := retryProxy(t, wrongPage)
		p, st := spoolPipeline(t)
		p.load = DefaultLoader(f, nil)
		_, err := p.Submit(t.Context(), Input{URL: retryRequested})
		want := "ingest: fetch " + retryRequested + ": landed on " + retryLanding +
			" instead of the requested page after 4 attempts through the fetch proxy; submit the landing URL if that page is intended"
		if err == nil || !strings.Contains(err.Error(), want) || connections.Load() != 4 || len(st.docs) != 0 {
			t.Fatalf("Submit error = %v, connections = %d, stored documents = %d, want %q", err, connections.Load(), len(st.docs), want)
		}
	})
}

func TestFetchProxyAcceptsSamePageRedirect(t *testing.T) {
	const landing = "http://WWW.CATALOG.example:443/guide/?view=new#details"
	f, connections := retryProxy(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/guide" {
			http.Redirect(w, r, landing, http.StatusFound)
			return
		}
		fmt.Fprint(w, "requested page")
	})
	res, err := f.Fetch(t.Context(), "http://Catalog.example:80/guide?view=old")
	if err != nil || connections.Load() != 1 || res.FinalURL != landing {
		t.Fatalf("Fetch = %+v, %v, connections = %d", res, err, connections.Load())
	}
}

func TestSameFetchPage(t *testing.T) {
	for _, tc := range []struct {
		requested, landing string
		match              bool
	}{
		{"http://catalog.example", "https://catalog.example/", true},
		{"http://catalog.example:8080/page", "http://catalog.example:8081/page", false},
		{"http://catalog.example/page", "http://distributor.catalog.example/page", false},
		{"http://catalog.example/page", "http://www.www.catalog.example/page", false},
		{"http://catalog.example/page/", "http://catalog.example/page//", false},
		{"http://catalog.example/a%2Fb", "http://catalog.example/a/b", false},
		{"http://catalog.example/Page", "http://catalog.example/page", false},
	} {
		requested, err := url.Parse(tc.requested)
		if err != nil {
			t.Fatal(err)
		}
		landing, err := url.Parse(tc.landing)
		if err != nil {
			t.Fatal(err)
		}
		if got := sameFetchPage(requested, landing); got != tc.match {
			t.Errorf("sameFetchPage(%q, %q) = %t, want %t", tc.requested, tc.landing, got, tc.match)
		}
	}
}

func TestFetchProxyRetriesSelectedHTTPStatuses(t *testing.T) {
	for _, tc := range []struct {
		code  int
		retry bool
	}{
		{429, true}, {500, true}, {502, true}, {503, true}, {504, true},
		{404, false}, {407, false}, {501, false},
	} {
		t.Run(fmt.Sprint(tc.code), func(t *testing.T) {
			var requests atomic.Int32
			f, connections := retryProxy(t, func(w http.ResponseWriter, _ *http.Request) {
				if requests.Add(1) == 1 {
					w.WriteHeader(tc.code)
					return
				}
				fmt.Fprint(w, "requested page")
			})
			res, err := f.Fetch(t.Context(), retryRequested)
			if tc.retry {
				if err != nil || string(res.Body) != "requested page" || connections.Load() != 2 {
					t.Fatalf("Fetch = %+v, %v, connections = %d, want success after retry", res, err, connections.Load())
				}
			} else if err == nil || !strings.Contains(err.Error(), fmt.Sprintf("HTTP %d", tc.code)) || connections.Load() != 1 {
				t.Fatalf("Fetch error = %v, connections = %d, want HTTP failure without retry", err, connections.Load())
			}
		})
	}
}

func TestFetchProxyRetriesTransportAndBodyReadErrors(t *testing.T) {
	var requests, dials atomic.Int32
	const user, password = "made-up-user", "made-up-password"
	f, connections := retryProxy(t, func(w http.ResponseWriter, _ *http.Request) {
		if requests.Add(1) == 1 {
			w.Header().Set("Content-Length", "100")
		}
		fmt.Fprint(w, "requested page")
	})
	f.Proxy.User = url.UserPassword(user, password)
	f.Client = &http.Client{Transport: &http.Transport{
		DialContext: func(ctx context.Context, network, address string) (net.Conn, error) {
			if dials.Add(1) == 1 {
				return nil, fmt.Errorf("proxy connect failed for %s:%s", user, password)
			}
			return (&net.Dialer{}).DialContext(ctx, network, address)
		},
	}}
	var logs bytes.Buffer
	prior := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
	t.Cleanup(func() { slog.SetDefault(prior) })
	res, err := f.Fetch(t.Context(), retryRequested)
	if err != nil || string(res.Body) != "requested page" || dials.Load() != 3 || connections.Load() != 2 {
		t.Fatalf("Fetch = %+v, %v, dials = %d, connections = %d", res, err, dials.Load(), connections.Load())
	}
	logText := logs.String()
	if strings.Count(logText, "level=WARN") != 2 || strings.Count(logText, "url="+retryRequested) != 2 ||
		!strings.Contains(logText, "attempt=1") || !strings.Contains(logText, "attempt=2") ||
		!strings.Contains(logText, "credentials omitted") || !strings.Contains(logText, "read body: unexpected EOF") ||
		strings.Contains(logText, user) || strings.Contains(logText, password) {
		t.Fatalf("unexpected retry logs: %s", logText)
	}
}

func TestFetchProxyDoesNotRetryGuardRejections(t *testing.T) {
	f, connections := retryProxy(t, func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "http://10.0.0.1/", http.StatusFound)
	})
	f.Guard = nil
	if _, err := f.Fetch(t.Context(), "http://10.0.0.1/"); err == nil || !strings.Contains(err.Error(), "blocked address") || connections.Load() != 0 {
		t.Fatalf("blocked initial URL: error = %v, connections = %d", err, connections.Load())
	}
	if _, err := f.Fetch(t.Context(), "http://8.8.8.8/"); err == nil || !strings.Contains(err.Error(), "blocked dial target") || !strings.Contains(err.Error(), "after 1 attempt") || connections.Load() != 0 {
		t.Fatalf("blocked proxy dial: error = %v, connections = %d", err, connections.Load())
	}
	f.Guard = guardRejectingPrivateRedirect{}
	if _, err := f.Fetch(t.Context(), retryRequested); err == nil || !strings.Contains(err.Error(), "redirect blocked") || connections.Load() != 1 {
		t.Fatalf("blocked redirect: error = %v, connections = %d", err, connections.Load())
	}
}

type guardRejectingPrivateRedirect struct{}

func (guardRejectingPrivateRedirect) CheckURL(_ context.Context, u *url.URL) error {
	if u.Hostname() == "10.0.0.1" {
		return errors.New("blocked address")
	}
	return nil
}

func TestFetchProxyDoesNotRetryRedirectLimitOrBodyCap(t *testing.T) {
	for _, tc := range []struct {
		name  string
		serve http.HandlerFunc
	}{
		{"too many redirects", func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, retryRequested, http.StatusFound)
		}},
		{"body exceeds", func(w http.ResponseWriter, _ *http.Request) { fmt.Fprint(w, "large response") }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f, connections := retryProxy(t, tc.serve)
			f.MaxRedirects, f.MaxBytes = 1, 1
			if _, err := f.Fetch(t.Context(), retryRequested); err == nil || !strings.Contains(err.Error(), tc.name) || connections.Load() != 1 {
				t.Fatalf("Fetch error = %v, connections = %d, want %s without retry", err, connections.Load(), tc.name)
			}
		})
	}
}

func TestFetchProxyBackoffCap(t *testing.T) {
	f, connections := retryProxy(t, wrongPage)
	f.FetchRetries = 5
	var delays []time.Duration
	f.waitRetry = func(_ context.Context, d time.Duration) bool {
		delays = append(delays, d)
		return true
	}
	want := []time.Duration{time.Second, 2 * time.Second, 4 * time.Second, 8 * time.Second, 8 * time.Second}
	if _, err := f.Fetch(t.Context(), retryRequested); err == nil || !slices.Equal(delays, want) || connections.Load() != 6 {
		t.Fatalf("Fetch error = %v, retry delays = %v, connections = %d, want %v and six connections", err, delays, connections.Load(), want)
	}
}

func TestFetchProxyStopsWhenCallerCancelsBackoff(t *testing.T) {
	f, connections := retryProxy(t, wrongPage)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	var waited bool
	f.waitRetry = func(ctx context.Context, delay time.Duration) bool {
		waited = true
		cancel()
		return waitForFetchRetry(ctx, delay)
	}
	if _, err := f.Fetch(ctx, retryRequested); err == nil || !strings.Contains(err.Error(), "landed on "+retryLanding) || !waited || connections.Load() != 1 {
		t.Fatalf("Fetch error = %v, entered backoff = %t, connections = %d", err, waited, connections.Load())
	}
}
