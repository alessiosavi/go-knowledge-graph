package ingest

import (
	"os"
	"testing"
)

// TestMain doubles as the isolated PDF parser entry point: PDFToText
// re-executes this test binary with the "pdf-extract" argument (the
// production binary exposes the same hidden subcommand), so parser spins
// are killed by the parent's timeout instead of wedging the test process.
func TestMain(m *testing.M) {
	if len(os.Args) > 1 && os.Args[1] == "pdf-extract" {
		os.Exit(PDFExtract(os.Stdin, os.Stdout, os.Stderr))
	}
	os.Exit(m.Run())
}
