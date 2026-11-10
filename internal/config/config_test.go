package config_test

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/alessiosavi/go-knowledge-graph/internal/config"
)

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
	if cfg.Ingest.MaxRedirects != 5 {
		t.Errorf("Ingest.MaxRedirects = %d, want 5", cfg.Ingest.MaxRedirects)
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
		{"GKG_INGEST_MAX_REDIRECTS", "3", 3, func(c config.Config) any { return c.Ingest.MaxRedirects }},
		{"GKG_LABELS_PRESETS_DIR", "custom", "custom", func(c config.Config) any { return c.Labels.PresetsDir }},
		{"GKG_LIMITS_CYPHER_ROWS", "50", 50, func(c config.Config) any { return c.Limits.CypherRows }},
		{"GKG_LIMITS_CYPHER_TIMEOUT_MS", "2500", 2500, func(c config.Config) any { return c.Limits.CypherTimeoutMS }},
		{"GKG_AGENT_ENABLED", "true", true, func(c config.Config) any { return c.Agent.Enabled }},
		{"GKG_AGENT_BASE_URL", "https://example/v1", "https://example/v1", func(c config.Config) any { return c.Agent.BaseURL }},
		{"GKG_AGENT_API_KEY", "ak", "ak", func(c config.Config) any { return c.Agent.APIKey }},
		{"GKG_AGENT_MODEL", "am", "am", func(c config.Config) any { return c.Agent.Model }},
		{"GKG_AGENT_MAX_STEPS", "3", 3, func(c config.Config) any { return c.Agent.MaxSteps }},
		{"GKG_AGENT_TIMEOUT_S", "30", 30, func(c config.Config) any { return c.Agent.TimeoutS }},
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
	if !cfg.Agent.Stream {
		t.Errorf("Agent.Stream = false, want the streaming client enabled by default")
	}
	if cfg.Agent.BaseURL != "https://api.openai.com/v1" {
		t.Errorf("Agent.BaseURL = %q, want the OpenAI-compatible default", cfg.Agent.BaseURL)
	}
	if cfg.Agent.Model != "deepseek-v4.1-flash" {
		t.Errorf("Agent.Model = %q, want the default model", cfg.Agent.Model)
	}
	if cfg.Agent.MaxSteps != 6 {
		t.Errorf("Agent.MaxSteps = %d, want 6", cfg.Agent.MaxSteps)
	}
	if cfg.Agent.TimeoutS != 60 {
		t.Errorf("Agent.TimeoutS = %d, want 60", cfg.Agent.TimeoutS)
	}
}
