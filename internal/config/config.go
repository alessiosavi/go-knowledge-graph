// Package config loads the gkg configuration from defaults, an optional
// YAML file and environment overrides.
//
// Precedence: environment (GKG_<SECTION>_<KEY>) > YAML > defaults.
// The HTTP API key is the documented exception: GKG_API_KEY maps to
// Server.APIKey.
package config

import (
	"fmt"
	"os"
	"reflect"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
)

// Config is the root configuration.
type Config struct {
	Memgraph  MemgraphConfig  `yaml:"memgraph"`
	Server    ServerConfig    `yaml:"server"`
	Extractor ExtractorConfig `yaml:"extractor"`
	Embedder  EmbedderConfig  `yaml:"embedder"`
	Judge     JudgeConfig     `yaml:"judge"`
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

// ServerConfig configures the REST/MCP HTTP surface.
type ServerConfig struct {
	Addr   string `yaml:"addr"`
	APIKey string `yaml:"api_key"`
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

// SidecarConfig configures the local GLiNEREL sidecar.
type SidecarConfig struct {
	URL string `yaml:"url"`
}

// EmbedderConfig configures the optional embedding provider.
type EmbedderConfig struct {
	Enabled bool   `yaml:"enabled"`
	BaseURL string `yaml:"base_url"`
	APIKey  string `yaml:"api_key"`
	Model   string `yaml:"model"`
	Dim     int    `yaml:"dim"`
}

// JudgeConfig configures the optional online verification judge.
type JudgeConfig struct {
	Mode        string  `yaml:"mode"` // off | low-confidence | sample | all
	VerifyFloor float64 `yaml:"verify_floor"`
	SamplePct   float64 `yaml:"sample_pct"`
	BaseURL     string  `yaml:"base_url"`
	APIKey      string  `yaml:"api_key"`
	Model       string  `yaml:"model"`
}

// IngestConfig tunes the ingestion pipeline.
type IngestConfig struct {
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

// LimitsConfig caps the read-only cypher surface.
type LimitsConfig struct {
	CypherRows      int `yaml:"cypher_rows"`
	CypherTimeoutMS int `yaml:"cypher_timeout_ms"`
}

func defaults() Config {
	return Config{
		Memgraph: MemgraphConfig{URI: "bolt://localhost:7687"},
		Server:   ServerConfig{Addr: ":8080"},
		Extractor: ExtractorConfig{
			Mode:    "openai",
			OpenAI:  OpenAIConfig{BaseURL: "https://api.openai.com/v1", Model: "deepseek-v4.1-flash"},
			Sidecar: SidecarConfig{URL: "http://localhost:8090"},
		},
		Embedder: EmbedderConfig{
			Enabled: false,
			BaseURL: "https://api.openai.com/v1",
			Dim:     768,
		},
		Judge: JudgeConfig{
			Mode:        "off",
			VerifyFloor: 0.35,
			SamplePct:   10,
			BaseURL:     "https://api.openai.com/v1",
		},
		Ingest: IngestConfig{
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
		Limits: LimitsConfig{CypherRows: 1000, CypherTimeoutMS: 10000},
	}
}

// Load returns the effective configuration: defaults, then the YAML file at
// path when non-empty, then environment overrides. A non-empty path that
// cannot be read or parsed is an error.
func Load(path string) (Config, error) {
	cfg := defaults()
	if path != "" {
		data, err := os.ReadFile(path)
		if err != nil {
			return Config{}, fmt.Errorf("config: read %s: %w", path, err)
		}
		dec := yaml.NewDecoder(strings.NewReader(string(data)))
		dec.KnownFields(true)
		if err := dec.Decode(&cfg); err != nil {
			return Config{}, fmt.Errorf("config: parse %s: %w", path, err)
		}
	}
	if err := applyEnv(&cfg, os.LookupEnv); err != nil {
		return Config{}, err
	}
	// Documented exception: the HTTP API key is set with GKG_API_KEY.
	if v, ok := os.LookupEnv("GKG_API_KEY"); ok {
		cfg.Server.APIKey = v
	}
	return cfg, nil
}

// applyEnv walks the configuration struct and applies overrides named
// GKG_<SECTION>_<KEY>, e.g. GKG_SERVER_ADDR, GKG_EXTRACTOR_OPENAI_BASE_URL.
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
		key := prefix + "_" + upperSnake(f.Name)
		switch fv.Kind() {
		case reflect.Struct:
			if err := walkEnv(fv, key, lookup); err != nil {
				return err
			}
		case reflect.String:
			if val, ok := lookup(key); ok {
				fv.SetString(val)
			}
		case reflect.Int:
			if val, ok := lookup(key); ok {
				n, err := strconv.Atoi(val)
				if err != nil {
					return fmt.Errorf("config: env %s=%q: %w", key, val, err)
				}
				fv.SetInt(int64(n))
			}
		case reflect.Float64:
			if val, ok := lookup(key); ok {
				n, err := strconv.ParseFloat(val, 64)
				if err != nil {
					return fmt.Errorf("config: env %s=%q: %w", key, val, err)
				}
				fv.SetFloat(n)
			}
		case reflect.Bool:
			if val, ok := lookup(key); ok {
				n, err := strconv.ParseBool(val)
				if err != nil {
					return fmt.Errorf("config: env %s=%q: %w", key, val, err)
				}
				fv.SetBool(n)
			}
		default:
			return fmt.Errorf("config: unsupported field type %s (field %s)", fv.Kind(), f.Name)
		}
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
