package graph_test

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/alessiosavi/go-knowledge-graph/internal/graph"
	"github.com/alessiosavi/go-knowledge-graph/internal/graph/graphtest"
)

func TestJudgeVerdictPersistence(t *testing.T) {
	st := graphtest.Store(t)
	ctx := t.Context()
	uri := os.Getenv(graphtest.URIEnv)
	key := fmt.Sprintf("zorbo-verdict-%d", time.Now().UnixNano())
	t.Cleanup(func() { rawRun(t, uri, fmt.Sprintf(`MATCH (v:JudgeVerdict {key: '%s'}) DELETE v`, key)) })
	if _, ok, err := st.JudgeVerdict(ctx, key); err != nil || ok {
		t.Fatalf("missing: ok=%v err=%v", ok, err)
	}
	for _, status := range []string{"confirmed", "unclear", "refuted"} {
		want := graph.JudgeVerdict{Key: key, Status: status, Reason: "Zorbó \"星\"", Model: "test-model", PromptVersion: "1", Confidence: 0.8, At: time.Now().UTC()}
		if err := st.PutJudgeVerdict(ctx, want); err != nil {
			t.Fatal(err)
		}
		got, ok, err := st.JudgeVerdict(ctx, key)
		if err != nil || !ok || got != want {
			t.Fatalf("verdict = %+v, ok=%v err=%v, want %+v", got, ok, err, want)
		}
	}
	if n := rawCount(t, uri, fmt.Sprintf(`MATCH (v:JudgeVerdict {key: '%s'}) RETURN count(v) AS n`, key)); n != 1 {
		t.Fatalf("MERGE created %d nodes", n)
	}
	if !rawUniqueConstraintExists(t, uri, "JudgeVerdict", "key") {
		t.Error("JudgeVerdict(key) unique constraint missing")
	}
	if !rawIndexExists(t, uri, "JudgeVerdict", "key") {
		t.Error("JudgeVerdict(key) index missing")
	}
	for _, bad := range []graph.JudgeVerdict{{Key: key, Status: "maybe"}, {Status: "confirmed"}} {
		if err := st.PutJudgeVerdict(ctx, bad); err == nil {
			t.Errorf("invalid cache entry accepted: %+v", bad)
		}
	}
}

func TestMigrateKeepsJudgeVerdict(t *testing.T) {
	st := graphtest.MigrationStore(t)
	ctx := context.Background()
	uri := os.Getenv(graphtest.URIEnv)
	key := fmt.Sprintf("quillon-cache-%d", time.Now().UnixNano())
	want := graph.JudgeVerdict{Key: key, Status: "confirmed", Reason: "supported", Model: "test-model", PromptVersion: "1", At: time.Now().UTC()}
	if err := st.PutJudgeVerdict(ctx, want); err != nil {
		t.Fatal(err)
	}
	rawRun(t, uri, fmt.Sprintf(`CREATE (:Entity {key: '%s'})`, key))
	t.Cleanup(func() { rawRun(t, uri, fmt.Sprintf(`MATCH (n) WHERE n.key = '%s' DETACH DELETE n`, key)) })
	rawRun(t, uri, `MERGE (m:GkgMeta {id: 'extraction_schema'}) SET m.value = 2`)
	result, err := st.MigrateExtractionSchema(ctx)
	if err != nil || !result.Migrated || result.EntitiesRemoved < 1 {
		t.Fatalf("migration = %+v err=%v", result, err)
	}
	got, ok, err := st.JudgeVerdict(ctx, key)
	if err != nil || !ok || got != want {
		t.Fatalf("cache lost after migration: %+v ok=%v err=%v", got, ok, err)
	}
}

// TestJudgeVerdictWithoutTime keeps a node that lacks a parsable time
// usable: the verdict is returned with a zero At instead of poisoning the key.
func TestJudgeVerdictWithoutTime(t *testing.T) {
	st := graphtest.Store(t)
	uri := os.Getenv(graphtest.URIEnv)
	key := fmt.Sprintf("zorbo-untimed-%d", time.Now().UnixNano())
	rawRun(t, uri, fmt.Sprintf(`CREATE (:JudgeVerdict {key: '%s', status: 'unclear', reason: 'r', model: 'm', prompt_version: '1'})`, key))
	t.Cleanup(func() { rawRun(t, uri, fmt.Sprintf(`MATCH (v:JudgeVerdict {key: '%s'}) DELETE v`, key)) })
	got, ok, err := st.JudgeVerdict(t.Context(), key)
	if err != nil || !ok || got.Status != "unclear" || !got.At.IsZero() {
		t.Fatalf("verdict = %+v ok=%v err=%v, want unclear with zero time", got, ok, err)
	}
}
