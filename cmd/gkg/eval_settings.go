package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"maps"
	"net/url"
	"slices"
	"strings"

	"github.com/alessiosavi/go-knowledge-graph/internal/agent"
	"github.com/alessiosavi/go-knowledge-graph/internal/config"
	"github.com/alessiosavi/go-knowledge-graph/internal/embed"
	"github.com/alessiosavi/go-knowledge-graph/internal/judge"
)

// Eval commands run without judge workers, regardless of the loaded mode.
const evalJudgeMode = "off"

func runEvalSettings(args []string, stdout, stderr io.Writer) int {
	const usage = "usage: gkg eval-settings"
	fs := flag.NewFlagSet("eval-settings", flag.ContinueOnError)
	fs.SetOutput(stderr)
	// Route help and usage errors to their respective streams after parsing.
	fs.Usage = func() {}
	err := fs.Parse(args)
	if errors.Is(err, flag.ErrHelp) {
		fmt.Fprintln(stdout, usage)
		return 0
	}
	if err != nil || fs.NArg() != 0 {
		fmt.Fprintln(stderr, usage)
		return 2
	}
	cfg, err := loadConfig()
	if err != nil {
		// Config parser errors can quote input values.
		fmt.Fprintln(stderr, "gkg eval-settings: could not load configuration")
		return 1
	}
	projection := evalSettingsProjection(cfg)
	hash, err := hashEvalSettingsProjection(projection)
	if err != nil {
		fmt.Fprintln(stderr, "gkg eval-settings: could not encode projection")
		return 1
	}
	overrides := map[string]any{
		"judge":    map[string]any{"mode": evalJudgeMode},
		"memgraph": map[string]any{"uri": redactSettingsURL(cfg.Memgraph.URI)},
		"server": map[string]any{
			"addr": cfg.Server.Addr, "pprof_addr": cfg.Server.PprofAddr, "api_key": settingsSecret(cfg.Server.APIKey),
		},
	}
	asserted := map[string]any{
		"judge": map[string]any{
			"mode": cfg.Judge.Mode, "store_inputs": cfg.Judge.StoreInputs,
			"verify_floor": cfg.Judge.VerifyFloor, "sample_pct": cfg.Judge.SamplePct,
		},
		"facts": map[string]any{
			"require_confirmation": cfg.Facts.RequireConfirmation, "allow_unverified_reads": cfg.Facts.AllowUnverifiedReads,
		},
		"ingest": map[string]any{"store_floor": cfg.Ingest.StoreFloor, "assert_floor": cfg.Ingest.AssertFloor},
		"agent":  map[string]any{"enabled": cfg.Agent.Enabled},
	}
	enc := json.NewEncoder(stdout)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(map[string]any{
		"projection": projection, "overrides": overrides, "asserted": asserted, "hash": hash,
	}); err != nil {
		fmt.Fprintln(stderr, "gkg eval-settings: could not write settings")
		return 1
	}
	return 0
}

func evalSettingsProjection(cfg config.Config) map[string]any {
	judgeBase, judgeModel := judge.ResolveEndpoint(cfg.Judge)
	agentBase, agentModel := agent.ResolveEndpoint(cfg.Agent)
	embedderModel := embed.ResolveModel(cfg.Embedder.Provider, cfg.Embedder.Model)
	return map[string]any{
		"judge": map[string]any{
			"auth": cfg.Judge.Auth, "base_url": redactSettingsURL(judgeBase), "model": judgeModel,
			"temperature": cfg.Judge.Temperature, "max_tokens": cfg.Judge.MaxTokens, "reasoning_effort": cfg.Judge.ReasoningEffort,
			"prompt_version": judge.PromptVersion, "prompt_sha256": settingsSHA256([]byte(judge.Prompt())),
		},
		"agent": map[string]any{
			"auth": cfg.Agent.Auth, "base_url": redactSettingsURL(agentBase), "model": agentModel,
			"temperature": cfg.Agent.Temperature, "max_tokens": cfg.Agent.MaxTokens, "reasoning_efforts": cfg.Agent.ReasoningEfforts,
			"stream": cfg.Agent.Stream, "max_steps": cfg.Agent.MaxSteps, "timeout_s": cfg.Agent.TimeoutS,
			"context_budget_kb": cfg.Agent.ContextBudgetKB, "top_k_enabled": cfg.Agent.TopKEnabled,
			"search_products":      cfg.Agent.SearchProductSlots(),
			"system_prompt_sha256": settingsSHA256([]byte(agent.EffectiveSystemPrompt(cfg))),
		},
		"gcp": map[string]any{
			"credentials": cfg.GCP.Credentials,
			"route":       map[string]any{"host": cfg.GCP.Route.Host, "project": cfg.GCP.Route.Project, "location": cfg.GCP.Route.Location},
		},
		"embedder": map[string]any{
			"enabled": cfg.Embedder.Enabled, "provider": cfg.Embedder.Provider,
			"base_url": redactSettingsURL(cfg.Embedder.BaseURL), "model": embedderModel, "dim": cfg.Embedder.Dim,
			"query_furl": redactSettingsURL(cfg.Embedder.QueryFURL), "product_furl": redactSettingsURL(cfg.Embedder.ProductFURL),
			"batch_size": cfg.Embedder.BatchSize,
		},
		"limits": map[string]any{
			"cypher_rows": cfg.Limits.CypherRows, "cypher_timeout_ms": cfg.Limits.CypherTimeoutMS, "read_timeout_ms": cfg.Limits.ReadTimeoutMS,
		},
	}
}

func hashEvalSettingsProjection(projection map[string]any) (string, error) {
	var data bytes.Buffer
	enc := json.NewEncoder(&data)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(projection); err != nil {
		return "", err
	}
	return settingsSHA256(bytes.TrimSuffix(data.Bytes(), []byte("\n"))), nil
}

func settingsSHA256(data []byte) string { return fmt.Sprintf("%x", sha256.Sum256(data)) }

func settingsSecret(value string) string {
	if value == "" {
		return "<unset>"
	}
	return "<set>"
}

// Endpoint URLs can carry credentials in userinfo or query parameters.
func redactSettingsURL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return settingsSecret(raw)
	}
	query := redactSettingsQuery(u.RawQuery)
	if u.User == nil && query == u.RawQuery {
		return raw
	}
	u.RawQuery = query
	if u.User == nil {
		return u.String()
	}
	user := settingsSecret(u.User.Username())
	userinfo := user
	if password, ok := u.User.Password(); ok {
		marker := settingsSecret(password)
		u.User = url.UserPassword(user, marker)
		userinfo += ":" + marker
	} else {
		u.User = url.User(user)
	}
	// URL.String escapes userinfo but writes RawQuery verbatim.
	return strings.Replace(u.String(), "//"+u.User.String()+"@", "//"+userinfo+"@", 1)
}

func redactSettingsQuery(raw string) string {
	query, err := url.ParseQuery(raw)
	if err != nil {
		return settingsSecret(raw)
	}
	var parts []string
	changed := false
	for _, key := range slices.Sorted(maps.Keys(query)) {
		normalized := strings.ToLower(key)
		sensitive := strings.Contains(normalized, "token") || strings.Contains(normalized, "secret") ||
			strings.Contains(normalized, "password") || strings.Contains(normalized, "signature") ||
			normalized == "key" || normalized == "api_key" || normalized == "api-key" || normalized == "apikey"
		changed = changed || sensitive
		prefix := url.QueryEscape(key) + "="
		for _, value := range query[key] {
			if sensitive {
				value = settingsSecret(value)
			} else {
				value = url.QueryEscape(value)
			}
			parts = append(parts, prefix+value)
		}
	}
	if changed {
		return strings.Join(parts, "&")
	}
	return raw
}
