package graph

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/alessiosavi/go-knowledge-graph/internal/config"
	"github.com/neo4j/neo4j-go-driver/v6/neo4j"
)

// legacyDropDriver controls the race between SHOW INDEX INFO and DROP while
// keeping schema reads and successful drops against the real database.
type legacyDropDriver struct {
	neo4j.Driver
	remove bool
	err    error
}

func (d legacyDropDriver) NewSession(ctx context.Context, cfg neo4j.SessionConfig) neo4j.Session {
	return legacyDropSession{Session: d.Driver.NewSession(ctx, cfg), remove: d.remove, err: d.err}
}

type legacyDropSession struct {
	neo4j.Session
	remove bool
	err    error
}

func (s legacyDropSession) Run(ctx context.Context, query string, params map[string]any, cfg ...func(*neo4j.TransactionConfig)) (neo4j.Result, error) {
	if query == "DROP TEXT INDEX chunk_text" || query == "DROP TEXT INDEX entity_text" {
		if s.remove {
			// Model another bootstrap winning the drop after our inspection.
			res, err := s.Session.Run(ctx, query, params, cfg...)
			if err != nil {
				return nil, err
			}
			if _, err := res.Consume(ctx); err != nil {
				return nil, err
			}
		}
		return nil, s.err
	}
	return s.Session.Run(ctx, query, params, cfg...)
}

func TestEnsureTextIndexesConcurrentLegacyDrop(t *testing.T) {
	uri := os.Getenv("GKG_TEST_MEMGRAPH_URI")
	if uri == "" {
		t.Skip("requires a disposable Memgraph integration database")
	}
	ctx := context.Background()
	cfg := config.Config{}
	cfg.Memgraph.URI = uri
	st, err := Open(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close(ctx) })
	if err := st.Bootstrap(ctx); err != nil {
		t.Fatal(err)
	}
	for _, remove := range []bool{true, false} {
		name := "already_dropped"
		if !remove {
			name = "still_exists"
		}
		t.Run(name, func(t *testing.T) {
			t.Cleanup(func() {
				if err := st.Bootstrap(ctx); err != nil {
					t.Error(err)
				}
			})
			for _, ddl := range []string{"CREATE TEXT INDEX chunk_text ON :Chunk(text)", "CREATE TEXT INDEX entity_text ON :Entity(name, aliases)"} {
				if err := st.runAuto(ctx, ddl, nil); err != nil {
					t.Fatal(err)
				}
			}
			dropErr := errors.New("no such text index (concurrent drop)")
			racing := Store{driver: legacyDropDriver{Driver: st.driver, remove: remove, err: dropErr}}
			err := racing.EnsureTextIndexes(ctx)
			if remove && err != nil {
				t.Errorf("already-dropped legacy index: %v", err)
			} else if !remove && !errors.Is(err, dropErr) {
				t.Errorf("drop error = %v, want original error while index still exists", err)
			}
			recs, err := st.runRead(ctx, "SHOW INDEX INFO", nil)
			if err != nil {
				t.Fatal(err)
			}
			for _, name := range []string{"chunk_text", "entity_text"} {
				found := false
				for _, r := range recs {
					found = found || strings.Contains(getString(r, "index type"), "name: "+name+")")
				}
				if found == remove {
					t.Errorf("legacy index %s exists = %t, want %t", name, found, !remove)
				}
			}
		})
	}
}
