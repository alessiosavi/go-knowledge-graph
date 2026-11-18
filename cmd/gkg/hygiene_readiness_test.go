package main

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/alessiosavi/go-knowledge-graph/internal/config"
	"github.com/alessiosavi/go-knowledge-graph/internal/domain"
)

func stoplistOverlayFixture(t *testing.T, kind string) (path, diagnostic string) {
	t.Helper()
	if kind == "unset" {
		return "", ""
	}
	dir := t.TempDir()
	path = filepath.Join(dir, "stoplist.txt")
	content := "# Acme overlay\n\n  Zorbo  \nQuillon Ltd\n"
	switch kind {
	case "missing":
		return path, "no such file or directory"
	case "directory":
		return dir, "is a directory"
	case "oversized":
		content = strings.Repeat("z", 128*1024)
		diagnostic = "token too long"
	}
	if err := os.WriteFile(path, []byte(content), 0600); err != nil {
		t.Fatal(err)
	}
	return path, diagnostic
}

func TestPreflightStoplistOverlay(t *testing.T) {
	for _, kind := range []string{"missing", "directory", "oversized", "valid", "unset"} {
		t.Run(kind, func(t *testing.T) {
			st := preflightCommandStore(t)
			migrationCommandDocument(t, st, "done")
			migrationQuery(t, `CREATE (:GkgMeta {id:'extraction_schema',value:3})`, nil)
			path, diagnostic := stoplistOverlayFixture(t, kind)
			t.Setenv("GKG_INGEST_STOPLIST_FILE", path)
			before := migrationQuery(t, `MATCH (n) RETURN labels(n),properties(n) ORDER BY id(n)`, nil)
			var out, errb bytes.Buffer
			code := run([]string{"preflight"}, &out, &errb)
			want := 10
			if diagnostic != "" {
				want = 3
				if !strings.Contains(errb.String(), "stoplist overlay unreadable: ") || !strings.Contains(errb.String(), diagnostic) {
					t.Errorf("overlay diagnostic=%q; want unreadable and %q", &errb, diagnostic)
				}
			}
			if code != want || out.Len() != 0 {
				t.Errorf("exit=%d stdout=%q stderr=%q; want %d and empty stdout", code, &out, &errb, want)
			}
			if after := migrationQuery(t, `MATCH (n) RETURN labels(n),properties(n) ORDER BY id(n)`, nil); !reflect.DeepEqual(before, after) {
				t.Error("preflight changed the graph")
			}
		})
	}
}

func TestCommandsRejectUnreadableStoplistOverlay(t *testing.T) {
	for _, command := range []string{"serve", "mcp", "migrate"} {
		for _, kind := range []string{"missing", "directory", "oversized"} {
			t.Run(command+"/"+kind, func(t *testing.T) {
				st := preflightCommandStore(t)
				id, _ := migrationCommandDocument(t, st, "chunked")
				migrationQuery(t, `CREATE (:GkgMeta {id:'extraction_schema',value:4})`, nil)
				path, diagnostic := stoplistOverlayFixture(t, kind)
				t.Setenv("GKG_INGEST_STOPLIST_FILE", path)
				// Keep a missed overlay check bounded and ahead of provider setup.
				t.Setenv("GKG_EXTRACTOR_MODE", "invalid-provider")
				var out, errb bytes.Buffer
				if code := run([]string{command}, &out, &errb); code != 1 || !strings.Contains(errb.String(), "stoplist overlay unreadable: ") || !strings.Contains(errb.String(), diagnostic) {
					t.Errorf("exit=%d stderr=%q; want startup refusal with overlay diagnostic %q", code, &errb, diagnostic)
				}
				doc, err := st.Document(context.Background(), id)
				if err != nil || doc.Status != "chunked" {
					t.Errorf("queued document=%+v err=%v", doc, err)
				}
			})
		}
	}
}

func TestBuildPipelineStoplistOverlayGate(t *testing.T) {
	for _, marker := range []int{3, 4} {
		for _, kind := range []string{"missing", "valid", "unset"} {
			t.Run(fmt.Sprintf("v%d/%s", marker, kind), func(t *testing.T) {
				st := preflightCommandStore(t)
				migrationQuery(t, `CREATE (:GkgMeta {id:'extraction_schema',value:$marker})`, map[string]any{"marker": marker})
				path, diagnostic := stoplistOverlayFixture(t, kind)
				cfg := config.Config{
					Extractor: config.ExtractorConfig{Mode: "mock"},
					Ingest:    config.IngestConfig{StoplistFile: path},
				}
				pipe, _, err := buildPipeline(context.Background(), cfg, st)
				if marker >= 4 && diagnostic != "" {
					if err == nil || !strings.Contains(err.Error(), "stoplist overlay unreadable: ") {
						t.Errorf("marker=%d overlay=%s error=%v; want overlay refusal", marker, kind, err)
					}
				} else if err != nil || pipe.WriteMode().Hygiene != (marker >= 4) {
					t.Errorf("marker=%d overlay=%s pipeline=%v error=%v", marker, kind, pipe, err)
				}
			})
		}
	}
}

// assignedRestartMarkers are the restart states the store resumes from: the
// assign phase is durable whatever the old marker was, including a legacy
// graph whose marker node the assign phase itself created without a value.
var assignedRestartMarkers = map[string]string{
	"v3":     `CREATE (:GkgMeta {id:'extraction_schema',value:3,v4_phase:'assigned'})`,
	"v2":     `CREATE (:GkgMeta {id:'extraction_schema',value:2,v4_phase:'assigned'})`,
	"legacy": `CREATE (:GkgMeta {id:'extraction_schema',v4_phase:'assigned'})`,
}

func TestPreflightAssignedRestart(t *testing.T) {
	for marker, create := range assignedRestartMarkers {
		for _, seed := range []string{"", "unmatched"} {
			t.Run(marker+"/seed="+seed, func(t *testing.T) {
				st := preflightCommandStore(t)
				migrationCommandDocument(t, st, "done")
				migrationQuery(t, create, nil)
				before := migrationQuery(t, `MATCH (m:GkgMeta) RETURN properties(m)`, nil)
				if seed != "" {
					path := filepath.Join(t.TempDir(), "seed.json")
					if err := os.WriteFile(path, []byte(`{"schema":1,"rules":[{"match":"https://quillon.invalid/","domain":"quillon"}]}`), 0600); err != nil {
						t.Fatal(err)
					}
					seed = path
				}
				t.Setenv("GKG_DOMAIN_SEED_FILE", seed)
				t.Setenv("GKG_INGEST_STOPLIST_FILE", "")
				var out, errb bytes.Buffer
				if code := run([]string{"preflight"}, &out, &errb); code != 10 || out.Len() != 0 {
					t.Errorf("exit=%d stdout=%q stderr=%q; want 10", code, &out, &errb)
				}
				if after := migrationQuery(t, `MATCH (m:GkgMeta) RETURN properties(m)`, nil); !reflect.DeepEqual(before, after) {
					t.Errorf("preflight changed the restart marker: %v -> %v", before, after)
				}
			})
		}
	}
}

// TestSchemaOnlyAssignedRestartWithoutSeed: once the assign phase is
// durable, the migration needs no seed, so a seed that became unreadable
// must not block `migrate --schema-only` after preflight said 10.
func TestSchemaOnlyAssignedRestartWithoutSeed(t *testing.T) {
	for marker, create := range assignedRestartMarkers {
		t.Run(marker, func(t *testing.T) {
			st := preflightCommandStore(t)
			migrationCommandDocument(t, st, "done")
			migrationQuery(t, create, nil)
			t.Setenv("GKG_DOMAIN_SEED_FILE", filepath.Join(t.TempDir(), "gone.json"))
			t.Setenv("GKG_INGEST_STOPLIST_FILE", "")
			var out, errb bytes.Buffer
			if code := run([]string{"migrate", "--schema-only"}, &out, &errb); code != 0 {
				t.Fatalf("exit=%d stderr=%q; want 0 without a seed in the assigned phase", code, &errb)
			}
			rows := migrationQuery(t, `MATCH (m:GkgMeta {id:'extraction_schema'}) RETURN m.value AS value, m.v4_phase AS phase`, nil)
			if len(rows) != 1 {
				t.Fatalf("marker rows=%d; want 1", len(rows))
			}
			value, _ := rows[0].Get("value")
			phase, _ := rows[0].Get("phase")
			if fmt.Sprint(value) != "4" || phase != nil {
				t.Fatalf("marker after restart value=%v v4_phase=%v; want 4 and none", value, phase)
			}
		})
	}
}

// TestPreflightEmptyGraphStoplistOverlay: startup on an empty graph writes the
// v4 marker and the pipeline then refuses an unreadable overlay, so preflight
// fails (3) here too instead of passing the deploy to a serve that cannot start.
func TestPreflightEmptyGraphStoplistOverlay(t *testing.T) {
	for _, marker := range []string{"none", "v3"} {
		for _, kind := range []string{"missing", "directory", "valid", "unset"} {
			t.Run(marker+"/"+kind, func(t *testing.T) {
				preflightCommandStore(t)
				if marker == "v3" {
					migrationQuery(t, `CREATE (:GkgMeta {id:'extraction_schema',value:3})`, nil)
				}
				path, diagnostic := stoplistOverlayFixture(t, kind)
				t.Setenv("GKG_INGEST_STOPLIST_FILE", path)
				t.Setenv("GKG_DOMAIN_SEED_FILE", "")
				before := migrationQuery(t, `MATCH (n) RETURN labels(n),properties(n) ORDER BY id(n)`, nil)
				var out, errb bytes.Buffer
				code := run([]string{"preflight"}, &out, &errb)
				want := 0
				if diagnostic != "" {
					want = 3
					if !strings.Contains(errb.String(), "stoplist overlay unreadable: ") || !strings.Contains(errb.String(), diagnostic) {
						t.Errorf("overlay diagnostic=%q; want unreadable and %q", &errb, diagnostic)
					}
				}
				if code != want || out.Len() != 0 {
					t.Errorf("exit=%d stdout=%q stderr=%q; want %d and empty stdout", code, &out, &errb, want)
				}
				if after := migrationQuery(t, `MATCH (n) RETURN labels(n),properties(n) ORDER BY id(n)`, nil); !reflect.DeepEqual(before, after) {
					t.Error("preflight changed the graph")
				}
			})
		}
	}
}

// TestPreflightCurrentStoplistOverlay: a current graph with an unreadable
// overlay fails preflight (3) instead of letting serve fail after the deploy.
func TestPreflightCurrentStoplistOverlay(t *testing.T) {
	preflightCommandStore(t)
	migrationQuery(t, `CREATE (:GkgMeta {id:'extraction_schema',value:4})`, nil)
	path, diagnostic := stoplistOverlayFixture(t, "missing")
	t.Setenv("GKG_INGEST_STOPLIST_FILE", path)
	var out, errb bytes.Buffer
	if code := run([]string{"preflight"}, &out, &errb); code != 3 || !strings.Contains(errb.String(), diagnostic) {
		t.Fatalf("exit=%d stderr=%q; want 3 with %q", code, &errb, diagnostic)
	}
}

// TestMigrationParamsPerFieldDefaults: a zero field takes its default, as in
// runtime detection, so the migration and ingest decide alike.
func TestMigrationParamsPerFieldDefaults(t *testing.T) {
	var cfg config.Config
	cfg.Domain.Tau = 0.07
	opt, err := migrationOptions(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if want := (domain.Params{Tau: 0.07, Delta: 0.02, TauOverride: 0.15}); opt.Params != want {
		t.Fatalf("params = %+v, want %+v", opt.Params, want)
	}
}
