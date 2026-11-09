package ingest_test

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/alessiosavi/go-knowledge-graph/internal/ingest"
)

// stubGuard allows everything except the listed hosts — fetch tests cover
// the wiring; netguard's own tests cover the real blocking rules.
type stubGuard struct {
	blocked map[string]bool
}

func (g stubGuard) CheckURL(_ context.Context, u *url.URL) error {
	if g.blocked[u.Hostname()] {
		return fmt.Errorf("netguard: blocked address %q", u.Hostname())
	}
	return nil
}

func TestFetchHTMLPageAndExtractText(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		fmt.Fprint(w, `<html><head><title>Città guide</title></head><body>
			<article><h1>Città perché</h1>
			<p>La città perché è bella. La città perché è viva. La città perché è nostra.</p>
			<p>La città perché è bella. La città perché è viva. La città perché è nostra.</p>
			<p>La città perché è bella. La città perché è viva. La città perché è nostra.</p>
			</article></body></html>`)
	}))
	t.Cleanup(srv.Close)

	f := &ingest.Fetcher{Guard: stubGuard{}}
	res, err := f.Fetch(context.Background(), srv.URL+"/guide")
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if res.ContentType == "" || !strings.Contains(res.ContentType, "text/html") {
		t.Errorf("ContentType = %q", res.ContentType)
	}
	if res.FinalURL != srv.URL+"/guide" {
		t.Errorf("FinalURL = %q", res.FinalURL)
	}
	text, err := ingest.HTMLToText(res.Body, mustURL(t, res.FinalURL))
	if err != nil {
		t.Fatalf("HTMLToText: %v", err)
	}
	if !strings.Contains(text, "Città perché") {
		t.Errorf("extracted text = %q, want the article text", text)
	}
	if strings.Contains(text, "<p>") || strings.Contains(text, "<article>") {
		t.Errorf("extracted text still contains tags: %q", text)
	}
}

func TestFetchHTMLToTextFallbackStripsTags(t *testing.T) {
	// Too small for readability's article detection: the strip fallback runs.
	text, err := ingest.HTMLToText([]byte(`<div>Hello <b>Città</b> world &amp; friends</div>`), mustURL(t, "https://example.com/"))
	if err != nil {
		t.Fatalf("HTMLToText: %v", err)
	}
	if !strings.Contains(text, "Hello Città world & friends") {
		t.Errorf("fallback text = %q", text)
	}
}

func TestFetchPDFBodyReturnedIntact(t *testing.T) {
	pdf := []byte("%PDF-1.4\n1 0 obj\n<< /Type /Catalog >>\nendobj\ntrailer\n%%EOF")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/pdf")
		w.Write(pdf)
	}))
	t.Cleanup(srv.Close)

	f := &ingest.Fetcher{Guard: stubGuard{}}
	res, err := f.Fetch(context.Background(), srv.URL+"/doc.pdf")
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if string(res.Body) != string(pdf) {
		t.Errorf("PDF body altered: %d bytes", len(res.Body))
	}
	if !strings.Contains(res.ContentType, "application/pdf") {
		t.Errorf("ContentType = %q", res.ContentType)
	}
}

func TestFetchBinaryBodyReturnedIntact(t *testing.T) {
	bin := []byte{0x00, 0x01, 0x02, 0xff, 0xfe}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Write(bin)
	}))
	t.Cleanup(srv.Close)

	f := &ingest.Fetcher{Guard: stubGuard{}}
	res, err := f.Fetch(context.Background(), srv.URL+"/blob")
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if string(res.Body) != string(bin) {
		t.Errorf("binary body altered")
	}
}

// stubDialGuard allows URLs but blocks listed hosts at dial time — used to
// drive the DialChecker wiring in both client paths.
type stubDialGuard struct {
	blockDial map[string]bool
}

func (g stubDialGuard) CheckURL(context.Context, *url.URL) error { return nil }

func (g stubDialGuard) CheckDial(_, address string) error {
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		return err
	}
	if g.blockDial[host] {
		return fmt.Errorf("netguard: blocked dial target %s", host)
	}
	return nil
}

func TestFetchDialCheckerBlocksAtConnect(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		fmt.Fprint(w, "secret")
	}))
	t.Cleanup(srv.Close)

	guard := stubDialGuard{blockDial: map[string]bool{"127.0.0.1": true}}
	for name, client := range map[string]*http.Client{"default": nil, "caller": {}, "caller-transport": {Transport: &http.Transport{}}} {
		t.Run(name, func(t *testing.T) {
			f := &ingest.Fetcher{Guard: guard, Client: client}
			_, err := f.Fetch(context.Background(), srv.URL+"/admin")
			if err == nil || !strings.Contains(err.Error(), "blocked dial") {
				t.Fatalf("Fetch = %v, want a dial-time block", err)
			}
			if hits.Load() != 0 {
				t.Errorf("server contacted despite the dial guard: %d hits", hits.Load())
			}
		})
	}
}

func TestFetchRedirectIntoBlockedRange(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		http.Redirect(w, r, "http://169.254.169.254/latest/meta-data/", http.StatusFound)
	}))
	t.Cleanup(srv.Close)

	f := &ingest.Fetcher{Guard: stubGuard{blocked: map[string]bool{"169.254.169.254": true}}}
	if _, err := f.Fetch(context.Background(), srv.URL+"/start"); err == nil || !strings.Contains(err.Error(), "redirect blocked") {
		t.Fatalf("Fetch through a blocked redirect: err = %v, want a guard block", err)
	}
}

func TestFetchInitialURLIsChecked(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		fmt.Fprint(w, "ok")
	}))
	t.Cleanup(srv.Close)

	f := &ingest.Fetcher{Guard: stubGuard{blocked: map[string]bool{"127.0.0.1": true}}}
	if _, err := f.Fetch(context.Background(), srv.URL); err == nil {
		t.Fatal("Fetch of a blocked initial URL: want error")
	}
	if hits.Load() != 0 {
		t.Errorf("server contacted despite a blocked initial URL: %d hits", hits.Load())
	}
}

func TestFetchZeroValueGuardIsFailClosed(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		fmt.Fprint(w, "ok")
	}))
	t.Cleanup(srv.Close)

	// No Guard configured: the production guard must apply (loopback blocked).
	f := &ingest.Fetcher{}
	if _, err := f.Fetch(context.Background(), srv.URL); err == nil {
		t.Fatal("zero-value Fetcher fetched loopback: guard is fail-open")
	}
	if hits.Load() != 0 {
		t.Errorf("server contacted despite the default guard: %d hits", hits.Load())
	}
}

func TestFetchRedirectCapBoundaryExact(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// /hop/N redirects N times before serving content.
		var n int
		if _, err := fmt.Sscanf(r.URL.Path, "/hop/%d", &n); err != nil {
			http.Error(w, "bad path", http.StatusBadRequest)
			return
		}
		if n > 0 {
			http.Redirect(w, r, fmt.Sprintf("/hop/%d", n-1), http.StatusFound)
			return
		}
		fmt.Fprint(w, "landed")
	}))
	t.Cleanup(srv.Close)

	f := &ingest.Fetcher{Guard: stubGuard{}, MaxRedirects: 2}
	if res, err := f.Fetch(context.Background(), srv.URL+"/hop/2"); err != nil {
		t.Fatalf("Fetch at exactly the cap (2 redirects): %v", err)
	} else if string(res.Body) != "landed" {
		t.Errorf("body = %q", res.Body)
	}
	if _, err := f.Fetch(context.Background(), srv.URL+"/hop/3"); err == nil {
		t.Fatal("Fetch one past the cap (3 redirects): want error")
	}
}

func TestFetchTooManyRedirects(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/again", http.StatusFound)
	}))
	t.Cleanup(srv.Close)

	f := &ingest.Fetcher{Guard: stubGuard{}, MaxRedirects: 5}
	_, err := f.Fetch(context.Background(), srv.URL+"/start")
	if err == nil || !strings.Contains(err.Error(), "too many redirects") {
		t.Fatalf("error = %v, want too many redirects", err)
	}
}

func TestFetchFollowsAllowedRedirectAndReportsFinalURL(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/start" {
			http.Redirect(w, r, "/final", http.StatusFound)
			return
		}
		fmt.Fprint(w, "landed")
	}))
	t.Cleanup(srv.Close)

	f := &ingest.Fetcher{Guard: stubGuard{}}
	res, err := f.Fetch(context.Background(), srv.URL+"/start")
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if !strings.HasSuffix(res.FinalURL, "/final") {
		t.Errorf("FinalURL = %q, want the redirect target", res.FinalURL)
	}
	if string(res.Body) != "landed" {
		t.Errorf("body = %q", res.Body)
	}
}

func TestFetchBodyOverCap(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write(make([]byte, 2<<20))
	}))
	t.Cleanup(srv.Close)

	f := &ingest.Fetcher{Guard: stubGuard{}, MaxBytes: 1 << 20}
	_, err := f.Fetch(context.Background(), srv.URL)
	if err == nil || !strings.Contains(err.Error(), "exceeds") {
		t.Fatalf("error = %v, want a body-cap error", err)
	}
}

func TestFetchRespectsContextDeadline(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
	}))
	t.Cleanup(srv.Close)

	f := &ingest.Fetcher{Guard: stubGuard{}}
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	start := time.Now()
	if _, err := f.Fetch(ctx, srv.URL); err == nil {
		t.Fatal("Fetch with a short deadline: want error")
	}
	if elapsed := time.Since(start); elapsed > 3*time.Second {
		t.Errorf("Fetch took %s, want prompt deadline enforcement", elapsed)
	}
}

func TestFetchNonSuccessStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.NotFound(w, r)
	}))
	t.Cleanup(srv.Close)

	f := &ingest.Fetcher{Guard: stubGuard{}}
	if _, err := f.Fetch(context.Background(), srv.URL); err == nil {
		t.Fatal("Fetch of a 404: want error")
	}
}

func mustURL(t *testing.T, raw string) *url.URL {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatalf("parse %q: %v", raw, err)
	}
	return u
}
