package main

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/alessiosavi/go-knowledge-graph/internal/config"
)

// TestBinaryDoesNotDependOnTesting pins that production packages never pull
// the Go test framework into the gkg binary: test support for a package must
// live in its own package, imported only from _test.go files.
func TestBinaryDoesNotDependOnTesting(t *testing.T) {
	cmd := exec.Command("go", "list", "-deps", "./cmd/gkg")
	cmd.Dir = filepath.Join("..", "..")
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("go list -deps ./cmd/gkg: %v", err)
	}
	for _, line := range strings.Split(string(out), "\n") {
		if strings.TrimSpace(line) == "testing" {
			t.Fatal("the gkg binary transitively imports the testing package; keep test support out of production packages")
		}
	}
}

func TestLoadConfigFromGKGConfig(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "custom.yaml")
	if err := os.WriteFile(path, []byte("server:\n  addr: \":19999\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GKG_CONFIG", path)
	cfg, err := loadConfig()
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	if cfg.Server.Addr != ":19999" {
		t.Errorf("Server.Addr = %q, want the file value", cfg.Server.Addr)
	}
}

func TestLoadConfigAutoDiscoversConfigYaml(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "config.yaml"), []byte("server:\n  addr: \":17777\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Chdir(dir)
	t.Setenv("GKG_CONFIG", "")
	cfg, err := loadConfig()
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	if cfg.Server.Addr != ":17777" {
		t.Errorf("Server.Addr = %q, want the discovered config.yaml value", cfg.Server.Addr)
	}
}

func TestRunDispatch(t *testing.T) {
	cases := []struct {
		name     string
		args     []string
		env      map[string]string
		wantExit int
		wantOut  string
		wantErr  string
	}{
		{"version", []string{"version"}, nil, 0, "gkg v", ""},
		{"help", []string{"help"}, nil, 0, "usage: gkg", ""},
		{"help flag", []string{"-h"}, nil, 0, "usage: gkg", ""},
		{"no args", nil, nil, 2, "", "usage: gkg"},
		{"unknown", []string{"bogus"}, nil, 2, "", "unknown command"},
		{"serve bad uri", []string{"serve"}, map[string]string{"GKG_MEMGRAPH_URI": "://bad"}, 1, "", "gkg serve:"},
		{"ingest no args", []string{"ingest"}, nil, 2, "", "gkg ingest:"},
		{"ingest bad uri", []string{"ingest", "sample.md"}, map[string]string{"GKG_MEMGRAPH_URI": "://bad"}, 1, "", "gkg ingest:"},
		{"mcp bad uri", []string{"mcp"}, map[string]string{"GKG_MEMGRAPH_URI": "://bad"}, 1, "", "gkg mcp:"},
		{"eval missing golden", []string{"eval", "--golden", "/nonexistent-golden.jsonl"}, map[string]string{"GKG_EXTRACTOR_MODE": "mock"}, 1, "", "gkg eval:"},
		{"migrate bad uri", []string{"migrate"}, map[string]string{"GKG_MEMGRAPH_URI": "://bad"}, 1, "", "gkg migrate:"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			for k, v := range tc.env {
				t.Setenv(k, v)
			}
			var out, errb bytes.Buffer
			code := run(tc.args, &out, &errb)
			if code != tc.wantExit {
				t.Errorf("exit = %d, want %d", code, tc.wantExit)
			}
			if tc.wantOut == "" {
				if out.Len() != 0 {
					t.Errorf("stdout = %q, want empty", out.String())
				}
			} else if !strings.Contains(out.String(), tc.wantOut) {
				t.Errorf("stdout = %q, want it to contain %q", out.String(), tc.wantOut)
			}
			if tc.wantErr == "" {
				if errb.Len() != 0 {
					t.Errorf("stderr = %q, want empty", errb.String())
				}
			} else if !strings.Contains(errb.String(), tc.wantErr) {
				t.Errorf("stderr = %q, want it to contain %q", errb.String(), tc.wantErr)
			}
		})
	}
}

func TestEmbedderFromConfigOpenAIDefault(t *testing.T) {
	cfg := config.Config{}
	cfg.Embedder.Dim = 768
	emb, err := embedderFromConfig(cfg)
	if err != nil {
		t.Fatalf("embedderFromConfig: %v", err)
	}
	if emb.Name() != "openai" {
		t.Errorf("Name = %q, want openai for the empty/default provider", emb.Name())
	}
	if got := emb.Space(); got != "openai|text-embedding-3-small|768" {
		t.Errorf("Space = %q, want openai|text-embedding-3-small|768", got)
	}
}

func TestEmbedderFromConfigFurlsValidation(t *testing.T) {
	base := config.EmbedderConfig{
		Provider:    "furls",
		ProductFURL: "https://product.example/v1",
		QueryFURL:   "https://query.example/v1",
		APIKey:      "k",
		Dim:         768,
	}
	cases := []struct {
		name   string
		mutate func(*config.EmbedderConfig)
		want   string
	}{
		{"missing product url", func(e *config.EmbedderConfig) { e.ProductFURL = "" }, "EMBED_PRODUCT_FURL"},
		{"invalid product url", func(e *config.EmbedderConfig) { e.ProductFURL = "not-a-url" }, "EMBED_PRODUCT_FURL"},
		{"missing query url", func(e *config.EmbedderConfig) { e.QueryFURL = "" }, "EMBED_QUERY_FURL"},
		{"invalid query url", func(e *config.EmbedderConfig) { e.QueryFURL = "ftp://host/x" }, "EMBED_QUERY_FURL"},
		{"empty key", func(e *config.EmbedderConfig) { e.APIKey = "" }, "EMBED_API_KEY"},
		{"non-allowlisted model", func(e *config.EmbedderConfig) { e.Model = "text-embedding-3-small" }, "GKG_EMBEDDER_MODEL"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := config.Config{}
			cfg.Embedder = base
			tc.mutate(&cfg.Embedder)
			_, err := embedderFromConfig(cfg)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want it to name %s", err, tc.want)
			}
		})
	}

	t.Run("unknown provider", func(t *testing.T) {
		cfg := config.Config{}
		cfg.Embedder = base
		cfg.Embedder.Provider = "gemini"
		if _, err := embedderFromConfig(cfg); err == nil {
			t.Fatal("unknown provider: want error")
		}
	})
}

func TestEmbedderFromConfigFurlsOK(t *testing.T) {
	cfg := config.Config{}
	cfg.Embedder = config.EmbedderConfig{
		Provider:    "furls",
		ProductFURL: "https://product.example/v1",
		QueryFURL:   "https://query.example/v1",
		APIKey:      "k",
		Dim:         768,
	}
	emb, err := embedderFromConfig(cfg)
	if err != nil {
		t.Fatalf("embedderFromConfig: %v", err)
	}
	if emb.Name() != "furls" {
		t.Errorf("Name = %q, want furls", emb.Name())
	}
	if got := emb.Space(); got != "furls|gemini-embedding-2|768" {
		t.Errorf("Space = %q, want furls|gemini-embedding-2|768", got)
	}
}

func TestConverterFromConfigOff(t *testing.T) {
	cfg := config.Config{}
	if c, err := converterFromConfig(cfg); err != nil || c != nil {
		t.Fatalf("converterFromConfig(zero) = %v, %v; want nil, nil", c, err)
	}
	cfg.Converter.Mode = "off"
	if c, err := converterFromConfig(cfg); err != nil || c != nil {
		t.Fatalf("converterFromConfig(off) = %v, %v; want nil, nil", c, err)
	}
}

func TestConverterFromConfigImages(t *testing.T) {
	for _, enabled := range []bool{false, true} {
		name := "disabled"
		if enabled {
			name = "enabled"
		}
		t.Run(name, func(t *testing.T) {
			var calls atomic.Int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				if r.Method != http.MethodPost || r.URL.Path != "/v1/convert/file" {
					t.Error("unexpected docling request")
				}
				io.WriteString(w, `{"status":"success","document":{"md_content":"converted"}}`)
			}))
			defer srv.Close()
			cfg := config.Config{Converter: config.ConverterConfig{Mode: "docling", URL: srv.URL, Images: enabled}}
			c, err := converterFromConfig(cfg)
			if err != nil {
				t.Fatal(err)
			}
			md, err := c.Convert(context.Background(), []byte("\x89PNG\r\n\x1a\n"), "page.png", "application/octet-stream")
			if enabled {
				if err != nil || md != "converted" || calls.Load() != 1 {
					t.Fatalf("opt-in image must reach docling: calls=%d, err=%v", calls.Load(), err)
				}
			} else if err == nil || !strings.Contains(err.Error(), "GKG_CONVERTER_IMAGES") || calls.Load() != 0 {
				t.Fatalf("disabled image must fail before docling: calls=%d, err=%v", calls.Load(), err)
			}
		})
	}
}

func TestConverterFromConfigValidation(t *testing.T) {
	cfg := config.Config{}
	cfg.Converter.Mode = "docling"
	if _, err := converterFromConfig(cfg); err == nil || !strings.Contains(err.Error(), "GKG_CONVERTER_URL") {
		t.Fatalf("empty URL err = %v, want it to name GKG_CONVERTER_URL", err)
	}
	cfg.Converter.URL = "not-a-url"
	if _, err := converterFromConfig(cfg); err == nil || !strings.Contains(err.Error(), "GKG_CONVERTER_URL") {
		t.Fatalf("invalid URL err = %v, want it to name GKG_CONVERTER_URL", err)
	}
	cfg.Converter.URL = "http://localhost:5001"
	c, err := converterFromConfig(cfg)
	if err != nil || c == nil {
		t.Fatalf("converterFromConfig(docling) = %v, %v; want a converter", c, err)
	}

	cfg.Converter.Mode = "pandoc"
	if _, err := converterFromConfig(cfg); err == nil || !strings.Contains(err.Error(), "pandoc") {
		t.Fatalf("unknown mode err = %v, want it to name the mode", err)
	}
}

// TestIngestArgSource pins the -source override: an explicit source is
// applied to every kind (file, URL, stdin) and the empty default keeps
// today's identities (file basename, empty for URL/text).
func TestIngestArgSource(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "notes.md")
	content := []byte("# notes\nAda Lovelace wrote the first algorithm.\n")
	if err := os.WriteFile(path, content, 0o600); err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name       string
		arg        string
		source     string
		wantKind   string
		wantSource string
		wantURL    string
	}{
		{"url explicit source", "https://example.com/x", "web/article", "url", "web/article", "https://example.com/x"},
		{"url default source", "https://example.com/x", "", "url", "", "https://example.com/x"},
		{"file explicit source", path, "renamed.md", "file", "renamed.md", ""},
		{"file default source", path, "", "file", "notes.md", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			in, err := ingestArg(tc.arg, tc.source)
			if err != nil {
				t.Fatalf("ingestArg: %v", err)
			}
			if in.Kind != tc.wantKind {
				t.Errorf("Kind = %q, want %q", in.Kind, tc.wantKind)
			}
			if in.Source != tc.wantSource {
				t.Errorf("Source = %q, want %q", in.Source, tc.wantSource)
			}
			if tc.wantURL != "" && in.URL != tc.wantURL {
				t.Errorf("URL = %q, want %q", in.URL, tc.wantURL)
			}
			if tc.wantKind == "file" && string(in.Bytes) != string(content) {
				t.Errorf("Bytes = %q, want the file content", in.Bytes)
			}
		})
	}

	stdin, err := os.CreateTemp(dir, "stdin")
	if err != nil {
		t.Fatal(err)
	}
	defer stdin.Close()
	if _, err := stdin.WriteString("Ada Lovelace wrote the first algorithm."); err != nil {
		t.Fatal(err)
	}
	if _, err := stdin.Seek(0, 0); err != nil {
		t.Fatal(err)
	}
	orig := os.Stdin
	os.Stdin = stdin
	t.Cleanup(func() { os.Stdin = orig })

	in, err := ingestArg("-", "notes/ada")
	if err != nil {
		t.Fatalf("ingestArg(stdin): %v", err)
	}
	if in.Kind != "text" || in.Source != "notes/ada" {
		t.Errorf("stdin: Kind = %q Source = %q, want text with the explicit source", in.Kind, in.Source)
	}
	if !strings.Contains(in.Text, "Ada Lovelace") {
		t.Errorf("stdin Text = %q, want the piped content", in.Text)
	}
}
