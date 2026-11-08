package graph

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/alessiosavi/go-knowledge-graph/internal/config"
)

func TestMethodStoreSkipsWithoutEnv(t *testing.T) {
	if os.Getenv(testURIEnv) != "" {
		t.Skip("GKG_TEST_MEMGRAPH_URI is set; the skip-path assertion only runs when it is unset")
	}
	t.Run("helper", func(t *testing.T) { TestStore(t) })
}

func TestOpenBadURI(t *testing.T) {
	cfg := config.Config{}
	cfg.Memgraph.URI = "://not-a-uri"
	if _, err := Open(context.Background(), cfg); err == nil {
		t.Fatal("Open with malformed URI: want error")
	}
}

func TestOpenUnreachable(t *testing.T) {
	cfg := config.Config{}
	cfg.Memgraph.URI = "bolt://127.0.0.1:1"
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := Open(ctx, cfg); err == nil {
		t.Fatal("Open against an unreachable server: want error")
	}
}

func TestBootstrapIdempotent(t *testing.T) {
	st := TestStore(t)
	ctx := context.Background()
	if err := st.Bootstrap(ctx); err != nil {
		t.Fatalf("Bootstrap is not idempotent: %v", err)
	}
	// Assert against raw SHOW CONSTRAINT INFO output (not the production
	// helper) so a false positive in the helper cannot make this pass.
	for _, c := range []struct{ label, prop string }{
		{"Document", "id"},
		{"Chunk", "id"},
		{"Entity", "key"},
	} {
		if !rawUniqueConstraintExists(t, st, c.label, c.prop) {
			t.Errorf("unique constraint on (:%s).%s missing after Bootstrap", c.label, c.prop)
		}
	}
}

// TestConstraintExistsIgnoresNonUniqueTypes pins that a non-unique
// constraint (EXISTS / data-type) on the same label+property does not fool
// the existence check into skipping the unique constraint.
func TestConstraintExistsIgnoresNonUniqueTypes(t *testing.T) {
	st := TestStore(t)
	ctx := context.Background()
	const label = "ConstraintTypeProbe"

	cleanup := func() {
		_ = st.runAuto(context.Background(), "DROP CONSTRAINT ON (n:"+label+") ASSERT EXISTS (n.id)", nil)
		_ = st.runAuto(context.Background(), "DROP CONSTRAINT ON (n:"+label+") ASSERT n.id IS UNIQUE", nil)
	}
	cleanup()
	t.Cleanup(cleanup)

	if err := st.runAuto(ctx, "CREATE CONSTRAINT ON (n:"+label+") ASSERT EXISTS (n.id)", nil); err != nil {
		t.Fatalf("create exists constraint: %v", err)
	}

	ok, err := st.constraintExists(ctx, label, "id")
	if err != nil {
		t.Fatalf("constraintExists: %v", err)
	}
	if ok {
		t.Error("constraintExists reported true for an EXISTS (non-unique) constraint")
	}

	if err := st.runAuto(ctx, "CREATE CONSTRAINT ON (n:"+label+") ASSERT n.id IS UNIQUE", nil); err != nil {
		t.Fatalf("create unique constraint: %v", err)
	}
	ok, err = st.constraintExists(ctx, label, "id")
	if err != nil {
		t.Fatalf("constraintExists after unique create: %v", err)
	}
	if !ok {
		t.Error("constraintExists reported false for an existing unique constraint")
	}
}

// rawUniqueConstraintExists queries SHOW CONSTRAINT INFO directly with an
// independent match, deliberately not sharing production matching code.
func rawUniqueConstraintExists(t *testing.T, st *Store, label, prop string) bool {
	t.Helper()
	recs, err := st.runRead(context.Background(), "SHOW CONSTRAINT INFO", nil)
	if err != nil {
		t.Fatalf("SHOW CONSTRAINT INFO: %v", err)
	}
	for _, r := range recs {
		tv, _ := r.Get("constraint type")
		if !strings.Contains(fmt.Sprint(tv), "unique") {
			continue
		}
		lv, _ := r.Get("label")
		if fmt.Sprint(lv) != label {
			continue
		}
		pv, _ := r.Get("properties")
		switch p := pv.(type) {
		case string:
			if p == prop {
				return true
			}
		case []any:
			for _, e := range p {
				if fmt.Sprint(e) == prop {
					return true
				}
			}
		}
	}
	return false
}

func TestMigrateCommand(t *testing.T) {
	uri := os.Getenv(testURIEnv)
	if uri == "" {
		t.Skipf("set %s to run integration tests", testURIEnv)
	}
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	bin := filepath.Join(t.TempDir(), "gkg")
	build := exec.Command("go", "build", "-o", bin, "./cmd/gkg")
	build.Dir = root
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build gkg: %v\n%s", err, out)
	}
	for i := 1; i <= 2; i++ {
		cmd := exec.Command(bin, "migrate")
		cmd.Dir = t.TempDir()
		cmd.Env = append(os.Environ(), "GKG_MEMGRAPH_URI="+uri)
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("gkg migrate run %d: %v\n%s", i, err, out)
		}
	}
}
