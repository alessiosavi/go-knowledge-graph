package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/alessiosavi/go-knowledge-graph/internal/agent"
	"github.com/alessiosavi/go-knowledge-graph/internal/config"
	"github.com/alessiosavi/go-knowledge-graph/internal/judge"
)

func TestEvalSettingsOutputKeysAndCanonicalHash(t *testing.T) {
	settingsFixture(t)
	const baseURL = "https://judge.invalid/v1?one=<fixture>&two=>"
	t.Setenv("GKG_JUDGE_BASE_URL", baseURL)
	got, _ := readEvalSettings(t)
	assertSettingsKeys(t, got, "projection", "overrides", "asserted", "hash")
	proj := got["projection"].(map[string]any)
	assertSettingsKeys(t, proj, "judge", "agent", "gcp", "embedder", "limits")
	assertSettingsKeys(t, proj["judge"].(map[string]any), "auth", "base_url", "model", "temperature", "max_tokens", "reasoning_effort", "prompt_version", "prompt_sha256")
	assertSettingsKeys(t, proj["agent"].(map[string]any), "auth", "base_url", "model", "temperature", "max_tokens", "reasoning_efforts", "stream", "max_steps",
		"timeout_s", "context_budget_kb", "top_k_enabled", "system_prompt_sha256")
	assertSettingsKeys(t, proj["gcp"].(map[string]any), "credentials", "route")
	assertSettingsKeys(t, proj["gcp"].(map[string]any)["route"].(map[string]any), "host", "project", "location")
	assertSettingsKeys(t, proj["embedder"].(map[string]any), "enabled", "provider", "base_url", "model", "dim", "query_furl", "product_furl", "batch_size")
	assertSettingsKeys(t, proj["limits"].(map[string]any), "cypher_rows", "cypher_timeout_ms", "read_timeout_ms")
	overrides := got["overrides"].(map[string]any)
	assertSettingsKeys(t, overrides, "judge", "memgraph", "server")
	assertSettingsKeys(t, overrides["judge"].(map[string]any), "mode")
	assertSettingsKeys(t, overrides["memgraph"].(map[string]any), "uri")
	assertSettingsKeys(t, overrides["server"].(map[string]any), "addr", "pprof_addr", "api_key")
	asserted := got["asserted"].(map[string]any)
	assertSettingsKeys(t, asserted, "judge", "facts", "ingest", "agent")
	assertSettingsKeys(t, asserted["judge"].(map[string]any), "mode", "store_inputs", "verify_floor", "sample_pct")
	assertSettingsKeys(t, asserted["facts"].(map[string]any), "require_confirmation", "allow_unverified_reads")
	assertSettingsKeys(t, asserted["ingest"].(map[string]any), "store_floor", "assert_floor")
	assertSettingsKeys(t, asserted["agent"].(map[string]any), "enabled")
	if proj["judge"].(map[string]any)["base_url"] != baseURL {
		t.Fatal("projection lost the base URL containing HTML-sensitive characters")
	}
	var canonical bytes.Buffer
	enc := json.NewEncoder(&canonical)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(proj); err != nil {
		t.Fatal(err)
	}
	data := bytes.TrimSuffix(canonical.Bytes(), []byte("\n"))
	if want := fmt.Sprintf("%x", sha256.Sum256(data)); got["hash"] != want {
		t.Errorf("projection hash=%v want %s for %s", got["hash"], want, data)
	}
}

func TestEvalSettingsHashIgnoresOverridesAndAsserted(t *testing.T) {
	settingsFixture(t)
	before, _ := readEvalSettings(t)
	for key, value := range map[string]string{
		"GKG_MEMGRAPH_URI": "bolt://different.invalid:1234", "GKG_SERVER_ADDR": ":8712", "GKG_SERVER_PPROF_ADDR": "127.0.0.1:8713",
		"GKG_API_KEY": "synthetic-server-key",
	} {
		t.Setenv(key, value)
	}
	second, _ := readEvalSettings(t)
	if before["hash"] != second["hash"] || !reflect.DeepEqual(before["projection"], second["projection"]) {
		t.Error("settings projection or hash changed with graph URI and server settings")
	}
	wantOverrides := map[string]any{
		"judge":    map[string]any{"mode": "off"},
		"memgraph": map[string]any{"uri": "bolt://different.invalid:1234"},
		"server":   map[string]any{"addr": ":8712", "pprof_addr": "127.0.0.1:8713", "api_key": "<set>"},
	}
	if !reflect.DeepEqual(second["overrides"], wantOverrides) || !reflect.DeepEqual(second["asserted"], before["asserted"]) {
		t.Fatalf("overrides=%v asserted=%v", second["overrides"], second["asserted"])
	}
	asserted := second["asserted"].(map[string]any)
	if asserted["judge"].(map[string]any)["mode"] != "all" ||
		asserted["facts"].(map[string]any)["require_confirmation"] != true {
		t.Fatalf("production judge and fact settings were lost: %v", asserted)
	}
	for key, value := range map[string]string{
		"GKG_JUDGE_MODE": "sample", "GKG_JUDGE_STORE_INPUTS": "true", "GKG_JUDGE_VERIFY_FLOOR": "0.71",
		"GKG_JUDGE_SAMPLE_PCT": "25", "GKG_FACTS_REQUIRE_CONFIRMATION": "false", "GKG_FACTS_ALLOW_UNVERIFIED_READS": "true",
		"GKG_INGEST_STORE_FLOOR": "0.61", "GKG_INGEST_ASSERT_FLOOR": "0.91", "GKG_AGENT_ENABLED": "true",
	} {
		t.Setenv(key, value)
	}
	third, _ := readEvalSettings(t)
	wantAsserted := map[string]any{
		"judge":  map[string]any{"mode": "sample", "store_inputs": true, "verify_floor": .71, "sample_pct": float64(25)},
		"facts":  map[string]any{"require_confirmation": false, "allow_unverified_reads": true},
		"ingest": map[string]any{"store_floor": .61, "assert_floor": .91},
		"agent":  map[string]any{"enabled": true},
	}
	if third["hash"] != before["hash"] || !reflect.DeepEqual(third["overrides"], wantOverrides) ||
		!reflect.DeepEqual(third["asserted"], wantAsserted) {
		t.Fatalf("production changes did not stay in asserted: %v", third)
	}
}

func TestEvalSettingsHashChangesWithProjectedFields(t *testing.T) {
	settingsFixture(t)
	for _, tc := range []struct {
		field string
		env   string
		value string
	}{
		{"judge.auth", "GKG_JUDGE_AUTH", "gcp"},
		{"judge.base_url", "GKG_JUDGE_BASE_URL", "https://changed.invalid"},
		{"judge.model", "GKG_JUDGE_MODEL", "changed"},
		{"judge.temperature", "GKG_JUDGE_TEMPERATURE", "0.3"},
		{"judge.max_tokens", "GKG_JUDGE_MAX_TOKENS", "1515"},
		{"judge.reasoning_effort", "GKG_JUDGE_REASONING_EFFORT", "low"},
		{"agent.auth", "GKG_AGENT_AUTH", "gcp"},
		{"agent.base_url", "GKG_AGENT_BASE_URL", "https://changed.invalid"},
		{"agent.model", "GKG_AGENT_MODEL", "changed"},
		{"agent.temperature", "GKG_AGENT_TEMPERATURE", "0.7"},
		{"agent.max_tokens", "GKG_AGENT_MAX_TOKENS", "1717"},
		{"agent.reasoning_efforts", "GKG_AGENT_REASONING_EFFORTS", "low,medium"},
		{"agent.stream", "GKG_AGENT_STREAM", "false"},
		{"agent.max_steps", "GKG_AGENT_MAX_STEPS", "7"},
		{"agent.timeout_s", "GKG_AGENT_TIMEOUT_S", "73"},
		{"agent.context_budget_kb", "GKG_AGENT_CONTEXT_BUDGET_KB", "777"},
		{"agent.top_k_enabled", "GKG_AGENT_TOP_K_ENABLED", "true"},
		{"gcp.route.host", "GKG_GCP_ROUTE_HOST", "changed.invalid"},
		{"gcp.route.project", "GKG_GCP_ROUTE_PROJECT", "changed"},
		{"gcp.route.location", "GKG_GCP_ROUTE_LOCATION", "changed"},
		{"embedder.enabled", "GKG_EMBEDDER_ENABLED", "true"},
		{"embedder.provider", "GKG_EMBEDDER_PROVIDER", "furls"},
		{"embedder.base_url", "GKG_EMBEDDER_BASE_URL", "https://changed.invalid"},
		{"embedder.model", "GKG_EMBEDDER_MODEL", "changed"},
		{"embedder.dim", "GKG_EMBEDDER_DIM", "512"},
		{"embedder.query_furl", "GKG_EMBEDDER_QUERY_FURL", "https://changed.invalid/query"},
		{"embedder.product_furl", "GKG_EMBEDDER_PRODUCT_FURL", "https://changed.invalid/product"},
		{"embedder.batch_size", "GKG_EMBEDDER_BATCH_SIZE", "10"},
		{"limits.cypher_rows", "GKG_LIMITS_CYPHER_ROWS", "77"},
		{"limits.cypher_timeout_ms", "GKG_LIMITS_CYPHER_TIMEOUT_MS", "7711"},
		{"limits.read_timeout_ms", "GKG_LIMITS_READ_TIMEOUT_MS", "7701"},
	} {
		t.Run(tc.field, func(t *testing.T) {
			if tc.value == "gcp" {
				const vertex = "https://vertex.invalid/v1/projects/fixture/locations/global/endpoints/openapi"
				t.Setenv("GKG_GCP_ROUTE_HOST", "vertex.invalid")
				t.Setenv("GKG_GCP_ROUTE_PROJECT", "fixture")
				t.Setenv("GKG_GCP_ROUTE_LOCATION", "global")
				role, _, _ := strings.Cut(tc.field, ".")
				t.Setenv("GKG_"+strings.ToUpper(role)+"_BASE_URL", vertex)
			}
			before, _ := readEvalSettings(t)
			t.Setenv(tc.env, tc.value)
			after, _ := readEvalSettings(t)
			if before["hash"] == after["hash"] {
				t.Errorf("changing %s did not change the projection hash", tc.env)
			}
			parts := strings.Split(tc.field, ".")
			var previous, current any = before["projection"], after["projection"]
			for _, part := range parts {
				previous = previous.(map[string]any)[part]
				current = current.(map[string]any)[part]
			}
			if reflect.DeepEqual(previous, current) {
				t.Errorf("%s did not change projection.%s, got %v", tc.env, tc.field, current)
			}
		})
	}
}

func TestEvalSettingsHashChangesWithPromptsAndCredentialMode(t *testing.T) {
	cfg := config.Config{GCP: config.GCPConfig{Credentials: "metadata"}}
	projection := evalSettingsProjection(cfg)
	baseline, err := hashEvalSettingsProjection(projection)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		block, field, want, alternate string
	}{
		{"judge", "prompt_version", judge.PromptVersion, judge.PromptVersion + "-changed"},
		{"judge", "prompt_sha256", fmt.Sprintf("%x", sha256.Sum256([]byte(judge.Prompt()))), strings.Repeat("0", 64)},
		{"agent", "system_prompt_sha256", fmt.Sprintf("%x", sha256.Sum256([]byte(agent.SystemPrompt()))), strings.Repeat("0", 64)},
		{"gcp", "credentials", cfg.GCP.Credentials, "alternate"},
	} {
		t.Run(tc.block+"."+tc.field, func(t *testing.T) {
			changed := evalSettingsProjection(cfg)
			fields := changed[tc.block].(map[string]any)
			if fields[tc.field] != tc.want {
				t.Fatalf("projected %s = %v, want %q", tc.field, fields[tc.field], tc.want)
			}
			fields[tc.field] = tc.alternate
			if digest, err := hashEvalSettingsProjection(changed); err != nil || digest == baseline {
				t.Errorf("changing %s gave hash %q, err=%v", tc.field, digest, err)
			}
		})
	}
}

func TestEvalSettingsRedactsSecrets(t *testing.T) {
	settingsFixture(t)
	before, _ := readEvalSettings(t)
	if before["overrides"].(map[string]any)["server"].(map[string]any)["api_key"] != "<unset>" {
		t.Fatal("empty server key is not redacted")
	}
	for key, value := range map[string]string{
		"GKG_JUDGE_API_KEY": "secret-fixture-judge", "GKG_AGENT_API_KEY": "secret-fixture-agent",
		"GKG_EMBEDDER_API_KEY": "secret-fixture-embedder", "GKG_API_KEY": "secret-fixture-server",
		"GKG_MEMGRAPH_PASSWORD":   "secret-fixture-graph",
		"GKG_MEMGRAPH_URI":        "bolt://secret-fixture-user:secret-fixture-uri@localhost:17724",
		"GKG_EMBEDDER_QUERY_FURL": "http://localhost:17724/embeddings?access_token=secret-fixture-url&access_token=&mode=%3Cset%3E",
	} {
		t.Setenv(key, value)
	}
	after, output := readEvalSettings(t)
	if strings.Contains(output, "secret-fixture-") {
		t.Error("settings output contains a raw secret")
	}
	for _, want := range []string{
		"bolt://<set>:<set>@localhost:17724",
		"http://localhost:17724/embeddings?access_token=<set>&access_token=<unset>&mode=%3Cset%3E",
	} {
		if !strings.Contains(output, want) {
			t.Errorf("settings output is missing redacted URL %q", want)
		}
	}
	if after["overrides"].(map[string]any)["server"].(map[string]any)["api_key"] != "<set>" {
		t.Error("configured server key is not redacted")
	}
}

func TestEvalSettingsUsage(t *testing.T) {
	settingsFixture(t)
	assertEvalCLI(t, []string{"eval-settings", "-h"}, 0, "usage: gkg eval-settings\n", "")
	assertEvalCLI(t, []string{"eval-settings", "extra"}, 2, "", "usage:")
	assertEvalCLI(t, []string{"eval-settings", "-bad"}, 2, "", "flag provided but not defined")
}

func settingsFixture(t *testing.T) {
	t.Helper()
	t.Chdir(t.TempDir())
	for key, value := range map[string]string{
		"GKG_CONFIG": "", "GKG_MEMGRAPH_URI": "invalid-test-uri", "GKG_JUDGE_MODE": "all",
		"GKG_JUDGE_STORE_INPUTS": "false", "GKG_FACTS_REQUIRE_CONFIRMATION": "true",
		"GKG_API_KEY": "", "GKG_JUDGE_API_KEY": "", "GKG_AGENT_API_KEY": "", "GKG_EMBEDDER_API_KEY": "",
		"GKG_EMBEDDER_PROVIDER": "openai", "EMBED_API_KEY": "",
	} {
		t.Setenv(key, value)
	}
}

func readEvalSettings(t *testing.T) (map[string]any, string) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	if code := run([]string{"eval-settings"}, &stdout, &stderr); code != 0 || stderr.Len() != 0 {
		t.Fatalf("eval-settings exit=%d stderr=%s", code, stderr.String())
	}
	var doc map[string]any
	if err := json.Unmarshal(stdout.Bytes(), &doc); err != nil {
		t.Fatal(err)
	}
	return doc, stdout.String()
}

func assertSettingsKeys(t *testing.T, got map[string]any, keys ...string) {
	t.Helper()
	if len(got) != len(keys) {
		t.Fatalf("keys %v, want %v", got, keys)
	}
	for _, key := range keys {
		if _, ok := got[key]; !ok {
			t.Errorf("missing key %s in %v", key, got)
		}
	}
}
