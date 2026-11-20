package ingest_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/alessiosavi/go-knowledge-graph/internal/ingest"
	"github.com/alessiosavi/go-knowledge-graph/internal/judge"
)

func TestPipelineStoredJudgeInputMatchesWire(t *testing.T) {
	env := newPipelineEnv(t)
	env.cfg.Ingest.ChunkDedup = "off"
	source, text := uniqueText(t)
	const query = `MATCH (:Entity)-[r]->(:Entity) WHERE r.doc_id = $doc RETURN properties(r) AS props`
	type capture struct {
		body  string
		props map[string]any
	}
	requests := make(chan capture, 2)
	var docID, providerVerdict atomic.Value
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Error(err)
		}
		rows, err := env.st.RunReadQuery(r.Context(), query, map[string]any{"doc": docID.Load()}, 2)
		var props map[string]any
		if err != nil || len(rows.Rows) != 1 {
			t.Errorf("typed edge before provider response = %+v, %v", rows, err)
		} else {
			props, _ = rows.Rows[0]["props"].(map[string]any)
		}
		select {
		case requests <- capture{body: string(body), props: props}:
		default:
			t.Error("unexpected judge request")
		}
		content := fmt.Sprintf(`{"verdict":%q,"reason":"provider reason"}`, providerVerdict.Load())
		_, _ = fmt.Fprintf(w, `{"choices":[{"message":{"content":%q}}]}`, content)
	}))
	defer server.Close()
	env.cfg.Judge.BaseURL = server.URL
	var previousBody, previousKey string
	for _, opt := range []struct {
		mode, verdict string
		enable        bool
	}{{"all", "confirmed", false}, {"all", "confirmed", true}, {"all", "refuted", true}, {"off", "confirmed", true}} {
		providerVerdict.Store(opt.verdict)
		env.cfg.Judge.Mode = opt.mode
		env.cfg.Judge.StoreInputs = opt.enable
		p := ingest.NewPipeline(env.cfg, env.st, env.mock, nil, textLoader)
		client := judge.NewClient(env.cfg)
		client.SetBackoff(nil)
		if opt.mode == "all" {
			if err := p.SetJudge(judge.NewCached(client, env.st, client.Model())); err != nil {
				t.Fatal(err)
			}
		}
		doc, err := p.Submit(t.Context(), ingest.Input{Kind: "text", Source: source, Text: text, Title: "Wire <fixture>"})
		if err != nil || doc.Noop {
			t.Fatalf("submit = %+v, %v", doc, err)
		}
		t.Cleanup(func() { _ = env.st.DeleteDocument(context.Background(), doc.DocumentID) })
		docID.Store(doc.DocumentID)
		if err := p.Process(t.Context(), doc.DocumentID); err != nil {
			t.Fatal(err)
		}
		after, err := env.st.RunReadQuery(t.Context(), query, map[string]any{"doc": doc.DocumentID}, 2)
		if opt.verdict == "refuted" {
			if err != nil || len(after.Rows) != 0 || len(requests) != 1 {
				t.Fatalf("refuted edges=%v requests=%d err=%v", after.Rows, len(requests), err)
			}
			recorded := <-requests
			if recorded.body != previousBody || recorded.props["judge_input"] == nil {
				t.Fatal("refuted iteration did not judge the original stored bytes")
			}
			audits, err := env.st.RunReadQuery(t.Context(), `MATCH (a:RejectedFact {doc_id:$doc})
RETURN a.judge_verdict AS verdict, a.reason AS reason`, map[string]any{"doc": doc.DocumentID}, 2)
			if err != nil || len(audits.Rows) != 1 || audits.Rows[0]["verdict"] != "refuted" || audits.Rows[0]["reason"] != "provider reason" {
				t.Fatalf("refuted audits = %+v, %v", audits, err)
			}
			if err := env.st.DeleteDocument(t.Context(), doc.DocumentID); err != nil {
				t.Fatal(err)
			}
			continue
		}
		if err != nil || len(after.Rows) != 1 {
			t.Fatalf("typed edge after Process = %+v, %v", after, err)
		}
		props, _ := after.Rows[0]["props"].(map[string]any)
		if opt.mode == "off" {
			if len(requests) != 0 || props["judge_input"] == nil || props["judge"] != "" {
				t.Fatalf("mode off requests = %d, edge = %v", len(requests), props)
			}
			break
		}
		if len(requests) != 1 {
			t.Fatalf("mode all made %d provider calls, want 1", len(requests))
		}
		recorded := <-requests
		var body struct {
			Messages []struct{ Role, Content string }
		}
		if err := json.Unmarshal([]byte(recorded.body), &body); err != nil || len(body.Messages) != 2 || body.Messages[1].Role != "user" {
			t.Fatalf("judge wire body = %s, %v", recorded.body, err)
		}
		payload := body.Messages[1].Content
		key := judge.RequestKey([]byte(payload), client.Model())
		if cached, ok, err := env.st.JudgeVerdict(t.Context(), key); err != nil || !ok || cached.DocID != doc.DocumentID {
			t.Fatalf("cache row = %+v, %v, %v", cached, ok, err)
		}
		if props["judge"] != "confirmed" || props["judge_input"] != nil || recorded.props["judge"] != "" {
			t.Fatalf("before edge = %v, after edge = %v", recorded.props, props)
		}
		if opt.enable {
			if recorded.body != previousBody || key != previousKey || recorded.props["judge_input"] != payload {
				t.Fatal("stored request, HTTP body, or cache key differs from legacy verification")
			}
			gen, _ := recorded.props["judge_gen"].(string)
			decoded, err := hex.DecodeString(gen)
			hash := fmt.Sprintf("%x", sha256.Sum256([]byte(payload)))
			if err != nil || len(decoded) != 16 || recorded.props["input_hash"] != hash || props["input_hash"] != hash || props["judge_gen"] != gen {
				t.Fatalf("before edge = %v, after edge = %v", recorded.props, props)
			}
		} else {
			previousBody, previousKey = recorded.body, key
			if len(recorded.props) != 12 || len(props) != 12 {
				t.Fatalf("unexpected legacy properties: before = %v, after = %v", recorded.props, props)
			}
		}
		// Each mode gets a cache miss for exactly the same v1 bytes.
		if err := env.st.DeleteDocument(t.Context(), doc.DocumentID); err != nil {
			t.Fatal(err)
		}
	}
}
