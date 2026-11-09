package ingest

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"github.com/dslipak/pdf"
)

// pdfParseTimeout bounds the isolated parser process. On expiry the child
// is killed — dslipak/pdf v0.0.2 can spin forever (a readToken loop that
// never sets its EOF flag), and the only safe containment available
// in-process is a killable process boundary. Tests shorten it. The child
// enforces the same bound for itself, so an orphaned parser (parent died)
// still exits.
var pdfParseTimeout = 30 * time.Second

const pdfTextCap = 32 << 20

// PDFToText is the best-effort pure-Go PDF text extraction (dslipak/pdf
// v0.0.2), run in a killable subprocess so parser hangs cannot wedge the
// service. No OCR, no tables or figures — the caveat is documented in
// docs/known-limitations.md and the sidecar's higher-quality text path is
// an explicit v0.1 deferral.
func PDFToText(body []byte) (text, title string, err error) {
	defer func() {
		if r := recover(); r != nil {
			text, title = "", ""
			err = fmt.Errorf("ingest: pdf: recovered panic: %v", r)
		}
	}()

	if err := validatePDFEnvelope(body); err != nil {
		return "", "", fmt.Errorf("ingest: pdf: %w", err)
	}

	exe, err := os.Executable()
	if err != nil {
		return "", "", fmt.Errorf("ingest: pdf: locate executable: %w", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), pdfParseTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, exe, "pdf-extract")
	cmd.Stdin = bytes.NewReader(body)
	stdout := &limitedBuffer{limit: pdfTextCap}
	stderr := &limitedBuffer{limit: 64 << 10}
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	runErr := cmd.Run()
	if ctx.Err() == context.DeadlineExceeded {
		return "", "", fmt.Errorf("ingest: pdf: parser killed after %s (hostile or very large document?)", pdfParseTimeout)
	}
	if runErr != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = runErr.Error()
		}
		return "", "", fmt.Errorf("ingest: pdf: %s", msg)
	}
	text = strings.TrimSpace(stdout.String())
	if text == "" {
		return "", "", errors.New("ingest: pdf: no extractable text (scanned document?)")
	}
	return text, "", nil
}

// PDFExtract is the isolated parser entry point ("pdf-extract" subcommand):
// it reads a PDF from stdin and prints the extracted text to stdout. It
// runs in a child process precisely because the parser is not interruptible.
// The parse runs in a goroutine with an internal watchdog: even if the
// parent dies, a spinning parser cannot outlive this process — returning
// exits it and takes the spinner down.
func PDFExtract(stdin io.Reader, stdout, stderr io.Writer) int {
	body, err := io.ReadAll(io.LimitReader(stdin, 64<<20))
	if err != nil {
		fmt.Fprintln(stderr, "ingest: pdf: read input:", err)
		return 1
	}
	type result struct {
		text string
		err  error
	}
	done := make(chan result, 1)
	go func() {
		text, _, err := pdfExtractInProcess(body)
		done <- result{text, err}
	}()
	select {
	case r := <-done:
		if r.err != nil {
			fmt.Fprintln(stderr, r.err)
			return 1
		}
		fmt.Fprint(stdout, r.text)
		return 0
	case <-time.After(pdfParseTimeout):
		fmt.Fprintf(stderr, "ingest: pdf: internal watchdog tripped after %s\n", pdfParseTimeout)
		return 1
	}
}

// pdfExtractInProcess is the actual parser call (only ever reached inside
// the isolated child or the unit tests' extraction mode).
func pdfExtractInProcess(body []byte) (text, title string, err error) {
	defer func() {
		if r := recover(); r != nil {
			text, title = "", ""
			err = fmt.Errorf("ingest: pdf: recovered panic: %v", r)
		}
	}()
	if err := validatePDFEnvelope(body); err != nil {
		return "", "", fmt.Errorf("ingest: pdf: %w", err)
	}
	reader, err := pdf.NewReader(bytes.NewReader(body), int64(len(body)))
	if err != nil {
		return "", "", fmt.Errorf("ingest: pdf: open: %w", err)
	}
	pr, err := reader.GetPlainText()
	if err != nil {
		return "", "", fmt.Errorf("ingest: pdf: extract text: %w", err)
	}
	raw, err := io.ReadAll(io.LimitReader(pr, pdfTextCap))
	if err != nil {
		return "", "", fmt.Errorf("ingest: pdf: read text: %w", err)
	}
	text = strings.TrimSpace(string(raw))
	if text == "" {
		return "", "", fmt.Errorf("ingest: pdf: no extractable text (scanned document?)")
	}
	return text, "", nil
}

// limitedBuffer caps child output: once the limit would be exceeded it
// reports the standard short-write error so the io.Copy in os/exec stops
// with a diagnosable failure instead of silently truncating.
type limitedBuffer struct {
	buf   bytes.Buffer
	limit int
}

var errOutputLimit = errors.New("output limit exceeded")

func (b *limitedBuffer) Write(p []byte) (int, error) {
	if len(p) > b.limit-b.buf.Len() {
		return 0, errOutputLimit
	}
	return b.buf.Write(p)
}

func (b *limitedBuffer) String() string { return b.buf.String() }

// validatePDFEnvelope rejects obviously unusable trailers before any
// parsing: missing or malformed startxref, and offsets at/beyond EOF. It is
// a fast-fail pre-check only — the full containment guarantee is the
// killable subprocess, because in-range crafted offsets can still spin the
// parser.
func validatePDFEnvelope(body []byte) error {
	tail := body
	if len(tail) > 4096 {
		tail = tail[len(tail)-4096:]
	}
	idx := bytes.LastIndex(tail, []byte("startxref"))
	if idx == -1 {
		return errors.New("missing startxref trailer")
	}
	rest := bytes.TrimLeft(tail[idx+len("startxref"):], " \t\r\n")
	i := 0
	for i < len(rest) && rest[i] >= '0' && rest[i] <= '9' {
		i++
	}
	if i == 0 {
		return errors.New("malformed startxref offset")
	}
	off, err := strconv.ParseInt(string(rest[:i]), 10, 64)
	if err != nil {
		return fmt.Errorf("startxref offset: %w", err)
	}
	if off < 0 || off >= int64(len(body)) {
		return fmt.Errorf("startxref offset %d out of range (file is %d bytes)", off, len(body))
	}
	return nil
}
