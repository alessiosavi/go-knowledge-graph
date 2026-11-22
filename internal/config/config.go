// Package config loads the gkg configuration from defaults, an optional
// YAML file and environment overrides.
//
// Precedence: environment (GKG_<SECTION>_<KEY>) > YAML > defaults.
// The HTTP API key is the documented exception: GKG_API_KEY maps to
// Server.APIKey. For the furls embedder (Embedder.Provider == "furls"),
// EMBED_QUERY_FURL, EMBED_PRODUCT_FURL and EMBED_API_KEY map to the
// Embedder section — non-empty values only.
package config

import (
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"time"

	"go.yaml.in/yaml/v4"
)

// Config is the root configuration.
type Config struct {
	Memgraph  MemgraphConfig  `yaml:"memgraph"`
	Server    ServerConfig    `yaml:"server"`
	Extractor ExtractorConfig `yaml:"extractor"`
	Embedder  EmbedderConfig  `yaml:"embedder"`
	Converter ConverterConfig `yaml:"converter"`
	Judge     JudgeConfig     `yaml:"judge"`
	GCP       GCPConfig       `yaml:"gcp"`
	Facts     FactsConfig     `yaml:"facts"`
	Domain    DomainConfig    `yaml:"domain"`
	Agent     AgentConfig     `yaml:"agent"`
	Ingest    IngestConfig    `yaml:"ingest"`
	Labels    LabelsConfig    `yaml:"labels"`
	Limits    LimitsConfig    `yaml:"limits"`
}

// MemgraphConfig points at the Memgraph instance.
type MemgraphConfig struct {
	URI      string `yaml:"uri"`
	User     string `yaml:"user"`
	Password string `yaml:"password"`
}

// ServerConfig configures the REST/MCP surface and optional profiler.
type ServerConfig struct {
	Addr      string `yaml:"addr"`
	APIKey    string `yaml:"api_key"`
	PprofAddr string `yaml:"pprof_addr"` // empty disables the loopback-only listener
}

// ExtractorConfig selects the extraction provider.
type ExtractorConfig struct {
	Mode    string        `yaml:"mode"` // sidecar | openai | mock
	OpenAI  OpenAIConfig  `yaml:"openai"`
	Sidecar SidecarConfig `yaml:"sidecar"`
}

// OpenAIConfig configures an OpenAI-compatible endpoint.
type OpenAIConfig struct {
	BaseURL string `yaml:"base_url"`
	APIKey  string `yaml:"api_key"`
	Model   string `yaml:"model"`
}

// DefaultSidecarModel is the model used when no sidecar model is specified.
const DefaultSidecarModel = "glinerel-314m"

// SidecarConfig configures the local GLiNEREL sidecar.
type SidecarConfig struct {
	URL      string `yaml:"url"`
	Model    string `yaml:"model"`
	Image    string `yaml:"image"` // optional image reference for replay provenance
	TimeoutS int    `yaml:"timeout_s"`
}

// EmbedderConfig configures the optional embedding provider.
type EmbedderConfig struct {
	Enabled     bool   `yaml:"enabled"`
	Provider    string `yaml:"provider"` // "" (openai default) | openai | furls
	BaseURL     string `yaml:"base_url"`
	APIKey      string `yaml:"api_key"`
	Model       string `yaml:"model"`
	Dim         int    `yaml:"dim"`
	QueryFURL   string `yaml:"query_furl"`   // furls: the embed_query Function URL
	ProductFURL string `yaml:"product_furl"` // furls: the embed_product Function URL
	BatchSize   int    `yaml:"batch_size"`   // furls: entries per request (0 = one request)
}

// ConverterConfig configures the optional high-quality document converter
// (a docling-serve sidecar).
type ConverterConfig struct {
	Mode          string  `yaml:"mode"` // off | docling
	URL           string  `yaml:"url"`
	APIKey        string  `yaml:"api_key"`
	TimeoutS      int     `yaml:"timeout_s"`
	MaxResponseMB int     `yaml:"max_response_mb"`
	Images        bool    `yaml:"images"`
	OCREngine     string  `yaml:"ocr_engine"`
	OCRLang       string  `yaml:"ocr_lang"` // comma-separated, engine codes (Tesseract: eng,ita); empty sends none
	MaxImageMP    float64 `yaml:"max_image_mp"`
}

// JudgeConfig configures the optional online verification judge.
type JudgeConfig struct {
	Auth            string   `yaml:"auth"`
	Temperature     *float64 `yaml:"temperature"`
	MaxTokens       *int     `yaml:"max_tokens"`
	ReasoningEffort *string  `yaml:"reasoning_effort"`
	Mode            string   `yaml:"mode"` // off | low-confidence | sample | all
	StoreInputs     bool     `yaml:"store_inputs"`
	VerifyFloor     float64  `yaml:"verify_floor"`
	SamplePct       float64  `yaml:"sample_pct"`
	BaseURL         string   `yaml:"base_url"`
	APIKey          string   `yaml:"api_key"`
	Model           string   `yaml:"model"`
}

// FactsConfig controls fact confirmation and read visibility.
type FactsConfig struct {
	RequireConfirmation  bool `yaml:"require_confirmation"`
	AllowUnverifiedReads bool `yaml:"allow_unverified_reads"`
}

// DomainConfig configures automatic domain assignment and migration seeding.
type DomainConfig struct {
	Tau         float64 `yaml:"tau"`
	Delta       float64 `yaml:"delta"`
	TauOverride float64 `yaml:"tau_override"`
	SeedFile    string  `yaml:"seed_file"`
}

const (
	// DefaultAgentBaseURL is the base URL used when no agent endpoint is specified.
	DefaultAgentBaseURL = "https://api.openai.com/v1"
	// DefaultAgentModel is the model used when no agent model is specified.
	DefaultAgentModel = "deepseek-v4.1-flash"
)

// AgentConfig configures the optional "Ask the graph" LLM agent served at
// POST /v1/agent/ask. Disabled by default; the endpoint answers 501 while
// disabled and the web UI hides its ask panel.
type AgentConfig struct {
	Auth             string   `yaml:"auth"`
	Temperature      *float64 `yaml:"temperature"`
	MaxTokens        *int     `yaml:"max_tokens"`
	ReasoningEfforts []string `yaml:"reasoning_efforts"`
	TopKEnabled      bool     `yaml:"top_k_enabled"`
	Enabled          bool     `yaml:"enabled"`
	Stream           bool     `yaml:"stream"` // stream the provider response (GKG_AGENT_STREAM; default true)
	BaseURL          string   `yaml:"base_url"`
	APIKey           string   `yaml:"api_key"`
	Model            string   `yaml:"model"`
	MaxSteps         int      `yaml:"max_steps"`         // tool-turn budget per ask (clamped 1..30; each turn executes up to 8 calls)
	TimeoutS         int      `yaml:"timeout_s"`         // whole-ask budget in seconds
	ContextBudgetKB  int      `yaml:"context_budget_kb"` // cumulative tool-result transcript budget (default 256 KiB)
}

// IngestConfig tunes the ingestion pipeline.
type IngestConfig struct {
	StoplistFile     string  `yaml:"stoplist_file"`
	SpoolDir         string  `yaml:"spool_dir"`
	SpoolMaxCount    int     `yaml:"spool_max_count"`
	SpoolMaxBytes    int     `yaml:"spool_max_bytes"`
	MaxUploadMB      int     `yaml:"max_upload_mb"`
	MaxURLMB         int     `yaml:"max_url_mb"`
	MaxChunks        int     `yaml:"max_chunks"`
	StoreFloor       float64 `yaml:"store_floor"`
	AssertFloor      float64 `yaml:"assert_floor"`
	ChunkWords       int     `yaml:"chunk_words"`
	OverlapSentences int     `yaml:"overlap_sentences"`
	ChunkDedup       string  `yaml:"chunk_dedup"` // off | exact | near
	JaccardThreshold float64 `yaml:"jaccard_threshold"`
	FetchTimeoutS    int     `yaml:"fetch_timeout_s"`
	MaxRedirects     int     `yaml:"max_redirects"`
}

// LabelsConfig locates label presets.
type LabelsConfig struct {
	PresetsDir string `yaml:"presets_dir"`
}

const defaultReadTimeoutMS = 15000

// LimitsConfig caps request-path reads.
type LimitsConfig struct {
	CypherRows      int `yaml:"cypher_rows"`
	CypherTimeoutMS int `yaml:"cypher_timeout_ms"`
	ReadTimeoutMS   int `yaml:"read_timeout_ms"`
}

// ReadTimeout is the effective timeout for non-Cypher reads.
func (c LimitsConfig) ReadTimeout() time.Duration {
	ms := int64(c.ReadTimeoutMS)
	if ms <= 0 {
		ms = defaultReadTimeoutMS
	}
	return time.Duration(min(ms, math.MaxInt64/int64(time.Millisecond))) * time.Millisecond
}

func defaults() Config {
	return Config{
		Memgraph: MemgraphConfig{URI: "bolt://localhost:7687"},
		Server:   ServerConfig{Addr: ":8080"},
		Extractor: ExtractorConfig{
			Mode:    "sidecar",
			OpenAI:  OpenAIConfig{BaseURL: "https://api.openai.com/v1", Model: "deepseek-v4.1-flash"},
			Sidecar: SidecarConfig{URL: "http://localhost:8090", Model: DefaultSidecarModel, TimeoutS: 300},
		},
		Embedder: EmbedderConfig{
			Enabled: false,
			BaseURL: "https://api.openai.com/v1",
			Dim:     768,
		},
		Converter: ConverterConfig{
			Mode:          "docling",
			URL:           "http://localhost:5001",
			TimeoutS:      300,
			MaxResponseMB: 32,
			Images:        true,
			OCREngine:     "tesseract",
			OCRLang:       "eng",
			MaxImageMP:    12.5,
		},
		GCP: GCPConfig{Credentials: "metadata"},
		Judge: JudgeConfig{
			Auth:        AuthStatic,
			Mode:        "off",
			VerifyFloor: 0.35,
			SamplePct:   10,
			BaseURL:     "https://api.openai.com/v1",
		},
		Domain: DomainConfig{Tau: 0.05, Delta: 0.02, TauOverride: 0.15},
		Agent: AgentConfig{
			Auth:            AuthStatic,
			Enabled:         false,
			Stream:          true,
			BaseURL:         DefaultAgentBaseURL,
			Model:           DefaultAgentModel,
			MaxSteps:        6,
			TimeoutS:        60,
			ContextBudgetKB: 256,
		},
		Ingest: IngestConfig{
			SpoolMaxCount:    1024,
			SpoolMaxBytes:    1 << 30,
			MaxUploadMB:      20,
			MaxURLMB:         10,
			MaxChunks:        2000,
			StoreFloor:       0.15,
			AssertFloor:      0.50,
			ChunkWords:       200,
			OverlapSentences: 1,
			ChunkDedup:       "near",
			JaccardThreshold: 0.85,
			FetchTimeoutS:    30,
			MaxRedirects:     5,
		},
		Labels: LabelsConfig{PresetsDir: "labels"},
		Limits: LimitsConfig{CypherRows: 1000, CypherTimeoutMS: 10000, ReadTimeoutMS: defaultReadTimeoutMS},
	}
}

// Load returns the effective configuration: defaults, then the YAML file at
// path when non-empty, then environment overrides. A non-empty path that
// cannot be read or parsed is an error; an empty document keeps the defaults.
func Load(path string) (Config, error) {
	cfg := defaults()
	if path != "" {
		data, err := os.ReadFile(path)
		if err != nil {
			return Config{}, fmt.Errorf("config: read %s: %w", path, err)
		}
		dec := yaml.NewDecoder(strings.NewReader(string(data)))
		dec.KnownFields(true)
		if err := dec.Decode(&cfg); err != nil && !errors.Is(err, io.EOF) {
			return Config{}, fmt.Errorf("config: parse %s: %w", path, err)
		}
	}
	if err := applyEnv(&cfg, os.LookupEnv); err != nil {
		return Config{}, err
	}
	if err := cfg.validateLLM(); err != nil {
		return Config{}, err
	}
	if cfg.Ingest.SpoolDir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return Config{}, fmt.Errorf("config: set GKG_INGEST_SPOOL_DIR: %w", err)
		}
		cfg.Ingest.SpoolDir = filepath.Join(home, ".local", "share", "gkg", "ingest-spool")
	}
	if cfg.Ingest.SpoolMaxCount <= 0 || cfg.Ingest.SpoolMaxBytes <= 0 {
		return Config{}, errors.New("config: GKG_INGEST_SPOOL_MAX_COUNT and GKG_INGEST_SPOOL_MAX_BYTES must be positive")
	}
	if cfg.Extractor.Sidecar.TimeoutS <= 0 || int64(cfg.Extractor.Sidecar.TimeoutS) > math.MaxInt64/1_000_000_000 {
		return Config{}, errors.New("config: GKG_EXTRACTOR_SIDECAR_TIMEOUT_S must be positive and fit a duration")
	}
	// Documented exception: the HTTP API key is set with GKG_API_KEY.
	if v, ok := os.LookupEnv("GKG_API_KEY"); ok {
		cfg.Server.APIKey = v
	}
	// Documented exceptions: the embeddings service exports its Function
	// URLs and shared secret as EMBED_*; they map to the Embedder section
	// only for the furls provider and only when non-empty (an empty
	// placeholder must not clobber a GKG_EMBEDDER_* value).
	if cfg.Embedder.Provider == "furls" {
		if v, ok := os.LookupEnv("EMBED_QUERY_FURL"); ok && v != "" {
			cfg.Embedder.QueryFURL = v
		}
		if v, ok := os.LookupEnv("EMBED_PRODUCT_FURL"); ok && v != "" {
			cfg.Embedder.ProductFURL = v
		}
		if v, ok := os.LookupEnv("EMBED_API_KEY"); ok && v != "" {
			cfg.Embedder.APIKey = v
		}
		if v, ok := os.LookupEnv("EMBED_BATCH_SIZE"); ok && v != "" {
			n, err := strconv.Atoi(v)
			if err != nil {
				return Config{}, fmt.Errorf("config: env EMBED_BATCH_SIZE=%q: %w", v, err)
			}
			cfg.Embedder.BatchSize = n
		}
	}
	if strings.TrimSpace(cfg.Converter.OCREngine) == "" {
		return Config{}, errors.New("config: GKG_CONVERTER_OCR_ENGINE must not be empty")
	}
	mp := cfg.Converter.MaxImageMP
	if mp <= 0 || math.IsNaN(mp) || math.IsInf(mp, 0) || mp >= float64(math.MaxInt64)/1e6 {
		return Config{}, errors.New("config: GKG_CONVERTER_MAX_IMAGE_MP must be positive, finite and fit an int64 pixel count")
	}
	if cfg.Judge.StoreInputs {
		switch cfg.Judge.Mode {
		case "off", "low-confidence", "sample", "all":
		default:
			return Config{}, errors.New("judge.store_inputs needs judge.mode=off, low-confidence, sample or all")
		}
		if cfg.Facts.RequireConfirmation {
			return Config{}, errors.New("judge.store_inputs is incompatible with facts.require_confirmation")
		}
	}
	if cfg.Facts.RequireConfirmation && cfg.Judge.Mode != "all" {
		return Config{}, errors.New("facts.require_confirmation needs judge.mode=all")
	}
	return cfg, nil
}

// applyEnv walks the configuration struct and applies overrides named
// GKG_<SECTION>_<KEY>, mirroring the snake_case yaml keys, e.g.
// GKG_SERVER_ADDR, GKG_EXTRACTOR_OPENAI_BASE_URL.
func applyEnv(cfg *Config, lookup func(string) (string, bool)) error {
	return walkEnv(reflect.ValueOf(cfg).Elem(), "GKG", lookup)
}

func walkEnv(v reflect.Value, prefix string, lookup func(string) (string, bool)) error {
	t := v.Type()
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		if !f.IsExported() {
			continue
		}
		fv := v.Field(i)
		seg := upperSnake(f.Name)
		if tag, ok := f.Tag.Lookup("yaml"); ok {
			if name := strings.Split(tag, ",")[0]; name != "" && name != "-" {
				seg = strings.ToUpper(name)
			}
		}
		key := prefix + "_" + seg
		if fv.Kind() == reflect.Struct {
			if err := walkEnv(fv, key, lookup); err != nil {
				return err
			}
			continue
		}
		val, ok := lookup(key)
		if !ok {
			continue
		}
		if err := setEnvField(fv, val); err != nil {
			return fmt.Errorf("config: env %s=%q: %w", key, val, err)
		}
	}
	return nil
}

func setEnvField(field reflect.Value, val string) error {
	switch field.Kind() {
	case reflect.Pointer:
		value := reflect.New(field.Type().Elem())
		if err := setEnvField(value.Elem(), val); err != nil {
			return err
		}
		field.Set(value)
	case reflect.String:
		field.SetString(val)
	case reflect.Int:
		n, err := strconv.Atoi(val)
		if err != nil {
			return err
		}
		field.SetInt(int64(n))
	case reflect.Float64:
		n, err := strconv.ParseFloat(val, 64)
		if err != nil {
			return err
		}
		field.SetFloat(n)
	case reflect.Bool:
		n, err := strconv.ParseBool(val)
		if err != nil {
			return err
		}
		field.SetBool(n)
	case reflect.Slice:
		if field.Type() != reflect.TypeFor[[]string]() {
			return fmt.Errorf("unsupported field type %s", field.Type())
		}
		items := []string{}
		if strings.TrimSpace(val) != "" {
			for item := range strings.SplitSeq(val, ",") {
				item = strings.TrimSpace(item)
				if item == "" {
					return errors.New("list contains an empty item")
				}
				items = append(items, item)
			}
		}
		field.Set(reflect.ValueOf(items))
	default:
		return fmt.Errorf("unsupported field type %s", field.Type())
	}
	return nil
}

// upperSnake converts a Go field name to UPPER_SNAKE_CASE
// (BaseURL -> BASE_URL, APIKey -> API_KEY, MaxUploadMB -> MAX_UPLOAD_MB).
func upperSnake(name string) string {
	var b []byte
	for i := 0; i < len(name); i++ {
		c := name[i]
		if isUpper(c) {
			insert := i > 0 && (isLower(name[i-1]) || isDigit(name[i-1]) ||
				(isUpper(name[i-1]) && i+1 < len(name) && isLower(name[i+1])))
			if insert {
				b = append(b, '_')
			}
		}
		if isLower(c) {
			c -= 'a' - 'A'
		}
		b = append(b, c)
	}
	return string(b)
}

func isUpper(c byte) bool { return c >= 'A' && c <= 'Z' }
func isLower(c byte) bool { return c >= 'a' && c <= 'z' }
func isDigit(c byte) bool { return c >= '0' && c <= '9' }
