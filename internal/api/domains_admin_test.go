package api_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"

	"github.com/alessiosavi/go-knowledge-graph/internal/api"
	"github.com/alessiosavi/go-knowledge-graph/internal/config"
	"github.com/alessiosavi/go-knowledge-graph/internal/graph"
	"github.com/alessiosavi/go-knowledge-graph/internal/graph/graphtest"
	"github.com/alessiosavi/go-knowledge-graph/internal/ingest"
	"github.com/neo4j/neo4j-go-driver/v6/neo4j"
)

type domainAdminFake struct {
	op    graph.DomainOp
	err   error
	calls int
}

func (*domainAdminFake) Submit(context.Context, ingest.Input) (ingest.Result, error) {
	return ingest.Result{}, nil
}
func (f *domainAdminFake) RunDomainOp(context.Context, ingest.DomainOpRequest) (graph.DomainOp, error) {
	f.calls++
	if f.op.OpID != "" {
		return f.op, f.err
	}
	return graph.DomainOp{OpID: "acme", State: "running"}, f.err
}
func (f *domainAdminFake) UndoDomainOp(context.Context, string) (graph.DomainOp, error) {
	f.calls++
	if f.op.OpID != "" {
		return f.op, f.err
	}
	return graph.DomainOp{OpID: "undo-acme", State: "completed"}, f.err
}
func TestDomainAdminLoopback(t *testing.T) {
	for _, action := range []string{"move", "merge", "rename", "undo"} {
		f := &domainAdminFake{}
		h := api.NewServer(config.Config{}, nil, f).Handler()
		r := httptest.NewRequest("POST", "/v1/admin/domains/"+action, bytes.NewBufferString(`{}`))
		r.RemoteAddr = "192.0.2.1:5000"
		r.Header.Set("X-Forwarded-For", "127.0.0.1")
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != http.StatusForbidden || f.calls != 0 {
			t.Fatalf("%s code=%d calls=%d", action, w.Code, f.calls)
		}
	}
}
func TestMoveNotDone(t *testing.T) { testDomainAdminError(t, graph.ErrNotDone, 409, "not_done") }
func TestDomainOpsNeedV4(t *testing.T) {
	testDomainAdminError(t, graph.ErrSchemaNotV4, 409, "schema_not_v4")
}
func testDomainAdminError(t *testing.T, err error, status int, code string) {
	t.Helper()
	f := &domainAdminFake{err: err}
	h := api.NewServer(config.Config{}, nil, f).Handler()
	r := httptest.NewRequest("POST", "/v1/admin/domains/move", bytes.NewBufferString(`{"document_ids":["acme"],"domain":"zorbo"}`))
	r.RemoteAddr = "127.0.0.1:5000"
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != status || !bytes.Contains(w.Body.Bytes(), []byte(code)) {
		t.Fatalf("code=%d body=%s", w.Code, w.Body)
	}
}

func TestDomainAdminGraphGuards(t *testing.T) {
	st := graphtest.Store(t)
	p := ingest.NewPipeline(config.Config{}, st, nil, nil, nil)
	h := api.NewServer(config.Config{}, st, p).Handler()
	call := func(action, body string) *httptest.ResponseRecorder {
		r := httptest.NewRequest("POST", "/v1/admin/domains/"+action, bytes.NewBufferString(body))
		r.RemoteAddr = "[::1]:5000"
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w
	}
	doc, err := st.UpsertDocument(t.Context(), graph.DocumentInput{Kind: "text", Source: t.Name(), Hash: "quillon"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.DeleteDocument(context.Background(), doc.ID) })
	body := fmt.Sprintf(`{"document_ids":["%s"],"domain":"zorbo","op_id":"api-acme-move"}`, doc.ID)
	w := call("move", body)
	if w.Code != 409 || !bytes.Contains(w.Body.Bytes(), []byte("schema_not_v4")) {
		t.Fatalf("v3 code=%d body=%s", w.Code, w.Body)
	}
	for _, tc := range []struct{ action, body string }{{"merge", `{"from":"acme","into":"zorbo"}`}, {"rename", `{"domain":"acme","name":"Quillon"}`}, {"undo", `{"op_id":"missing"}`}} {
		w = call(tc.action, tc.body)
		if w.Code != 409 || !bytes.Contains(w.Body.Bytes(), []byte("schema_not_v4")) {
			t.Fatalf("v3 %s code=%d body=%s", tc.action, w.Code, w.Body)
		}
	}
	before, err := st.RunReadQuery(t.Context(), `MATCH (o:DomainOp {op_id:'api-acme-move'}) RETURN count(o) AS n`, nil, 0)
	if err != nil || before.Rows[0]["n"] != int64(0) {
		t.Fatalf("v3 wrote operation: %+v err=%v", before, err)
	}
	adminGraphWrite(t, `MATCH (m:GkgMeta {id:'extraction_schema'}) SET m.value=4`)
	t.Cleanup(func() {
		adminGraphWrite(t, `MATCH (m:GkgMeta {id:'extraction_schema'}) SET m.value=3`)
		adminGraphWrite(t, `MATCH (o:DomainOp) WHERE o.op_id STARTS WITH 'api-acme-' DELETE o`)
	})
	w = call("move", body)
	if w.Code != 409 || !bytes.Contains(w.Body.Bytes(), []byte("not_done")) {
		t.Fatalf("busy code=%d body=%s", w.Code, w.Body)
	}
	op, err := st.DomainOperation(t.Context(), "api-acme-move")
	if !errors.Is(err, graph.ErrNotFound) {
		t.Fatalf("busy operation persisted=%+v err=%v", op, err)
	}
	stored, err := st.Document(t.Context(), doc.ID)
	if err != nil || stored.Domain != "" || stored.Status != graph.StatusReceived {
		t.Fatalf("busy move changed document: %+v err=%v", stored, err)
	}
	for _, tc := range []struct {
		action, body string
		status       int
	}{
		{"move", `{"document_ids":["missing"],"domain":"zorbo","op_id":"api-acme-missing"}`, 404},
		{"move", `{"document_ids":["missing"],"domain":"Bad:Slug"}`, 400},
		{"merge", `{"from":"missing","into":"missing-too"}`, 404},
		{"rename", `{"domain":"missing","name":"Quillon"}`, 404},
		{"undo", `{"op_id":"missing"}`, 404},
	} {
		w = call(tc.action, tc.body)
		if w.Code != tc.status {
			t.Fatalf("%s %s code=%d body=%s", tc.action, tc.body, w.Code, w.Body)
		}
	}
}
func adminGraphWrite(t *testing.T, query string) {
	t.Helper()
	ctx := context.Background()
	driver, err := neo4j.NewDriver(os.Getenv(graphtest.URIEnv), neo4j.NoAuth())
	if err != nil {
		t.Fatal(err)
	}
	defer driver.Close(ctx)
	session := driver.NewSession(ctx, neo4j.SessionConfig{AccessMode: neo4j.AccessModeWrite})
	defer session.Close(ctx)
	res, err := session.Run(ctx, query, nil)
	if err == nil {
		_, err = res.Consume(ctx)
	}
	if err != nil {
		t.Fatal(err)
	}
}

func TestDomainAdminProgressResult(t *testing.T) {
	for _, tc := range []struct {
		action    string
		op        graph.DomainOp
		field, id string
	}{
		{"merge", graph.DomainOp{OpID: "acme", State: "incomplete", Remaining: []string{"quillon"}}, "remaining", "quillon"},
		{"undo", graph.DomainOp{OpID: "acme-undo", State: "completed", Skipped: []string{"zorbo"}}, "skipped", "zorbo"},
	} {
		t.Run(tc.action, func(t *testing.T) {
			f := &domainAdminFake{op: tc.op}
			h := api.NewServer(config.Config{}, nil, f).Handler()
			r := httptest.NewRequest("POST", "/v1/admin/domains/"+tc.action, bytes.NewBufferString(`{"op_id":"acme"}`))
			r.RemoteAddr = "127.0.0.1:5000"
			w := httptest.NewRecorder()
			h.ServeHTTP(w, r)
			var result map[string]any
			if w.Code != 202 || json.Unmarshal(w.Body.Bytes(), &result) != nil {
				t.Fatalf("code=%d body=%s", w.Code, w.Body)
			}
			ids, ok := result[tc.field].([]any)
			if result["state"] != tc.op.State || result["op_id"] != tc.op.OpID || !ok || len(ids) != 1 || ids[0] != tc.id {
				t.Fatalf("result=%v", result)
			}
		})
	}
}
