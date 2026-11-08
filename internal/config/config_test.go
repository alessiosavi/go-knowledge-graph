package config_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/alessiosavi/go-knowledge-graph/internal/config"
)

func TestLoadDefaults(t *testing.T) {
	cfg, err := config.Load("")
	if err != nil {
		t.Fatalf("Load: %v", err)
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
