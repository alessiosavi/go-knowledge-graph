package config_test

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/alessiosavi/go-knowledge-graph/internal/config"
	"go.yaml.in/yaml/v4"
)

func TestProductTextConfig(t *testing.T) {
	values := map[string]string{
		"GKG_PRODUCT_TEXT_SEARCH":           "true",
		"GKG_PRODUCT_TEXT_FLOOR":            "0",
		"GKG_PRODUCT_TEXT_SEARCH_TIMEOUT":   "750ms",
		"GKG_PRODUCT_TEXT_AUTO_TIMEOUT":     "125ms",
		"GKG_PRODUCT_TEXT_EMBED":            "pause",
		"GKG_PRODUCT_TEXT_EMBED_PAUSE":      "125ms",
		"GKG_PRODUCT_TEXT_EMBED_MAX_INGEST": "0",
		"GKG_PRODUCT_TEXT_MAX_VECTORS":      "123",
	}
	for key := range values {
		t.Setenv(key, "")
		if err := os.Unsetenv(key); err != nil {
			t.Fatal(err)
		}
	}
	cfg, err := config.Load("")
	if err != nil {
		t.Fatal(err)
	}
	if got := cfg.ProductText; got.Search || got.Embed != "run" || got.EmbedPause != 250*time.Millisecond ||
		got.EmbedMaxIngest != 2 || got.MaxVectors != 65536 || got.SearchTimeout != 1500*time.Millisecond ||
		got.AutoTimeout != 300*time.Millisecond || got.SimilarityFloor() != .72 {
		t.Fatalf("defaults = %+v", got)
	}
	for key, val := range values {
		t.Setenv(key, val)
	}
	cfg, err = config.Load("")
	if err != nil {
		t.Fatal(err)
	}
	if got := cfg.ProductText; !got.Search || got.Embed != "pause" || got.EmbedPause != 125*time.Millisecond ||
		got.EmbedMaxIngest != 0 || got.MaxVectors != 123 || got.SearchTimeout != 750*time.Millisecond ||
		got.AutoTimeout != 125*time.Millisecond || got.SimilarityFloor() != 0 {
		t.Fatalf("overrides = %+v", got)
	}
	for value, want := range map[string]bool{"on": true, "off": false} {
		t.Setenv("GKG_PRODUCT_TEXT_SEARCH", value)
		cfg, err := config.Load("")
		if err != nil || cfg.ProductText.Search != want {
			t.Errorf("search=%s: cfg=%+v, err=%v", value, cfg.ProductText, err)
		}
	}
	for key, value := range map[string]string{
		"GKG_PRODUCT_TEXT_EMBED": "stop", "GKG_PRODUCT_TEXT_EMBED_PAUSE": "-1s",
		"GKG_PRODUCT_TEXT_EMBED_MAX_INGEST": "-1", "GKG_PRODUCT_TEXT_MAX_VECTORS": "-1",
		"GKG_PRODUCT_TEXT_FLOOR": "NaN", "GKG_PRODUCT_TEXT_SEARCH_TIMEOUT": "0s",
		"GKG_PRODUCT_TEXT_AUTO_TIMEOUT": "-1s",
	} {
		t.Run(key, func(t *testing.T) {
			t.Setenv(key, value)
			if _, err := config.Load(""); err == nil || !strings.Contains(err.Error(), key) {
				t.Errorf("invalid %s: %v", key, err)
			}
		})
	}
	t.Setenv("GKG_PRODUCT_TEXT_MAX_VECTORS", "0")
	if _, err := config.Load(""); err == nil || !strings.Contains(err.Error(), "GKG_PRODUCT_TEXT_MAX_VECTORS") {
		t.Fatalf("zero admission limit: %v", err)
	}
}

func TestWriteGateConfig(t *testing.T) {
	for _, key := range []string{"GKG_FACTS_REQUIRE_CONFIRMATION", "GKG_FACTS_ALLOW_UNVERIFIED_READS", "GKG_DOMAIN_TAU", "GKG_DOMAIN_DELTA", "GKG_DOMAIN_TAU_OVERRIDE", "GKG_DOMAIN_SEED_FILE"} {
		t.Setenv(key, "")
		if err := os.Unsetenv(key); err != nil {
			t.Fatal(err)
		}
	}
	t.Run("defaults", func(t *testing.T) {
		cfg, err := config.Load("")
		if err != nil {
			t.Fatal(err)
		}
		if cfg.Facts != (config.FactsConfig{}) || cfg.Domain != (config.DomainConfig{Tau: 0.05, Delta: 0.02, TauOverride: 0.15}) {
			t.Errorf("gate defaults: facts=%+v domain=%+v", cfg.Facts, cfg.Domain)
		}
	})
	p := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(p, []byte("judge:\n  mode: all\nfacts:\n  require_confirmation: true\n  allow_unverified_reads: true\ndomain:\n  tau: 0.1\n  delta: 0.03\n  tau_override: 0.2\n  seed_file: acme.json\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Run("yaml", func(t *testing.T) {
		cfg, err := config.Load(p)
		if err != nil {
			t.Fatal(err)
		}
		if cfg.Facts != (config.FactsConfig{RequireConfirmation: true, AllowUnverifiedReads: true}) || cfg.Domain != (config.DomainConfig{Tau: 0.1, Delta: 0.03, TauOverride: 0.2, SeedFile: "acme.json"}) {
			t.Errorf("gate YAML: facts=%+v domain=%+v", cfg.Facts, cfg.Domain)
		}
	})
	t.Run("environment overrides yaml", func(t *testing.T) {
		for key, value := range map[string]string{
			"GKG_FACTS_REQUIRE_CONFIRMATION": "false", "GKG_FACTS_ALLOW_UNVERIFIED_READS": "false",
			"GKG_DOMAIN_TAU": "0.12", "GKG_DOMAIN_DELTA": "0.04", "GKG_DOMAIN_TAU_OVERRIDE": "0.25", "GKG_DOMAIN_SEED_FILE": "detect-all",
		} {
			t.Setenv(key, value)
		}
		cfg, err := config.Load(p)
		if err != nil {
			t.Fatal(err)
		}
		if cfg.Facts != (config.FactsConfig{}) || cfg.Domain != (config.DomainConfig{Tau: 0.12, Delta: 0.04, TauOverride: 0.25, SeedFile: "detect-all"}) {
			t.Errorf("gate environment: facts=%+v domain=%+v", cfg.Facts, cfg.Domain)
		}
	})
}

func TestLoadDefaults(t *testing.T) {
	cfg, err := config.Load("")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Extractor.Mode != "sidecar" {
		t.Errorf("Extractor.Mode = %q, want sidecar", cfg.Extractor.Mode)
	}
	if cfg.Extractor.Sidecar.URL != "http://localhost:8090" {
		t.Errorf("Extractor.Sidecar.URL differs from the localhost default")
	}
	if cfg.Ingest.StoreFloor != 0.15 {
		t.Errorf("Ingest.StoreFloor = %v, want 0.15", cfg.Ingest.StoreFloor)
	}
	if cfg.Ingest.AssertFloor != 0.50 {
		t.Errorf("Ingest.AssertFloor = %v, want 0.50", cfg.Ingest.AssertFloor)
	}
	if cfg.Judge.VerifyFloor != 0.35 {
		t.Errorf("Judge.VerifyFloor = %v, want 0.35", cfg.Judge.VerifyFloor)
	}
	if cfg.Ingest.ChunkDedup != "near" {
		t.Errorf("Ingest.ChunkDedup = %q, want %q", cfg.Ingest.ChunkDedup, "near")
	}
	if cfg.Ingest.JaccardThreshold != 0.85 {
		t.Errorf("Ingest.JaccardThreshold = %v, want 0.85", cfg.Ingest.JaccardThreshold)
	}
	if cfg.Judge.Mode != "off" {
		t.Errorf("Judge.Mode = %q, want %q", cfg.Judge.Mode, "off")
	}
	if cfg.Ingest.FetchTimeoutS != 30 {
		t.Errorf("Ingest.FetchTimeoutS = %d, want 30", cfg.Ingest.FetchTimeoutS)
	}
	if cfg.Ingest.FetchProxy != "" {
		t.Error("Ingest.FetchProxy should default to direct fetching")
	}
	if cfg.Ingest.MaxRedirects != 5 {
		t.Errorf("Ingest.MaxRedirects = %d, want 5", cfg.Ingest.MaxRedirects)
	}
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Ingest.SpoolDir != filepath.Join(home, ".local", "share", "gkg", "ingest-spool") || cfg.Ingest.SpoolMaxCount != 1024 || cfg.Ingest.SpoolMaxBytes != 1<<30 || cfg.Extractor.Sidecar.TimeoutS != 300 {
		t.Fatal("spool/sidecar defaults differ")
	}
}

func TestDurabilityConfigValidation(t *testing.T) {
	for _, key := range []string{"GKG_INGEST_SPOOL_MAX_COUNT", "GKG_INGEST_SPOOL_MAX_BYTES", "GKG_EXTRACTOR_SIDECAR_TIMEOUT_S"} {
		for _, value := range []string{"0", "-1", "invalid"} {
			t.Run(key+"/"+value, func(t *testing.T) {
				t.Setenv(key, value)
				if _, err := config.Load(""); err == nil || !strings.Contains(err.Error(), key) {
					t.Fatalf("want error naming %s: %v", key, err)
				}
			})
		}
	}
}

func TestIngestFetchProxyRejectsInvalidURLWithoutCredentials(t *testing.T) {
	const key = "GKG_INGEST_FETCH_PROXY"
	for _, tc := range []struct {
		name, value, problem string
	}{
		{"scheme", "ftp://proxy-user:proxy-password@proxy.example", "http or https"},
		{"missing host", "http://proxy-user:proxy-password@", "host"},
		{"missing hostname", "http://proxy-user:proxy-password@:8080", "host"},
		{"unparsable", "https://proxy-user:proxy-password@proxy.example:%GG", "URL"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv(key, tc.value)
			_, err := config.Load("")
			if err == nil || !strings.Contains(err.Error(), key) || !strings.Contains(err.Error(), tc.problem) {
				t.Fatalf("Load error = %v, want a keyed %s error", err, tc.problem)
			}
			if strings.Contains(err.Error(), "proxy-user") || strings.Contains(err.Error(), "proxy-password") {
				t.Errorf("Load error exposes proxy credentials: %v", err)
			}
		})
	}
}

func TestIngestFetchProxyYAMLAndSafeDumps(t *testing.T) {
	const proxy = "https://proxy-user:proxy-password@proxy.example:8443"
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte("ingest:\n  fetch_proxy: "+proxy+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(path)
	if err != nil || cfg.Ingest.FetchProxy != proxy {
		t.Fatalf("Load did not retain ingest.fetch_proxy: %v", err)
	}
	jsonDump, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	yamlDump, err := yaml.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	for _, dump := range []string{string(jsonDump), string(yamlDump), fmt.Sprintf("%+v", cfg), fmt.Sprintf("%#v", cfg)} {
		if strings.Contains(dump, "proxy-user") || strings.Contains(dump, "proxy-password") {
			t.Error("configuration dump contains proxy credentials")
		}
	}
	t.Setenv("GKG_INGEST_FETCH_PROXY", "")
	cfg, err = config.Load(path)
	if err != nil || cfg.Ingest.FetchProxy != "" {
		t.Fatalf("empty environment override did not select direct fetching: %v", err)
	}
}

func TestReadTimeout(t *testing.T) {
	t.Setenv("GKG_LIMITS_READ_TIMEOUT_MS", "")
	if err := os.Unsetenv("GKG_LIMITS_READ_TIMEOUT_MS"); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load("")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Limits.ReadTimeoutMS != 15000 || cfg.Limits.ReadTimeout() != 15*time.Second {
		t.Errorf("default limits = %+v, want a 15s read timeout", cfg.Limits)
	}

	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte("limits:\n  read_timeout_ms: 750\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err = config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := cfg.Limits.ReadTimeout(); got != 750*time.Millisecond {
		t.Errorf("YAML read timeout = %v, want 750ms", got)
	}

	for _, value := range []string{"0", "-1"} {
		t.Setenv("GKG_LIMITS_READ_TIMEOUT_MS", value)
		cfg, err := config.Load(path)
		if err != nil {
			t.Fatal(err)
		}
		if got := cfg.Limits.ReadTimeout(); got != 15*time.Second {
			t.Errorf("read timeout with env = %s is %v, want 15s", value, got)
		}
	}
}

func TestConverterImagesConfig(t *testing.T) {
	for _, key := range []string{"GKG_CONVERTER_IMAGES", "GKG_CONVERTER_OCR_ENGINE", "GKG_CONVERTER_OCR_LANG", "GKG_CONVERTER_MAX_IMAGE_MP"} {
		t.Setenv(key, "")
		if err := os.Unsetenv(key); err != nil {
			t.Fatal(err)
		}
	}
	t.Run("default", func(t *testing.T) {
		cfg, err := config.Load("")
		if err != nil || !cfg.Converter.Images || cfg.Converter.OCREngine != "tesseract" || cfg.Converter.OCRLang != "eng" || cfg.Converter.MaxImageMP != 12.5 {
			t.Fatalf("want images enabled, tesseract, eng, 12.5 MP: %+v, %v", cfg.Converter, err)
		}
	})
	t.Run("env overrides yaml", func(t *testing.T) {
		p := filepath.Join(t.TempDir(), "config.yaml")
		if err := os.WriteFile(p, []byte("converter:\n  images: true\n  ocr_engine: tesseract\n  max_image_mp: 12.5\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		t.Setenv("GKG_CONVERTER_IMAGES", "false")
		t.Setenv("GKG_CONVERTER_OCR_ENGINE", "easyocr")
		t.Setenv("GKG_CONVERTER_OCR_LANG", "eng,ita")
		t.Setenv("GKG_CONVERTER_MAX_IMAGE_MP", "20.25")
		cfg, err := config.Load(p)
		if err != nil || cfg.Converter.Images || cfg.Converter.OCREngine != "easyocr" || cfg.Converter.OCRLang != "eng,ita" || cfg.Converter.MaxImageMP != 20.25 {
			t.Fatalf("env overrides not applied: %+v, %v", cfg.Converter, err)
		}
	})
	t.Run("yaml", func(t *testing.T) {
		p := filepath.Join(t.TempDir(), "config.yaml")
		if err := os.WriteFile(p, []byte("converter:\n  images: false\n  ocr_engine: easyocr\n  max_image_mp: 8.5\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		cfg, err := config.Load(p)
		if err != nil || cfg.Converter.Images || cfg.Converter.OCREngine != "easyocr" || cfg.Converter.MaxImageMP != 8.5 {
			t.Fatalf("yaml overrides not applied: %+v, %v", cfg.Converter, err)
		}
	})
}

func TestConverterImageValidation(t *testing.T) {
	for _, tc := range []struct{ key, value string }{
		{"GKG_CONVERTER_MAX_IMAGE_MP", "0"}, {"GKG_CONVERTER_MAX_IMAGE_MP", "-1"},
		{"GKG_CONVERTER_MAX_IMAGE_MP", "NaN"}, {"GKG_CONVERTER_MAX_IMAGE_MP", "+Inf"},
		{"GKG_CONVERTER_MAX_IMAGE_MP", "1e20"}, {"GKG_CONVERTER_MAX_IMAGE_MP", "invalid"},
		{"GKG_CONVERTER_OCR_ENGINE", ""}, {"GKG_CONVERTER_OCR_ENGINE", "  "},
	} {
		t.Run(tc.key+"/"+tc.value, func(t *testing.T) {
			t.Setenv(tc.key, tc.value)
			_, err := config.Load("")
			if err == nil || !strings.Contains(err.Error(), tc.key) {
				t.Fatalf("want error naming %s, got %v", tc.key, err)
			}
		})
	}
	for _, body := range []string{"max_image_mp: 0", "ocr_engine: ''"} {
		t.Run("yaml/"+body, func(t *testing.T) {
			p := filepath.Join(t.TempDir(), "config.yaml")
			if err := os.WriteFile(p, []byte("converter:\n  "+body+"\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			_, err := config.Load(p)
			if err == nil || !strings.Contains(err.Error(), "GKG_CONVERTER_") {
				t.Fatalf("want validation error, got %v", err)
			}
		})
	}
}

func TestEnvOverridesYAML(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "config.yaml")
	body := "server:\n  addr: \":9999\"\nmemgraph:\n  uri: bolt://from-yaml:7687\n"
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GKG_SERVER_ADDR", ":7777")

	cfg, err := config.Load(p)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Server.Addr != ":7777" {
		t.Errorf("Server.Addr = %q, want env override :7777", cfg.Server.Addr)
	}
	if cfg.Memgraph.URI != "bolt://from-yaml:7687" {
		t.Errorf("Memgraph.URI = %q, want yaml value bolt://from-yaml:7687", cfg.Memgraph.URI)
	}
}

func TestAPIKeyEnvException(t *testing.T) {
	t.Setenv("GKG_API_KEY", "sekret")
	cfg, err := config.Load("")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Server.APIKey != "sekret" {
		t.Errorf("Server.APIKey = %q, want GKG_API_KEY exception to map to Server.APIKey", cfg.Server.APIKey)
	}
}

// TestEnvNamesMirrorYAMLKeys pins the environment name of every leaf field:
// GKG_<SECTION>_<KEY> mirrors the snake_case yaml keys, so the openai
// section is GKG_EXTRACTOR_OPENAI_* (not ...OPEN_AI_*).
func TestEnvNamesMirrorYAMLKeys(t *testing.T) {
	cases := []struct {
		key  string
		val  string
		want any
		get  func(config.Config) any
	}{
		{"GKG_MEMGRAPH_URI", "bolt://example:1111", "bolt://example:1111", func(c config.Config) any { return c.Memgraph.URI }},
		{"GKG_MEMGRAPH_USER", "memuser", "memuser", func(c config.Config) any { return c.Memgraph.User }},
		{"GKG_MEMGRAPH_PASSWORD", "mempass", "mempass", func(c config.Config) any { return c.Memgraph.Password }},
		{"GKG_SERVER_ADDR", ":1234", ":1234", func(c config.Config) any { return c.Server.Addr }},
		{"GKG_SERVER_API_KEY", "sk", "sk", func(c config.Config) any { return c.Server.APIKey }},
		{"GKG_EXTRACTOR_MODE", "mock", "mock", func(c config.Config) any { return c.Extractor.Mode }},
		{"GKG_EXTRACTOR_OPENAI_BASE_URL", "https://example/v1", "https://example/v1", func(c config.Config) any { return c.Extractor.OpenAI.BaseURL }},
		{"GKG_EXTRACTOR_OPENAI_API_KEY", "ok", "ok", func(c config.Config) any { return c.Extractor.OpenAI.APIKey }},
		{"GKG_EXTRACTOR_OPENAI_MODEL", "om", "om", func(c config.Config) any { return c.Extractor.OpenAI.Model }},
		{"GKG_EXTRACTOR_SIDECAR_URL", "http://localhost:9999", "http://localhost:9999", func(c config.Config) any { return c.Extractor.Sidecar.URL }},
		{"GKG_EXTRACTOR_SIDECAR_MODEL", "glinerel-314m", "glinerel-314m", func(c config.Config) any { return c.Extractor.Sidecar.Model }},
		{"GKG_EXTRACTOR_SIDECAR_IMAGE", "synthetic:tag", "synthetic:tag", func(c config.Config) any { return c.Extractor.Sidecar.Image }},
		{"GKG_EXTRACTOR_SIDECAR_TIMEOUT_S", "42", 42, func(c config.Config) any { return c.Extractor.Sidecar.TimeoutS }},
		{"GKG_INGEST_SPOOL_DIR", "custom-spool", "custom-spool", func(c config.Config) any { return c.Ingest.SpoolDir }},
		{"GKG_INGEST_SPOOL_MAX_COUNT", "12", 12, func(c config.Config) any { return c.Ingest.SpoolMaxCount }},
		{"GKG_INGEST_SPOOL_MAX_BYTES", "2048", 2048, func(c config.Config) any { return c.Ingest.SpoolMaxBytes }},
		{"GKG_EMBEDDER_ENABLED", "true", true, func(c config.Config) any { return c.Embedder.Enabled }},
		{"GKG_EMBEDDER_BASE_URL", "https://example/v1", "https://example/v1", func(c config.Config) any { return c.Embedder.BaseURL }},
		{"GKG_EMBEDDER_API_KEY", "ek", "ek", func(c config.Config) any { return c.Embedder.APIKey }},
		{"GKG_EMBEDDER_MODEL", "em", "em", func(c config.Config) any { return c.Embedder.Model }},
		{"GKG_EMBEDDER_DIM", "1536", 1536, func(c config.Config) any { return c.Embedder.Dim }},
		{"GKG_EMBEDDER_PROVIDER", "furls", "furls", func(c config.Config) any { return c.Embedder.Provider }},
		{"GKG_EMBEDDER_QUERY_FURL", "https://query.example/v1", "https://query.example/v1", func(c config.Config) any { return c.Embedder.QueryFURL }},
		{"GKG_EMBEDDER_PRODUCT_FURL", "https://product.example/v1", "https://product.example/v1", func(c config.Config) any { return c.Embedder.ProductFURL }},
		{"GKG_EMBEDDER_BATCH_SIZE", "16", 16, func(c config.Config) any { return c.Embedder.BatchSize }},
		{"GKG_CONVERTER_MODE", "docling", "docling", func(c config.Config) any { return c.Converter.Mode }},
		{"GKG_CONVERTER_URL", "http://docling:5001", "http://docling:5001", func(c config.Config) any { return c.Converter.URL }},
		{"GKG_CONVERTER_API_KEY", "ck", "ck", func(c config.Config) any { return c.Converter.APIKey }},
		{"GKG_CONVERTER_TIMEOUT_S", "42", 42, func(c config.Config) any { return c.Converter.TimeoutS }},
		{"GKG_CONVERTER_MAX_RESPONSE_MB", "64", 64, func(c config.Config) any { return c.Converter.MaxResponseMB }},
		{"GKG_JUDGE_MODE", "sample", "sample", func(c config.Config) any { return c.Judge.Mode }},
		{"GKG_JUDGE_VERIFY_FLOOR", "0.4", 0.4, func(c config.Config) any { return c.Judge.VerifyFloor }},
		{"GKG_JUDGE_SAMPLE_PCT", "25", 25.0, func(c config.Config) any { return c.Judge.SamplePct }},
		{"GKG_JUDGE_BASE_URL", "https://example/v1", "https://example/v1", func(c config.Config) any { return c.Judge.BaseURL }},
		{"GKG_JUDGE_API_KEY", "jk", "jk", func(c config.Config) any { return c.Judge.APIKey }},
		{"GKG_JUDGE_MODEL", "jm", "jm", func(c config.Config) any { return c.Judge.Model }},
		{"GKG_INGEST_MAX_UPLOAD_MB", "7", 7, func(c config.Config) any { return c.Ingest.MaxUploadMB }},
		{"GKG_INGEST_MAX_URL_MB", "8", 8, func(c config.Config) any { return c.Ingest.MaxURLMB }},
		{"GKG_INGEST_MAX_CHUNKS", "9", 9, func(c config.Config) any { return c.Ingest.MaxChunks }},
		{"GKG_INGEST_STORE_FLOOR", "0.2", 0.2, func(c config.Config) any { return c.Ingest.StoreFloor }},
		{"GKG_INGEST_ASSERT_FLOOR", "0.6", 0.6, func(c config.Config) any { return c.Ingest.AssertFloor }},
		{"GKG_INGEST_CHUNK_WORDS", "123", 123, func(c config.Config) any { return c.Ingest.ChunkWords }},
		{"GKG_INGEST_OVERLAP_SENTENCES", "2", 2, func(c config.Config) any { return c.Ingest.OverlapSentences }},
		{"GKG_INGEST_CHUNK_DEDUP", "exact", "exact", func(c config.Config) any { return c.Ingest.ChunkDedup }},
		{"GKG_INGEST_JACCARD_THRESHOLD", "0.9", 0.9, func(c config.Config) any { return c.Ingest.JaccardThreshold }},
		{"GKG_INGEST_FETCH_TIMEOUT_S", "11", 11, func(c config.Config) any { return c.Ingest.FetchTimeoutS }},
		{"GKG_INGEST_FETCH_PROXY", "http://proxy.example:8080", "http://proxy.example:8080", func(c config.Config) any { return c.Ingest.FetchProxy }},
		{"GKG_INGEST_MAX_REDIRECTS", "3", 3, func(c config.Config) any { return c.Ingest.MaxRedirects }},
		{"GKG_LABELS_PRESETS_DIR", "custom", "custom", func(c config.Config) any { return c.Labels.PresetsDir }},
		{"GKG_LIMITS_CYPHER_ROWS", "50", 50, func(c config.Config) any { return c.Limits.CypherRows }},
		{"GKG_LIMITS_CYPHER_TIMEOUT_MS", "2500", 2500, func(c config.Config) any { return c.Limits.CypherTimeoutMS }},
		{"GKG_LIMITS_READ_TIMEOUT_MS", "4500", 4500, func(c config.Config) any { return c.Limits.ReadTimeoutMS }},
		{"GKG_AGENT_ENABLED", "true", true, func(c config.Config) any { return c.Agent.Enabled }},
		{"GKG_AGENT_BASE_URL", "https://example/v1", "https://example/v1", func(c config.Config) any { return c.Agent.BaseURL }},
		{"GKG_AGENT_API_KEY", "ak", "ak", func(c config.Config) any { return c.Agent.APIKey }},
		{"GKG_AGENT_MODEL", "am", "am", func(c config.Config) any { return c.Agent.Model }},
		{"GKG_AGENT_MAX_STEPS", "3", 3, func(c config.Config) any { return c.Agent.MaxSteps }},
		{"GKG_AGENT_TIMEOUT_S", "30", 30, func(c config.Config) any { return c.Agent.TimeoutS }},
		{"GKG_AGENT_CONTEXT_BUDGET_KB", "64", 64, func(c config.Config) any { return c.Agent.ContextBudgetKB }},
		{"GKG_AGENT_TOP_K_ENABLED", "true", true, func(c config.Config) any { return c.Agent.TopKEnabled }},
		{"GKG_AGENT_STREAM", "false", false, func(c config.Config) any { return c.Agent.Stream }},
	}
	for _, tc := range cases {
		t.Run(tc.key, func(t *testing.T) {
			t.Setenv(tc.key, tc.val)
			cfg, err := config.Load("")
			if err != nil {
				t.Fatalf("Load: %v", err)
			}
			if got := tc.get(cfg); !reflect.DeepEqual(got, tc.want) {
				t.Errorf("%s: got %v, want %v", tc.key, got, tc.want)
			}
		})
	}
}

func TestEnvInvalidValueReturnsKeyedError(t *testing.T) {
	t.Setenv("GKG_INGEST_MAX_CHUNKS", "many")
	_, err := config.Load("")
	if err == nil || !strings.Contains(err.Error(), "GKG_INGEST_MAX_CHUNKS") {
		t.Fatalf("want error naming GKG_INGEST_MAX_CHUNKS, got %v", err)
	}
}

func TestUnknownYAMLKeyRejected(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(p, []byte("server:\n  adddr: \":1\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := config.Load(p); err == nil {
		t.Fatal("want error for unknown yaml key")
	}
}

func TestMissingConfigFileErrors(t *testing.T) {
	if _, err := config.Load(filepath.Join(t.TempDir(), "absent.yaml")); err == nil {
		t.Fatal("want error for a non-empty path that does not exist")
	}
}

func TestEmptyYAMLKeepsDefaults(t *testing.T) {
	for name, body := range map[string]string{
		"empty":   "",
		"comment": "# nothing but a comment\n",
	} {
		t.Run(name, func(t *testing.T) {
			p := filepath.Join(t.TempDir(), "config.yaml")
			if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
				t.Fatal(err)
			}
			cfg, err := config.Load(p)
			if err != nil {
				t.Fatalf("Load(empty yaml): %v", err)
			}
			if cfg.Ingest.StoreFloor != 0.15 || cfg.Judge.Mode != "off" {
				t.Errorf("defaults not kept for empty yaml: %+v", cfg.Ingest)
			}
		})
	}
}

func TestAPIKeyExceptionWinsOverGenericEnv(t *testing.T) {
	t.Setenv("GKG_SERVER_API_KEY", "generic")
	t.Setenv("GKG_API_KEY", "exception")
	cfg, err := config.Load("")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Server.APIKey != "exception" {
		t.Errorf("Server.APIKey = %q, want GKG_API_KEY exception to win", cfg.Server.APIKey)
	}
}

func TestEmbedderProviderDefaultsAndOverride(t *testing.T) {
	cfg, err := config.Load("")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Embedder.Provider != "" {
		t.Errorf("Embedder.Provider = %q, want the openai default (empty)", cfg.Embedder.Provider)
	}
	t.Setenv("GKG_EMBEDDER_PROVIDER", "furls")
	cfg, err = config.Load("")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Embedder.Provider != "furls" {
		t.Errorf("Embedder.Provider = %q, want the env override furls", cfg.Embedder.Provider)
	}
}

func TestEmbedderEnvExceptionsFurlsOnly(t *testing.T) {
	t.Setenv("EMBED_API_KEY", "embed-secret")
	t.Setenv("EMBED_QUERY_FURL", "https://query.example/v1")
	t.Setenv("EMBED_PRODUCT_FURL", "https://product.example/v1")

	t.Setenv("GKG_EMBEDDER_PROVIDER", "openai")
	cfg, err := config.Load("")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Embedder.APIKey != "" || cfg.Embedder.QueryFURL != "" || cfg.Embedder.ProductFURL != "" {
		t.Errorf("openai run picked up EMBED_* values: %+v", cfg.Embedder)
	}

	t.Setenv("GKG_EMBEDDER_PROVIDER", "furls")
	cfg, err = config.Load("")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Embedder.APIKey != "embed-secret" || cfg.Embedder.QueryFURL != "https://query.example/v1" || cfg.Embedder.ProductFURL != "https://product.example/v1" {
		t.Errorf("furls run did not apply EMBED_* exceptions: %+v", cfg.Embedder)
	}
}

func TestEmbedderEnvExceptionEmptyPlaceholder(t *testing.T) {
	t.Setenv("GKG_EMBEDDER_PROVIDER", "furls")
	t.Setenv("GKG_EMBEDDER_API_KEY", "generic")
	t.Setenv("EMBED_API_KEY", "")
	cfg, err := config.Load("")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Embedder.APIKey != "generic" {
		t.Errorf("Embedder.APIKey = %q, want the GKG_ value to survive an empty EMBED_API_KEY placeholder", cfg.Embedder.APIKey)
	}
}

func TestEmbedderEnvExceptionPrecedence(t *testing.T) {
	t.Setenv("GKG_EMBEDDER_PROVIDER", "furls")
	t.Setenv("GKG_EMBEDDER_API_KEY", "generic")
	t.Setenv("EMBED_API_KEY", "exception")
	cfg, err := config.Load("")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Embedder.APIKey != "exception" {
		t.Errorf("Embedder.APIKey = %q, want the non-empty EMBED_API_KEY to win", cfg.Embedder.APIKey)
	}
}

func TestEmbedderBatchSizeEnvException(t *testing.T) {
	t.Setenv("EMBED_BATCH_SIZE", "16")
	t.Setenv("GKG_EMBEDDER_PROVIDER", "openai")
	cfg, err := config.Load("")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Embedder.BatchSize != 0 {
		t.Errorf("openai run picked up EMBED_BATCH_SIZE: %d", cfg.Embedder.BatchSize)
	}

	t.Setenv("GKG_EMBEDDER_PROVIDER", "furls")
	cfg, err = config.Load("")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Embedder.BatchSize != 16 {
		t.Errorf("Embedder.BatchSize = %d, want the EMBED_BATCH_SIZE exception (16)", cfg.Embedder.BatchSize)
	}

	// An empty placeholder must not clobber the GKG_ value.
	t.Setenv("GKG_EMBEDDER_BATCH_SIZE", "8")
	t.Setenv("EMBED_BATCH_SIZE", "")
	cfg, err = config.Load("")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Embedder.BatchSize != 8 {
		t.Errorf("Embedder.BatchSize = %d, want the GKG_ value to survive an empty placeholder", cfg.Embedder.BatchSize)
	}

	// A malformed value is an actionable error naming the variable.
	t.Setenv("EMBED_BATCH_SIZE", "many")
	if _, err := config.Load(""); err == nil || !strings.Contains(err.Error(), "EMBED_BATCH_SIZE") {
		t.Fatalf("err = %v, want a parse error naming EMBED_BATCH_SIZE", err)
	}
}

func TestConverterDefaults(t *testing.T) {
	cfg, err := config.Load("")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Converter.Mode != "docling" {
		t.Errorf("Converter.Mode = %q, want docling", cfg.Converter.Mode)
	}
	if cfg.Converter.URL != "http://localhost:5001" {
		t.Errorf("Converter.URL differs from the localhost default")
	}
	if cfg.Converter.TimeoutS != 300 {
		t.Errorf("Converter.TimeoutS = %d, want 300", cfg.Converter.TimeoutS)
	}
	if cfg.Converter.MaxResponseMB != 32 {
		t.Errorf("Converter.MaxResponseMB = %d, want 32", cfg.Converter.MaxResponseMB)
	}
}

func TestConverterEnvOverrides(t *testing.T) {
	t.Setenv("GKG_CONVERTER_MODE", "docling")
	t.Setenv("GKG_CONVERTER_URL", "http://docling:5001")
	t.Setenv("GKG_CONVERTER_API_KEY", "ck")
	t.Setenv("GKG_CONVERTER_TIMEOUT_S", "42")
	t.Setenv("GKG_CONVERTER_MAX_RESPONSE_MB", "64")
	cfg, err := config.Load("")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Converter.Mode != "docling" || cfg.Converter.URL != "http://docling:5001" ||
		cfg.Converter.APIKey != "ck" || cfg.Converter.TimeoutS != 42 || cfg.Converter.MaxResponseMB != 64 {
		t.Errorf("Converter = %+v, want the env overrides applied", cfg.Converter)
	}
}

func TestAgentDefaults(t *testing.T) {
	cfg, err := config.Load("")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Agent.Enabled {
		t.Errorf("Agent.Enabled = true, want the disabled default")
	}
	if cfg.Agent.TopKEnabled {
		t.Error("sampling top-k must be opt-in")
	}
	if !cfg.Agent.Stream {
		t.Errorf("Agent.Stream = false, want the streaming client enabled by default")
	}
	if cfg.Agent.BaseURL != "https://api.openai.com/v1" {
		t.Errorf("Agent.BaseURL = %q, want the OpenAI-compatible default", cfg.Agent.BaseURL)
	}
	if cfg.Agent.Model != "deepseek-v4.1-flash" {
		t.Errorf("Agent.Model = %q, want the default model", cfg.Agent.Model)
	}
	if cfg.Agent.ContextBudgetKB != 256 {
		t.Errorf("Agent.ContextBudgetKB = %d, want 256", cfg.Agent.ContextBudgetKB)
	}
	if cfg.Agent.MaxSteps != 6 {
		t.Errorf("Agent.MaxSteps = %d, want 6", cfg.Agent.MaxSteps)
	}
	if cfg.Agent.TimeoutS != 60 {
		t.Errorf("Agent.TimeoutS = %d, want 60", cfg.Agent.TimeoutS)
	}
	if cfg.Agent.SearchProducts != 3 {
		t.Errorf("Agent.SearchProducts = %d, want 3", cfg.Agent.SearchProducts)
	}
}

func TestAgentContextBudgetYAML(t *testing.T) {
	p := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(p, []byte("agent:\n  context_budget_kb: 64\n"), 0600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(p)
	if err != nil || cfg.Agent.ContextBudgetKB != 64 {
		t.Fatalf("budget=%d err=%v", cfg.Agent.ContextBudgetKB, err)
	}
}

func TestAgentSearchProductsValidation(t *testing.T) {
	for _, tc := range []struct{ value, want string }{
		{"0", "config: GKG_AGENT_SEARCH_PRODUCTS must be between 1 and 20"},
		{"21", "config: GKG_AGENT_SEARCH_PRODUCTS must be between 1 and 20"},
		{"x", `config: env GKG_AGENT_SEARCH_PRODUCTS="x":`},
	} {
		t.Run(tc.value, func(t *testing.T) {
			t.Setenv("GKG_AGENT_SEARCH_PRODUCTS", tc.value)
			_, err := config.Load("")
			if err == nil || !strings.HasPrefix(err.Error(), tc.want) {
				t.Fatalf("Load error = %v, want prefix %q", err, tc.want)
			}
		})
	}
}

func TestAgentSearchProductsYAMLAndEnv(t *testing.T) {
	p := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(p, []byte("agent:\n  search_products: 20\n"), 0600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(p)
	if err != nil || cfg.Agent.SearchProducts != 20 {
		t.Fatalf("YAML search_products=%d err=%v, want 20", cfg.Agent.SearchProducts, err)
	}
	t.Setenv("GKG_AGENT_SEARCH_PRODUCTS", "5")
	cfg, err = config.Load(p)
	if err != nil || cfg.Agent.SearchProducts != 5 {
		t.Fatalf("env search_products=%d err=%v, want 5", cfg.Agent.SearchProducts, err)
	}
}

func TestRequireConfirmationNeedsJudgeAll(t *testing.T) {
	for _, mode := range []string{"off", "low-confidence", "sample", "", "all"} {
		t.Run(mode, func(t *testing.T) {
			t.Setenv("GKG_FACTS_REQUIRE_CONFIRMATION", "true")
			t.Setenv("GKG_JUDGE_MODE", mode)
			_, err := config.Load("")
			if mode == "all" {
				if err != nil {
					t.Fatal(err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), "facts.require_confirmation needs judge.mode=all") {
				t.Fatalf("mode %q: error = %v, want facts.require_confirmation needs judge.mode=all", mode, err)
			}
		})
	}
}

func TestStoreJudgeInputsConfig(t *testing.T) {
	for _, tc := range []struct {
		name, yaml, env, wantError string
		want                       bool
	}{
		{name: "default"},
		{name: "yaml", yaml: "judge:\n  store_inputs: true\n  mode: off\n", want: true},
		{name: "low-confidence", yaml: "judge:\n  mode: low-confidence\n", env: "true", want: true},
		{name: "sample", yaml: "judge:\n  mode: sample\n", env: "true", want: true},
		{name: "all", yaml: "judge:\n  mode: all\n", env: "true", want: true},
		{name: "env-wins", yaml: "judge:\n  store_inputs: true\n  mode: unknown\n", env: "false"},
		{name: "env-enables", yaml: "judge:\n  store_inputs: false\n  mode: unknown\n", env: "true", wantError: "judge.mode"},
		{name: "invalid-boolean", env: "maybe", wantError: "GKG_JUDGE_STORE_INPUTS"},
		{name: "unknown-mode", yaml: "judge:\n  mode: unknown\n", env: "true", wantError: "judge.mode"},
		{name: "confirmation", yaml: "judge:\n  mode: all\nfacts:\n  require_confirmation: true\n", env: "true", wantError: "facts.require_confirmation"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if tc.env != "" {
				t.Setenv("GKG_JUDGE_STORE_INPUTS", tc.env)
			}
			path := filepath.Join(t.TempDir(), "config.yaml")
			if err := os.WriteFile(path, []byte(tc.yaml), 0600); err != nil {
				t.Fatal(err)
			}
			cfg, err := config.Load(path)
			if tc.wantError == "" {
				if err != nil {
					t.Fatal(err)
				}
				if cfg.Judge.StoreInputs != tc.want {
					t.Errorf("judge.store_inputs = %v, want %v", cfg.Judge.StoreInputs, tc.want)
				}
			} else if err == nil || !strings.Contains(err.Error(), tc.wantError) {
				t.Fatalf("Load error = %v, want %s", err, tc.wantError)
			}
		})
	}
}

func TestHygieneStoplistFile(t *testing.T) {
	t.Setenv("GKG_INGEST_STOPLIST_FILE", "/private/stop.txt")
	cfg, err := config.Load("")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Ingest.StoplistFile != "/private/stop.txt" {
		t.Fatalf("ingest.stoplist_file was not loaded from the environment")
	}
}
