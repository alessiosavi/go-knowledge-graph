package config

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

const testVertexBase = "https://aiplatform.googleapis.com/v1/projects/test-project/locations/global/endpoints/openapi"

func TestLLMAuthConfig(t *testing.T) {
	t.Setenv("GKG_GCP_ROUTE_HOST", "aiplatform.googleapis.com")
	t.Setenv("GKG_GCP_ROUTE_PROJECT", "test-project")
	t.Setenv("GKG_GCP_ROUTE_LOCATION", "global")
	t.Setenv("GKG_JUDGE_AUTH", "gcp")
	t.Setenv("GKG_AGENT_AUTH", "gcp")
	t.Setenv("GKG_JUDGE_BASE_URL", testVertexBase+"/")
	t.Setenv("GKG_AGENT_BASE_URL", testVertexBase)
	t.Setenv("GKG_JUDGE_API_KEY", "")
	t.Setenv("GKG_AGENT_API_KEY", "")
	t.Setenv("GKG_GCP_CREDENTIALS", "metadata")
	if _, err := Load(""); err != nil {
		t.Fatalf("valid Vertex route: %v", err)
	}
	for _, tc := range []struct{ key, val, want string }{
		{"GKG_JUDGE_BASE_URL", strings.Replace(testVertexBase, "/global/", "/eu/", 1), "judge.base_url"},
		{"GKG_AGENT_BASE_URL", testVertexBase + "?x=1", "agent.base_url"},
		{"GKG_AGENT_BASE_URL", strings.Replace(testVertexBase, "https://", "http://", 1), "agent.base_url"},
		{"GKG_GCP_ROUTE_HOST", "aiplatform.googleapis.com@evil.example", "gcp.route"},
		{"GKG_GCP_ROUTE_PROJECT", "../test-project", "gcp.route"},
		{"GKG_JUDGE_API_KEY", "fake-secret", "judge.api_key"},
		{"GKG_AGENT_API_KEY", "fake-secret", "agent.api_key"},
		{"GKG_AGENT_AUTH", "other", "agent.auth"},
		{"GKG_JUDGE_AUTH", "", "judge.auth"},
		{"GKG_GCP_CREDENTIALS", "adc", "gcp.credentials"},
	} {
		t.Run(tc.key+"="+tc.val, func(t *testing.T) {
			t.Setenv(tc.key, tc.val)
			if _, err := Load(""); err == nil || !strings.Contains(err.Error(), tc.want) || strings.Contains(err.Error(), "fake-secret") {
				t.Fatalf("error = %v, want safe error naming %s", err, tc.want)
			}
		})
	}
}

func TestLLMSettingsYAMLAndEnvLoads(t *testing.T) {
	file := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(file, []byte("judge:\n  temperature: 0\n  max_tokens: 512\n  reasoning_effort: low\nagent:\n  temperature: 1\n  max_tokens: 2048\n  reasoning_efforts: [minimal]\n"), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GKG_AGENT_TEMPERATURE", "0")
	t.Setenv("GKG_AGENT_REASONING_EFFORTS", "minimal, low, high")
	cfg, err := Load(file)
	if err != nil {
		t.Fatalf("load generation settings: %v", err)
	}
	if cfg.Judge.Auth != AuthStatic || cfg.Agent.Auth != AuthStatic || cfg.GCP.Credentials != "metadata" ||
		cfg.Judge.Temperature == nil || *cfg.Judge.Temperature != 0 ||
		cfg.Judge.MaxTokens == nil || *cfg.Judge.MaxTokens != 512 ||
		cfg.Judge.ReasoningEffort == nil || *cfg.Judge.ReasoningEffort != "low" ||
		cfg.Agent.Temperature == nil || *cfg.Agent.Temperature != 0 ||
		cfg.Agent.MaxTokens == nil || *cfg.Agent.MaxTokens != 2048 ||
		!slices.Equal(cfg.Agent.ReasoningEfforts, []string{"minimal", "low", "high"}) {
		t.Fatalf("unexpected effective configuration judge=%+v agent=%+v", cfg.Judge, cfg.Agent)
	}
	t.Setenv("GKG_AGENT_REASONING_EFFORTS", "")
	cfg, err = Load(file)
	if err != nil || cfg.Agent.ReasoningEfforts == nil || len(cfg.Agent.ReasoningEfforts) != 0 {
		t.Fatalf("empty env list: %v, %v", cfg.Agent.ReasoningEfforts, err)
	}
}

func TestLLMSettingsRanges(t *testing.T) {
	for _, role := range []string{"judge", "agent"} {
		t.Run(role, func(t *testing.T) {
			for _, tc := range []struct {
				name, field, yaml, env string
				invalid                bool
			}{
				{name: "unset"},
				{"zero temperature", "temperature", "0", "", false},
				{"max temperature", "temperature", "2", "", false},
				{"negative temperature", "temperature", "-0.1", "", true},
				{"above max temperature", "temperature", "2.1", "", true},
				{"NaN temperature", "temperature", ".nan", "", true},
				{"infinite temperature", "temperature", "0", "+Inf", true},
				{"min tokens", "max_tokens", "1", "", false},
				{"zero tokens", "max_tokens", "0", "", true},
				{"env rejects negative tokens", "max_tokens", "1", "-1", true},
				{"env rejects high temperature", "temperature", "0", "3", true},
				{"env corrects high temperature", "temperature", "3", "2", false},
				{"env corrects zero tokens", "max_tokens", "0", "1", false},
			} {
				t.Run(tc.name, func(t *testing.T) {
					var file string
					if tc.field != "" {
						file = filepath.Join(t.TempDir(), "config.yaml")
						yaml := fmt.Sprintf("%s:\n  %s: %s\n", role, tc.field, tc.yaml)
						if err := os.WriteFile(file, []byte(yaml), 0600); err != nil {
							t.Fatal(err)
						}
					}
					if tc.env != "" {
						t.Setenv(strings.ToUpper("GKG_"+role+"_"+tc.field), tc.env)
					}
					_, err := Load(file)
					if tc.invalid {
						if err == nil || !strings.Contains(err.Error(), role+"."+tc.field) {
							t.Errorf("Load error = %v, want range error for %s.%s", err, role, tc.field)
						}
					} else if err != nil {
						t.Errorf("Load error = %v, want success", err)
					}
				})
			}
		})
	}
}
