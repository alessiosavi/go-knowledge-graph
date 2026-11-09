package ingest

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"regexp"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/alessiosavi/go-knowledge-graph/internal/config"
	"github.com/alessiosavi/go-knowledge-graph/internal/dedup"
	"github.com/alessiosavi/go-knowledge-graph/internal/extract"
	"github.com/alessiosavi/go-knowledge-graph/internal/graph"
	"github.com/alessiosavi/go-knowledge-graph/internal/graph/graphtest"
)

// TestPDFToTextRejectsOutOfRangeStartxref pins the pre-flight guard: a
// trailing startxref at/beyond EOF makes dslipak/pdf v0.0.2 spin forever
// (uncatchable), so it must be rejected before the parser runs. The
// watchdog turns a regression into a failure instead of a hung test.
func TestPDFToTextRejectsOutOfRangeStartxref(t *testing.T) {
	body, err := os.ReadFile("testdata/sample.pdf")
	if err != nil {
		t.Fatal(err)
	}
	re := regexp.MustCompile(`startxref\s+\d+`)
	tampered := re.ReplaceAll(body, []byte("startxref\n9000000"))
	if bytes.Equal(tampered, body) {
		t.Fatal("fixture has no startxref to tamper with")
	}
	done := make(chan error, 1)
	go func() {
		_, _, err := PDFToText(tampered)
		done <- err
	}()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("out-of-range startxref: want error")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("PDFToText hung on an out-of-range startxref (parser EOF-spin)")
	}
}

func TestDecodeStripsUTF8BOM(t *testing.T) {
	bom := []byte{0xEF, 0xBB, 0xBF}
	t.Run("html", func(t *testing.T) {
		htmlBody := []byte(`<html><body><article><p>Città perché with a BOM body. Città perché with a BOM body.</p></article></body></html>`)
		l, err := Decode(append(append([]byte{}, bom...), htmlBody...), "", nil)
		if err != nil {
			t.Fatalf("Decode: %v", err)
		}
		if !strings.Contains(l.Text, "Città perché with a BOM body.") || strings.Contains(l.Text, "<p>") {
			t.Errorf("BOM'd html decode = %q", l.Text)
		}
	})
	t.Run("pdf", func(t *testing.T) {
		pdfBody, err := os.ReadFile("testdata/sample.pdf")
		if err != nil {
			t.Fatal(err)
		}
		l, err := Decode(append(append([]byte{}, bom...), pdfBody...), "application/pdf", nil)
		if err != nil {
			t.Fatalf("Decode BOM'd pdf: %v", err)
		}
		if !strings.Contains(l.Text, "GKG sample PDF fixture text") {
			t.Errorf("BOM'd pdf decode = %q", l.Text)
		}
	})
}

func TestDecodeHTMLSniffRequiresTagBoundary(t *testing.T) {
	// Plain text that merely starts with "<html" must pass through.
	plain := "<htmlfoo bar baz text is not markup at all."
	l, err := Decode([]byte(plain), "", nil)
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if l.Text != plain {
		t.Errorf("text = %q, want passthrough (no HTML sniff)", l.Text)
	}
}

func TestDefaultLoaderMirrorsIdentityPrecedence(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		w.Write([]byte("URL wins."))
	}))
	t.Cleanup(srv.Close)
	loader := DefaultLoader(&Fetcher{Guard: allowAllGuard{}})

	// URL is the identity-inferred kind first; the loader must match.
	l, err := loader(context.Background(), Input{Text: "INLINE", URL: srv.URL})
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if l.Text != "URL wins." {
		t.Errorf("text = %q, want the URL body (identity precedence)", l.Text)
	}
}

func TestDefaultLoaderReloadErrorsAreClear(t *testing.T) {
	loader := DefaultLoader(&Fetcher{Guard: allowAllGuard{}})
	for _, in := range []Input{
		{Kind: "file", Source: "notes.md", URL: "notes.md"},
		{Kind: "text", Source: "abc12345", URL: "abc12345"},
	} {
		_, err := loader(context.Background(), in)
		if err == nil || !strings.Contains(err.Error(), "resubmit") {
			t.Errorf("reload of %s input = %v, want a clear resubmit hint", in.Kind, err)
		}
	}
}

// spinInput builds the in-range EOF-spin fixture: a valid PDF whose
// startxref points at the trailing %%EOF comment.
func spinInput(t *testing.T) []byte {
	t.Helper()
	body, err := os.ReadFile("testdata/sample.pdf")
	if err != nil {
		t.Fatal(err)
	}
	eofIdx := bytes.LastIndex(body, []byte("%%EOF"))
	if eofIdx < 0 {
		t.Fatal("fixture is missing its EOF trailer marker")
	}
	re := regexp.MustCompile(`startxref\s+\d+`)
	tampered := re.ReplaceAll(body, []byte(fmt.Sprintf("startxref\n%d", eofIdx)))
	if bytes.Equal(tampered, body) {
		t.Fatal("fixture tamper failed")
	}
	return tampered
}

// TestPDFToTextInRangeSpinKilled pins the isolation guarantee against the
// in-range variant of the parser's EOF-spin: a startxref pointing at the
// trailing %%EOF comment passes every range check, yet makes dslipak/pdf
// spin forever. The parse therefore runs in a killable subprocess; the
// watchdog here fails a regression instead of hanging.
func TestPDFToTextInRangeSpinKilled(t *testing.T) {
	tampered := spinInput(t)
	old := pdfParseTimeout
	pdfParseTimeout = 2 * time.Second
	t.Cleanup(func() { pdfParseTimeout = old })

	done := make(chan error, 1)
	go func() {
		_, _, err := PDFToText(tampered)
		done <- err
	}()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("in-range spin input: want a timeout error")
		}
	case <-time.After(8 * time.Second):
		t.Fatal("PDFToText hung: the parser spin was not isolated/killed")
	}
}

// TestPDFExtractChildWatchdog pins the child-side watchdog: even when the
// parent is gone, a spinning parser cannot outlive the parser process.
func TestPDFExtractChildWatchdog(t *testing.T) {
	tampered := spinInput(t)
	old := pdfParseTimeout
	pdfParseTimeout = time.Second
	t.Cleanup(func() { pdfParseTimeout = old })

	done := make(chan int, 1)
	go func() {
		done <- PDFExtract(bytes.NewReader(tampered), &bytes.Buffer{}, &bytes.Buffer{})
	}()
	select {
	case code := <-done:
		if code == 0 {
			t.Fatal("spinning input inside the child: want a nonzero exit")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("child watchdog did not trip")
	}
}

func TestLimitedBufferHonorsWriteContract(t *testing.T) {
	b := &limitedBuffer{limit: 5}
	if n, err := b.Write([]byte("12345")); n != 5 || err != nil {
		t.Fatalf("Write = %d, %v", n, err)
	}
	if _, err := b.Write([]byte("6")); !errors.Is(err, errOutputLimit) {
		t.Fatalf("overflowing Write error = %v, want errOutputLimit", err)
	}
	if b.String() != "12345" {
		t.Errorf("buffer = %q, want the capped content", b.String())
	}
}

// TestDefaultLoaderReloadKindGating pins that reload dispatch honors the
// document kind: a file/text document whose source parses as a URL is NOT
// re-fetched, and a URL document reloads from its real URL.
func TestDefaultLoaderReloadKindGating(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.Header().Set("Content-Type", "text/plain")
		w.Write([]byte("real content"))
	}))
	t.Cleanup(srv.Close)
	loader := DefaultLoader(&Fetcher{Guard: allowAllGuard{}})

	// kind=file with a URL-shaped source must fail with a resubmit hint,
	// never fetch.
	if _, err := loader(context.Background(), Input{Kind: "file", Source: srv.URL, URL: srv.URL}); err == nil || !strings.Contains(err.Error(), "resubmit") {
		t.Errorf("file reload with URL-shaped source = %v, want a resubmit hint", err)
	}
	if hits.Load() != 0 {
		t.Errorf("file reload fetched the URL-shaped source: %d hits", hits.Load())
	}

	// kind=url reloads via the given URL.
	l, err := loader(context.Background(), Input{Kind: "url", Source: "custom-id", URL: srv.URL + "/doc"})
	if err != nil || l.Text != "real content" {
		t.Errorf("url reload = %+v, %v", l, err)
	}
}

// TestRecoveryReloadURLDocument exercises the real loader end to end: a URL
// document submitted with a caller-supplied identity source must recover
// after a restart by re-fetching its FinalURL.
func TestRecoveryReloadURLDocument(t *testing.T) {
	st := graphtest.Store(t)
	cfg, err := config.Load("")
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	body := fmt.Sprintf("Stable remote content for the reload test %d.", time.Now().UnixNano())
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)

	loader := DefaultLoader(&Fetcher{Guard: allowAllGuard{}})
	p1 := NewPipeline(cfg, st, &extract.Mock{}, dedup.New(), loader)
	res, err := p1.Submit(ctx, Input{Kind: "url", URL: srv.URL + "/doc", Source: "custom-id"})
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}

	// Restart: fresh pipeline, no submission cache.
	mock := &extract.Mock{}
	p2 := NewPipeline(cfg, st, mock, dedup.New(), loader)
	if err := p2.Process(ctx, res.DocumentID); err != nil {
		t.Fatalf("Process after restart: %v", err)
	}
	doc, err := st.Document(ctx, res.DocumentID)
	if err != nil {
		t.Fatal(err)
	}
	if doc.Status != graph.StatusDone {
		t.Fatalf("status = %q (%s), want done", doc.Status, doc.Error)
	}
	if mock.Calls() != 1 {
		t.Errorf("provider calls = %d, want 1", mock.Calls())
	}
}

// TestRecoveryReloadFileDocumentFailsCleanly pins that file (bytes)
// submissions cannot be reloaded and fail with the documented resubmit
// hint instead of a transport error.
func TestRecoveryReloadFileDocumentFailsCleanly(t *testing.T) {
	st := graphtest.Store(t)
	cfg, err := config.Load("")
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	loader := DefaultLoader(&Fetcher{Guard: allowAllGuard{}})
	p1 := NewPipeline(cfg, st, &extract.Mock{}, dedup.New(), loader)
	res, err := p1.Submit(ctx, Input{Kind: "file", Source: "notes.md", Bytes: []byte("File body that will not be reloadable.")})
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}
	p2 := NewPipeline(cfg, st, &extract.Mock{}, dedup.New(), loader)
	if err := p2.Process(ctx, res.DocumentID); err == nil {
		t.Fatal("Process of a reloaded file document: want error")
	}
	doc, err := st.Document(ctx, res.DocumentID)
	if err != nil {
		t.Fatal(err)
	}
	if doc.Status != graph.StatusFailed || !strings.Contains(doc.Error, "resubmit") {
		t.Errorf("status/error = %q/%q, want failed with a resubmit hint", doc.Status, doc.Error)
	}
}

func TestDecodeText(t *testing.T) {
	l, err := Decode([]byte("Plain text without markup."), "text/plain; charset=utf-8", nil)
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if l.Text != "Plain text without markup." {
		t.Errorf("text = %q", l.Text)
	}
	if l.Hash != sha256Hex(l.Text) || l.Hash == "" {
		t.Errorf("hash = %q, want sha256 of the cleaned text", l.Hash)
	}
}

func TestDecodeMarkdownKeepsHeadings(t *testing.T) {
	body, err := os.ReadFile("testdata/sample.md")
	if err != nil {
		t.Fatal(err)
	}
	l, err := Decode(body, "text/markdown", nil)
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	for _, want := range []string{"# Sample Document", "## Details", "Città perché"} {
		if !strings.Contains(l.Text, want) {
			t.Errorf("markdown decode lost %q:\n%s", want, l.Text)
		}
	}
}

func TestDecodeHTMLByContentTypeAndSniff(t *testing.T) {
	body := []byte(`<html><body><article><p>Città perché is readable. Città perché is readable. Città perché is readable.</p></article></body></html>`)
	for name, ct := range map[string]string{"content-type": "text/html; charset=utf-8", "sniff": ""} {
		t.Run(name, func(t *testing.T) {
			l, err := Decode(body, ct, &url.URL{Scheme: "https", Host: "example.test"})
			if err != nil {
				t.Fatalf("Decode: %v", err)
			}
			if !strings.Contains(l.Text, "Città perché is readable.") {
				t.Errorf("html decode = %q", l.Text)
			}
			if strings.Contains(l.Text, "<p>") {
				t.Errorf("html decode left tags: %q", l.Text)
			}
		})
	}
}

func TestDecodePDFFromBytes(t *testing.T) {
	body, err := os.ReadFile("testdata/sample.pdf")
	if err != nil {
		t.Fatal(err)
	}
	l, err := Decode(body, "application/pdf", nil)
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if !strings.Contains(l.Text, "GKG sample PDF fixture text for extraction checks.") {
		t.Errorf("pdf decode = %q, want the fixture phrase", l.Text)
	}
}

// TestDecodePDFFromURLBody pins the PDF-over-URL fix: the %PDF magic wins
// over a generic content type.
func TestDecodePDFFromURLBody(t *testing.T) {
	body, err := os.ReadFile("testdata/sample.pdf")
	if err != nil {
		t.Fatal(err)
	}
	l, err := Decode(body, "application/octet-stream", &url.URL{Scheme: "https", Host: "files.example.test", Path: "/doc"})
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if !strings.Contains(l.Text, "GKG sample PDF fixture text") {
		t.Errorf("pdf-over-url decode = %q", l.Text)
	}
}

func TestDecodeUnknownBinaryErrors(t *testing.T) {
	body := []byte{0x89, 0x50, 0x4e, 0x47, 0xff, 0xfe, 0x00, 0x01} // PNG-ish, invalid UTF-8
	if _, err := Decode(body, "application/octet-stream", nil); err == nil {
		t.Fatal("Decode(unknown binary): want error")
	}
}

func TestDecodeEmptyBodyErrors(t *testing.T) {
	if _, err := Decode(nil, "text/plain", nil); err == nil {
		t.Fatal("Decode(empty): want error")
	}
}

// TestPDFToTextNeverPanics walks truncated prefixes of the fixture: the
// library is best-effort and its panics must surface as errors.
func TestPDFToTextNeverPanics(t *testing.T) {
	body, err := os.ReadFile("testdata/sample.pdf")
	if err != nil {
		t.Fatal(err)
	}
	for cut := 5; cut < len(body); cut += 97 {
		func() {
			defer func() {
				if r := recover(); r != nil {
					t.Fatalf("PDFToText panicked on a %d-byte prefix: %v", cut, r)
				}
			}()
			_, _, err := PDFToText(body[:cut])
			_ = err // an error is fine; the point is that nothing escapes
		}()
	}
}

func TestIdentityRules(t *testing.T) {
	cases := []struct {
		name string
		in   Input
		kind string
		src  string
		err  bool
	}{
		{"url", Input{URL: "https://x.example/y"}, "url", "https://x.example/y", false},
		{"url with source override", Input{URL: "https://x.example/y", Source: "custom"}, "url", "custom", false},
		{"file with source", Input{Bytes: []byte("x"), Source: "notes.md"}, "file", "notes.md", false},
		{"file without source", Input{Bytes: []byte("x")}, "", "", true},
		{"text with source", Input{Text: "x", Source: "readme"}, "text", "readme", false},
		{"text without source", Input{Text: "x"}, "text", "abc12345", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			kind, source, err := resolveIdentity(tc.in, "abc12345ffff")
			if tc.err {
				if err == nil {
					t.Fatalf("resolveIdentity(%+v) = %s/%s, want error", tc.in, kind, source)
				}
				return
			}
			if err != nil {
				t.Fatalf("resolveIdentity: %v", err)
			}
			if kind != tc.kind || source != tc.src {
				t.Errorf("resolveIdentity = %s/%s, want %s/%s", kind, source, tc.kind, tc.src)
			}
		})
	}
}

// allowAllGuard permits everything for the loader tests.
type allowAllGuard struct{}

func (allowAllGuard) CheckURL(context.Context, *url.URL) error { return nil }

func TestDefaultLoader(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/missing" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.Write([]byte("Fetched body text."))
	}))
	t.Cleanup(srv.Close)

	loader := DefaultLoader(&Fetcher{Guard: allowAllGuard{}})

	t.Run("inline text", func(t *testing.T) {
		l, err := loader(context.Background(), Input{Text: "inline text"})
		if err != nil || l.Text != "inline text" {
			t.Fatalf("load = %+v, %v", l, err)
		}
	})
	t.Run("bytes", func(t *testing.T) {
		l, err := loader(context.Background(), Input{Bytes: []byte("bytes body"), Source: "x.txt"})
		if err != nil || l.Text != "bytes body" {
			t.Fatalf("load = %+v, %v", l, err)
		}
	})
	t.Run("url", func(t *testing.T) {
		l, err := loader(context.Background(), Input{URL: srv.URL + "/doc"})
		if err != nil || l.Text != "Fetched body text." {
			t.Fatalf("load = %+v, %v", l, err)
		}
	})
	t.Run("url failure", func(t *testing.T) {
		if _, err := loader(context.Background(), Input{URL: srv.URL + "/missing"}); err == nil {
			t.Fatal("HTTP 404 through the loader: want error")
		}
	})
	t.Run("empty input", func(t *testing.T) {
		if _, err := loader(context.Background(), Input{}); err == nil {
			t.Fatal("empty input: want error")
		}
	})
}

func TestDefaultLoaderNilFetcher(t *testing.T) {
	loader := DefaultLoader(nil)
	if _, err := loader(context.Background(), Input{URL: "https://x.example/"}); err == nil {
		t.Fatal("url input with a nil fetcher: want error")
	}
}
