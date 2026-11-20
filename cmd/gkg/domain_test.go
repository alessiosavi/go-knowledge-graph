package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/alessiosavi/go-knowledge-graph/internal/api"
	"github.com/alessiosavi/go-knowledge-graph/internal/config"
	"github.com/alessiosavi/go-knowledge-graph/internal/domain"
	"github.com/alessiosavi/go-knowledge-graph/internal/graph"
	"github.com/alessiosavi/go-knowledge-graph/internal/graph/graphtest"
	"github.com/alessiosavi/go-knowledge-graph/internal/ingest"
)

func TestDomainServeUnreachable(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := l.Addr().String()
	_ = l.Close()
	var out, stderr bytes.Buffer
	code := run([]string{"domain", "move", "--server", "http://" + addr, "acme", "zorbo"}, &out, &stderr)
	if code != 1 || !strings.Contains(stderr.String(), "gkg domain: serve unreachable at http://"+addr+":") {
		t.Fatalf("exit=%d stderr=%s", code, &stderr)
	}
}
func TestDomainCLIAdmin(t *testing.T) {
	t.Setenv("GKG_API_KEY", "acme-secret")
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/admin/domains/rename" || r.Header.Get("Authorization") != "Bearer acme-secret" {
			t.Errorf("request=%s auth=%q", r.URL, r.Header.Get("Authorization"))
		}
		w.WriteHeader(202)
		fmt.Fprint(w, `{"op_id":"acme","state":"completed"}`)
	}))
	defer s.Close()
	var out, stderr bytes.Buffer
	if code := run([]string{"domain", "rename", "acme", "Quillon Ltd", "--server", s.URL}, &out, &stderr); code != 0 {
		t.Fatalf("exit=%d %s", code, &stderr)
	}
}

func TestDomainCLIProductConflict(t *testing.T) {
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusConflict)
		fmt.Fprintln(w, `{"error":"domain_has_products","detail":"domain acme is bound to product store shop"}`)
	}))
	defer s.Close()
	for _, args := range [][]string{{"move", "doc", "acme"}, {"merge", "acme", "zorbo"}, {"undo", "op"}} {
		var out, stderr bytes.Buffer
		code := run(append(append([]string{"domain"}, args...), "--server", s.URL), &out, &stderr)
		if code != 1 || !strings.Contains(stderr.String(), "domain is bound to a product store") || out.Len() != 0 {
			t.Errorf("%v exit=%d stdout=%s stderr=%s", args, code, &out, &stderr)
		}
	}
}
func TestDomainEvaluate(t *testing.T) {
	st := graphtest.Store(t)
	ctx := context.Background()
	for i := 0; i < 4; i++ {
		slug := fmt.Sprintf("eval-quillon-%d", i/2)
		r, err := st.UpsertDocument(ctx, graph.DocumentInput{Kind: "text", Source: fmt.Sprintf("%s-%d", t.Name(), i), Hash: "quillon"})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = st.DeleteDocument(ctx, r.ID) })
		if err := st.AssignDomain(ctx, graph.Assignment{DocID: r.ID, Domain: slug, Profile: domain.Profile{slug: 1}, CreatedBy: "admin"}); err != nil {
			t.Fatal(err)
		}
		if err := st.SetStatus(ctx, r.ID, graph.StatusDone, ""); err != nil {
			t.Fatal(err)
		}
	}
	// CLI reads the configured throwaway store; no provider is constructed.
	t.Setenv("GKG_MEMGRAPH_URI", os.Getenv(graphtest.URIEnv))
	profiles, err := st.DomainProfiles(ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	expected := domain.Evaluate(profiles, domain.DefaultParams())
	want := fmt.Sprintf("docs=%d top_choice_accuracy=%.6f auto_attach_share=%.6f unseen_false_attach_share=%.6f\n", expected.Docs, expected.TopChoiceAccuracy, expected.AutoAttachShare, expected.UnseenFalseAttachShare)
	var out, stderr bytes.Buffer
	if code := run([]string{"domain", "evaluate"}, &out, &stderr); code != 0 || out.String() != want {
		t.Fatalf("exit=%d out=%s err=%s", code, &out, &stderr)
	}
	t.Log(strings.TrimSpace(out.String()))
}

func TestDomainNearest(t *testing.T) {
	profiles := []domain.DocProfile{{DocID: "acme", Domain: "a", Profile: domain.Profile{"quillon": 1}}, {DocID: "zorbo", Domain: "b", Profile: domain.Profile{"quillon": 1}}, {DocID: "flimble", Domain: "c", Profile: domain.Profile{"trindle": 1}}}
	if id, score := nearestDomain("a", profiles); id != "b" || score < .99 {
		t.Fatalf("nearest=%s score=%f", id, score)
	}
}

type cliDomainOperator struct{}

func (*cliDomainOperator) Submit(context.Context, ingest.Input) (ingest.Result, error) {
	return ingest.Result{}, nil
}
func (*cliDomainOperator) RunDomainOp(context.Context, ingest.DomainOpRequest) (graph.DomainOp, error) {
	return graph.DomainOp{OpID: "acme", State: "completed"}, nil
}
func (*cliDomainOperator) UndoDomainOp(_ context.Context, id string) (graph.DomainOp, error) {
	return graph.DomainOp{OpID: id + "-undo", State: "completed"}, nil
}

func TestDomainCLIDefaultLoopback(t *testing.T) {
	cfg := config.Config{}
	cfg.Server.APIKey = "quillon-secret"
	s := httptest.NewServer(api.NewServer(cfg, nil, &cliDomainOperator{}).Handler())
	defer s.Close()
	_, port, err := net.SplitHostPort(strings.TrimPrefix(s.URL, "http://"))
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("GKG_SERVER_ADDR", ":"+port)
	t.Setenv("GKG_API_KEY", cfg.Server.APIKey)
	for _, args := range [][]string{{"move", "acme", "zorbo"}, {"undo", "--op-id", "acme"}} {
		var out, stderr bytes.Buffer
		code := run(append([]string{"domain"}, args...), &out, &stderr)
		var result struct{ OpID, State string }
		if code != 0 || json.Unmarshal(out.Bytes(), &result) != nil || result.State != "completed" {
			t.Fatalf("%v exit=%d stdout=%s stderr=%s", args, code, &out, &stderr)
		}
	}
}
func TestDomainCLIInvalidOpIDFlag(t *testing.T) {
	var out, stderr bytes.Buffer
	code := run([]string{"domain", "rename", "acme", "Zorbo", "--op-id", "quillon"}, &out, &stderr)
	if code != 2 || !strings.Contains(stderr.String(), "--op-id") {
		t.Fatalf("exit=%d stderr=%s", code, &stderr)
	}
}
