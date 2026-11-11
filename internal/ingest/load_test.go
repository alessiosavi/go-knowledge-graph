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
	loader := DefaultLoader(&Fetcher{Guard: allowAllGuard{}}, nil)

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
	loader := DefaultLoader(&Fetcher{Guard: allowAllGuard{}}, nil)
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
	loader := DefaultLoader(&Fetcher{Guard: allowAllGuard{}}, nil)

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
	cfg.Ingest.SpoolDir = t.TempDir()
	ctx := context.Background()
	body := fmt.Sprintf("Stable remote content for the reload test %d.", time.Now().UnixNano())
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)

	loader := DefaultLoader(&Fetcher{Guard: allowAllGuard{}}, nil)
	p1 := NewPipeline(cfg, st, &extract.Mock{}, dedup.New(), loader)
	res, err := p1.Submit(ctx, Input{Kind: "url", URL: srv.URL + "/doc", Source: "custom-id"})
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}

	// Legacy/missing-spool recovery still re-fetches the URL.
	cfg.Ingest.SpoolDir = t.TempDir()
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

// Missing-spool/legacy file submissions fail with a resubmit hint.
func TestRecoveryReloadFileDocumentFailsCleanly(t *testing.T) {
	st := graphtest.Store(t)
	cfg, err := config.Load("")
	if err != nil {
		t.Fatal(err)
	}
	cfg.Ingest.SpoolDir = t.TempDir()
	ctx := context.Background()
	loader := DefaultLoader(&Fetcher{Guard: allowAllGuard{}}, nil)
	p1 := NewPipeline(cfg, st, &extract.Mock{}, dedup.New(), loader)
	res, err := p1.Submit(ctx, Input{Kind: "file", Source: "notes.md", Bytes: []byte("File body that will not be reloadable.")})
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}
	cfg.Ingest.SpoolDir = t.TempDir()
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

	loader := DefaultLoader(&Fetcher{Guard: allowAllGuard{}}, nil)

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
	loader := DefaultLoader(nil, nil)
	if _, err := loader(context.Background(), Input{URL: "https://x.example/"}); err == nil {
		t.Fatal("url input with a nil fetcher: want error")
	}
}

// stubConverter records the last Convert call and answers with md (or err).
type stubConverter struct {
	calls    int
	lastBody []byte
	lastName string
	lastCT   string
	md       string
	err      error
}

func (s *stubConverter) Convert(_ context.Context, body []byte, filename, contentType string) (string, error) {
	s.calls++
	s.lastBody = append([]byte(nil), body...)
	s.lastName, s.lastCT = filename, contentType
	if s.err != nil {
		return "", s.err
	}
	return s.md, nil
}

// TestLoaderConverterDispatchesPdfZipImage pins the sniff dispatch: with a
// converter attached, PDF, zip (OOXML) and image bodies go through it and
// the returned markdown becomes the loaded text (hash included).
func TestLoaderConverterDispatchesPdfZipImage(t *testing.T) {
	png := append([]byte{0x89, 0x50, 0x4E, 0x47, 0x0D, 0x0A, 0x1A, 0x0A}, []byte("pngdata")...)
	cases := []struct {
		name string
		body []byte
		want string // detected part content type
	}{
		{"pdf", []byte("%PDF-1.4 converts"), "application/pdf"},
		{"zip", []byte("PK\x03\x04zipbytes"), "application/zip"},
		{"image", png, "image/png"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			conv := &stubConverter{md: "# md from " + tc.name}
			loader := DefaultLoader(nil, conv)
			l, err := loader(context.Background(), Input{Kind: "file", Source: "doc." + tc.name, Bytes: tc.body})
			if err != nil {
				t.Fatalf("load: %v", err)
			}
			if l.Text != conv.md {
				t.Errorf("text = %q, want the converter markdown", l.Text)
			}
			if l.Hash != sha256Hex(l.Text) || l.Hash == "" {
				t.Errorf("hash = %q, want sha256 of the converted markdown", l.Hash)
			}
			if conv.calls != 1 {
				t.Fatalf("converter calls = %d, want 1", conv.calls)
			}
			if conv.lastName != "doc."+tc.name {
				t.Errorf("filename = %q, want the source name", conv.lastName)
			}
			if conv.lastCT != tc.want {
				t.Errorf("content-type = %q, want %q (file inputs detect from bytes)", conv.lastCT, tc.want)
			}
			if string(conv.lastBody) != string(tc.body) {
				t.Errorf("body = %q, want the raw document bytes", conv.lastBody)
			}
		})
	}
}

func TestLoaderConverterKeepsHtmlReadability(t *testing.T) {
	conv := &stubConverter{md: "SHOULD NOT BE USED"}
	loader := DefaultLoader(nil, conv)
	body := []byte(`<html><body><article><p>Città perché is readable. Città perché is readable. Città perché is readable.</p></article></body></html>`)
	l, err := loader(context.Background(), Input{Kind: "file", Source: "page.html", Bytes: body})
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if conv.calls != 0 {
		t.Errorf("converter calls = %d, want 0 (HTML keeps the readability path)", conv.calls)
	}
	if !strings.Contains(l.Text, "Città perché is readable.") {
		t.Errorf("text = %q, want the readability output", l.Text)
	}
}

func TestLoaderConverterPassesTextThrough(t *testing.T) {
	conv := &stubConverter{md: "SHOULD NOT BE USED"}
	loader := DefaultLoader(nil, conv)
	l, err := loader(context.Background(), Input{Kind: "file", Source: "notes.md", Bytes: []byte("# Heading\n\nBody text.")})
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if conv.calls != 0 {
		t.Errorf("converter calls = %d, want 0 (text passes through)", conv.calls)
	}
	if l.Text != "# Heading\n\nBody text." {
		t.Errorf("text = %q, want the markdown passthrough", l.Text)
	}
}

// TestLoaderConverterDoesNotConvertTextWithImagePrefixes pins the weak-magic
// guard: prose starting with "BM25…" (BMP's 2-byte magic) or "GIF89a…"
// (GIF's 6-byte magic) is valid UTF-8 text and must pass through — only
// binary bodies route to the converter.
func TestLoaderConverterDoesNotConvertTextWithImagePrefixes(t *testing.T) {
	cases := map[string][]byte{
		"bm-text":  []byte("BM25 is a ranking function for search engines."),
		"gif-text": []byte("GIF89a is the acronym used by the GIF format."),
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			conv := &stubConverter{md: "SHOULD NOT BE USED"}
			loader := DefaultLoader(nil, conv)
			l, err := loader(context.Background(), Input{Kind: "file", Source: name + ".md", Bytes: body})
			if err != nil {
				t.Fatalf("load: %v", err)
			}
			if conv.calls != 0 {
				t.Errorf("converter calls = %d, want 0 (UTF-8 text must pass through)", conv.calls)
			}
			if l.Text != string(body) {
				t.Errorf("text = %q, want the passthrough", l.Text)
			}
		})
	}
}

func TestLoaderConverterFailureFailsLoad(t *testing.T) {
	conv := &stubConverter{err: errors.New("docling boom")}
	loader := DefaultLoader(nil, conv)
	_, err := loader(context.Background(), Input{Kind: "file", Source: "scanned.pdf", Bytes: []byte("%PDF-1.4 scanned")})
	if err == nil || !strings.Contains(err.Error(), "docling boom") {
		t.Fatalf("load err = %v, want the converter failure surfaced", err)
	}
}

func TestLoaderConverterOffUnchanged(t *testing.T) {
	loader := DefaultLoader(nil, nil)
	// A binary body with no converter keeps the existing decode rejection.
	png := append([]byte{0x89, 0x50, 0x4E, 0x47, 0xFF, 0xFE}, byte(0x00))
	if _, err := loader(context.Background(), Input{Kind: "file", Source: "x.bin", Bytes: png}); err == nil {
		t.Fatal("binary with no converter: want the existing decode error")
	}
	// Text passthrough is identical.
	l, err := loader(context.Background(), Input{Kind: "file", Source: "x.txt", Bytes: []byte("text body")})
	if err != nil || l.Text != "text body" {
		t.Fatalf("load = %+v, %v; want the unchanged passthrough", l, err)
	}
}

// TestLoaderFilenameDerivation pins the multipart naming contract: file
// inputs keep their source name (content type detected from bytes); URL
// inputs use the path basename when it carries an extension, else the
// sniffed canonical extension; the fetched content type is passed through.
func TestLoaderFilenameDerivation(t *testing.T) {
	t.Run("file keeps its source name and detects the content type", func(t *testing.T) {
		conv := &stubConverter{md: "x"}
		loader := DefaultLoader(nil, conv)
		body := []byte("%PDF-1.4 minimal")
		if _, err := loader(context.Background(), Input{Kind: "file", Source: "notes.docx", Bytes: body}); err != nil {
			t.Fatalf("load: %v", err)
		}
		if conv.lastName != "notes.docx" {
			t.Errorf("filename = %q, want the source name", conv.lastName)
		}
		if conv.lastCT != http.DetectContentType(body) {
			t.Errorf("content-type = %q, want DetectContentType(bytes)", conv.lastCT)
		}
	})

	t.Run("url uses the path basename when it has an extension", func(t *testing.T) {
		body := []byte("%PDF-1.4 served")
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/pdf")
			_, _ = w.Write(body)
		}))
		t.Cleanup(srv.Close)
		conv := &stubConverter{md: "x"}
		loader := DefaultLoader(&Fetcher{Guard: allowAllGuard{}}, conv)
		if _, err := loader(context.Background(), Input{Kind: "url", URL: srv.URL + "/files/report.pdf"}); err != nil {
			t.Fatalf("load: %v", err)
		}
		if conv.lastName != "report.pdf" {
			t.Errorf("filename = %q, want the URL path basename", conv.lastName)
		}
		if conv.lastCT != "application/pdf" {
			t.Errorf("content-type = %q, want the fetched content type", conv.lastCT)
		}
	})

	t.Run("url without an extension gets the sniffed canonical extension", func(t *testing.T) {
		png := append([]byte{0x89, 0x50, 0x4E, 0x47, 0x0D, 0x0A, 0x1A, 0x0A}, []byte("imagedata")...)
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "image/png")
			_, _ = w.Write(png)
		}))
		t.Cleanup(srv.Close)
		conv := &stubConverter{md: "x"}
		loader := DefaultLoader(&Fetcher{Guard: allowAllGuard{}}, conv)
		if _, err := loader(context.Background(), Input{Kind: "url", URL: srv.URL + "/download"}); err != nil {
			t.Fatalf("load: %v", err)
		}
		if conv.lastName != "download.png" {
			t.Errorf("filename = %q, want the basename plus the sniffed extension", conv.lastName)
		}
		if conv.lastCT != "image/png" {
			t.Errorf("content-type = %q, want the fetched content type", conv.lastCT)
		}
	})

	t.Run("url with an opaque path uses a document name", func(t *testing.T) {
		body := []byte("%PDF-1.4 opaque")
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/pdf")
			_, _ = w.Write(body)
		}))
		t.Cleanup(srv.Close)
		conv := &stubConverter{md: "x"}
		loader := DefaultLoader(&Fetcher{Guard: allowAllGuard{}}, conv)
		if _, err := loader(context.Background(), Input{Kind: "url", URL: srv.URL + "/"}); err != nil {
			t.Fatalf("load: %v", err)
		}
		if conv.lastName != "document.pdf" {
			t.Errorf("filename = %q, want document.pdf for an opaque path", conv.lastName)
		}
	})

	t.Run("url with a generic content type derives .pdf from the magic", func(t *testing.T) {
		body := []byte("%PDF-1.4 blob")
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/octet-stream")
			_, _ = w.Write(body)
		}))
		t.Cleanup(srv.Close)
		conv := &stubConverter{md: "x"}
		loader := DefaultLoader(&Fetcher{Guard: allowAllGuard{}}, conv)
		if _, err := loader(context.Background(), Input{Kind: "url", URL: srv.URL + "/blob/9f8a2"}); err != nil {
			t.Fatalf("load: %v", err)
		}
		if conv.lastName != "9f8a2.pdf" {
			t.Errorf("filename = %q, want the basename plus the PDF magic extension", conv.lastName)
		}
	})

	t.Run("url with a BOM-prefixed PDF and a generic content type derives .pdf", func(t *testing.T) {
		body := append([]byte{0xEF, 0xBB, 0xBF}, []byte("%PDF-1.4 bom blob")...)
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/octet-stream")
			_, _ = w.Write(body)
		}))
		t.Cleanup(srv.Close)
		conv := &stubConverter{md: "x"}
		loader := DefaultLoader(&Fetcher{Guard: allowAllGuard{}}, conv)
		if _, err := loader(context.Background(), Input{Kind: "url", URL: srv.URL + "/blob/bom9f8a2"}); err != nil {
			t.Fatalf("load: %v", err)
		}
		if conv.lastName != "bom9f8a2.pdf" {
			t.Errorf("filename = %q, want the basename plus the PDF magic extension (BOM tolerated)", conv.lastName)
		}
	})
}
