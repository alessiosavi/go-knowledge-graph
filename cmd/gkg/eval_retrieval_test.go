package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/alessiosavi/go-knowledge-graph/internal/graph"
	"github.com/alessiosavi/go-knowledge-graph/internal/graph/graphtest"
	"github.com/alessiosavi/go-knowledge-graph/internal/reteval"
)

func TestEvalRetrievalFlags(t *testing.T) {
	for _, args := range [][]string{{"eval-retrieval"}, {"eval-retrieval", "--k", "0"}, {"eval-retrieval", "--k", "bogus"}, {"eval-retrieval", "--bogus"}} {
		var out, err bytes.Buffer
		if code := run(args, &out, &err); code != 2 {
			t.Errorf("%v exit=%d err=%s", args, code, err.String())
		}
	}
	var out, err bytes.Buffer
	if code := run([]string{"eval-retrieval", "--help"}, &out, &err); code != 0 {
		t.Fatalf("help exit=%d", code)
	}
	for _, flag := range []string{"-questions", "-agent", "-replay", "-k", "-out"} {
		if !strings.Contains(err.String(), flag) {
			t.Errorf("help missing %s", flag)
		}
	}
	out.Reset()
	usage(&out)
	if !strings.Contains(out.String(), "eval-retrieval") {
		t.Fatal("usage missing eval-retrieval")
	}
}

func TestEvalRetrievalAgentWarning(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(configPath, []byte("{}\n"), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GKG_CONFIG", configPath)
	t.Setenv("GKG_AGENT_ENABLED", "true")
	var out, stderr bytes.Buffer
	code := run([]string{"eval-retrieval", "--agent", "--questions", filepath.Join(t.TempDir(), "missing.json")}, &out, &stderr)
	if code != 2 || !strings.Contains(stderr.String(), "warning: --agent calls the configured agent provider and spends money") {
		t.Fatalf("exit=%d stderr=%s", code, stderr.String())
	}
}

func TestEvalRetrievalAgentDisabled(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(configPath, []byte("{}\n"), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GKG_CONFIG", configPath)
	t.Setenv("GKG_AGENT_ENABLED", "false")
	t.Setenv("GKG_MEMGRAPH_URI", "bolt://127.0.0.1:1")
	questions := filepath.Join(dir, "questions.json")
	if err := os.WriteFile(questions, []byte(`{"schema":1,"questions":[{"id":"n","stratum":"null","question":"Where is Zorbo?","absent":["Zorbo"]}]}`), 0600); err != nil {
		t.Fatal(err)
	}
	var out, stderr bytes.Buffer
	code := run([]string{"eval-retrieval", "--agent", "--questions", questions}, &out, &stderr)
	if code != 2 || stderr.String() != "gkg eval-retrieval: agent is not enabled (agent.enabled=false)\n" || out.Len() != 0 {
		t.Fatalf("exit=%d stdout=%s stderr=%s", code, out.String(), stderr.String())
	}
}

func TestEvalRetrievalAgentReplayConflict(t *testing.T) {
	var out, stderr bytes.Buffer
	code := run([]string{"eval-retrieval", "--agent", "--replay", "stored.json", "--questions", "missing.json"}, &out, &stderr)
	if code != 2 || !strings.Contains(stderr.String(), "--agent and --replay") || strings.Contains(stderr.String(), "warning:") || out.Len() != 0 {
		t.Fatalf("exit=%d stdout=%s stderr=%s", code, out.String(), stderr.String())
	}
}

func TestEvalRetrievalCLI(t *testing.T) {
	st := graphtest.Store(t)
	ctx := context.Background()
	token := fmt.Sprintf("quillon%d", time.Now().UnixNano())
	doc, err := st.UpsertDocument(ctx, graph.DocumentInput{Kind: "text", Source: token + "/acme.txt", Hash: token})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := st.DeleteDocument(ctx, doc.ID); err != nil {
			t.Error(err)
		}
	})
	if err := st.PutChunks(ctx, doc.ID, []graph.Chunk{{Text: "Acme " + token + " owns Zorbo"}}); err != nil {
		t.Fatal(err)
	}
	if err := st.SetStatus(ctx, doc.ID, graph.StatusDone, ""); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	questions := filepath.Join(dir, "questions.json")
	set := reteval.QuestionSet{Schema: 1, Questions: []reteval.Question{{ID: "s", Stratum: "single", Question: token, Gold: []reteval.Anchor{{Source: "acme.txt", Quote: token + " owns"}}, Answer: []reteval.AnswerPart{{Names: []string{"Acme"}}}}}}
	b, err := json.Marshal(set)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(questions, b, 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GKG_MEMGRAPH_URI", os.Getenv(graphtest.URIEnv))
	t.Setenv("GKG_CONFIG", filepath.Join(dir, "config.yaml"))
	if err := os.WriteFile(filepath.Join(dir, "config.yaml"), []byte("{}\n"), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GKG_EMBEDDER_ENABLED", "false")
	t.Setenv("GKG_AGENT_ENABLED", "false")
	// Like every other graph command, eval-retrieval bootstraps the store:
	// a graph without the text indexes must not fail the first question.
	migrationQuery(t, "DROP TEXT INDEX chunk_text", nil)
	path := filepath.Join(dir, "report.json")
	var stdout, stderr bytes.Buffer
	if code := run([]string{"eval-retrieval", "--questions", questions, "--k", "1,5", "--out", path}, &stdout, &stderr); code != 0 {
		t.Fatalf("exit=%d stderr=%s", code, stderr.String())
	}
	b, err = os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var report reteval.Report
	if err := json.Unmarshal(b, &report); err != nil {
		t.Fatal(err)
	}
	if len(report.Questions) != 1 || report.Strata["single"]["chunk/question/recall@5"].Mean != 1 || !strings.Contains(stdout.String(), "95% CI") {
		t.Fatalf("report=%+v summary=%s", report, stdout.String())
	}
}

func TestEvalRetrievalPartialAgentFailure(t *testing.T) {
	st := graphtest.Store(t)
	ctx := context.Background()
	token := fmt.Sprintf("quillon%d", time.Now().UnixNano())
	doc, err := st.UpsertDocument(ctx, graph.DocumentInput{Kind: "text", Source: token + "/acme.txt", Hash: token})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := st.DeleteDocument(ctx, doc.ID); err != nil {
			t.Error(err)
		}
	})
	if err := st.PutChunks(ctx, doc.ID, []graph.Chunk{{Text: "Acme " + token + " owns Zorbo"}}); err != nil {
		t.Fatal(err)
	}
	if err := st.SetStatus(ctx, doc.ID, graph.StatusDone, ""); err != nil {
		t.Fatal(err)
	}
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Messages []struct{ Role, Content string } `json:"messages"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Error(err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		for _, msg := range req.Messages {
			if msg.Role == "user" && strings.HasSuffix(msg.Content, " unavailable") {
				w.WriteHeader(http.StatusUnauthorized)
				fmt.Fprint(w, `{"error":{"message":"synthetic provider failure"}}`)
				return
			}
		}
		fmt.Fprint(w, `{"choices":[{"message":{"role":"assistant","content":"Acme"},"finish_reason":"stop"}]}`)
	}))
	defer provider.Close()
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(configPath, []byte("{}\n"), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GKG_CONFIG", configPath)
	t.Setenv("GKG_MEMGRAPH_URI", os.Getenv(graphtest.URIEnv))
	t.Setenv("GKG_EMBEDDER_ENABLED", "false")
	t.Setenv("GKG_AGENT_ENABLED", "true")
	t.Setenv("GKG_AGENT_BASE_URL", provider.URL)
	t.Setenv("GKG_AGENT_API_KEY", "synthetic-test-key")
	t.Setenv("GKG_AGENT_STREAM", "false")
	good := reteval.Question{ID: "scored", Stratum: "single", Question: token, Gold: []reteval.Anchor{{Quote: token + " owns"}}, Answer: []reteval.AnswerPart{{Names: []string{"Acme"}}}}
	bad := good
	bad.ID, bad.Question = "failed", token+" unavailable"
	for _, tc := range []struct {
		name string
		qs   []reteval.Question
		want int
	}{
		{"partial", []reteval.Question{bad, good}, 1},
		{"all-failed", []reteval.Question{bad}, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			questions := filepath.Join(dir, tc.name+"-questions.json")
			b, err := json.Marshal(reteval.QuestionSet{Schema: 1, Questions: tc.qs})
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(questions, b, 0600); err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(dir, tc.name+"-report.json")
			var stdout, stderr bytes.Buffer
			code := run([]string{"eval-retrieval", "--agent", "--questions", questions, "--out", path}, &stdout, &stderr)
			b, err = os.ReadFile(path)
			if err != nil {
				t.Fatalf("report not written: %v; exit=%d stderr=%s", err, code, stderr.String())
			}
			var report struct {
				Questions []reteval.QuestionResult     `json:"questions"`
				Failed    []struct{ ID, Error string } `json:"failed"`
			}
			if err := json.Unmarshal(b, &report); err != nil {
				t.Fatal(err)
			}
			if code != 1 || len(report.Questions) != tc.want || len(report.Failed) != 1 || report.Failed[0].ID != "failed" || report.Failed[0].Error == "" || !strings.Contains(stderr.String(), "1 failed question") || !strings.Contains(stdout.String(), "95% CI") {
				t.Fatalf("exit=%d report=%+v stdout=%s stderr=%s", code, report, stdout.String(), stderr.String())
			}
		})
	}
}
