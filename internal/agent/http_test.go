package agent

import (
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/alessiosavi/go-knowledge-graph/internal/api"
	"github.com/alessiosavi/go-knowledge-graph/internal/config"
)

// doReq drives one request against a handler.
func doReq(t *testing.T, h http.Handler, method, target, body string, hdr map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, target, strings.NewReader(body))
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	return rr
}

func postAsk(t *testing.T, h http.Handler, body string) *httptest.ResponseRecorder {
	t.Helper()
	return doReq(t, h, http.MethodPost, "/v1/agent/ask", body, map[string]string{"Content-Type": "application/json"})
}

// enabledCfg returns an enabled agent configuration pointed at url.
func enabledCfg(url string) config.Config {
	cfg := config.Config{}
	cfg.Agent.Enabled = true
	cfg.Agent.BaseURL = url
	cfg.Agent.APIKey = "sekret-key"
	cfg.Agent.Model = "stub-model"
	return cfg
}

func TestAskDisabled(t *testing.T) {
	s := NewServer(config.Config{}, nil, nil)
	rr := postAsk(t, s.AskHandler(), `{"question":"anything"}`)
	if rr.Code != http.StatusNotImplemented {
		t.Fatalf("disabled ask: code = %d, want 501", rr.Code)
	}
	var body struct {
		Error string `json:"error"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil || body.Error != "agent_disabled" {
		t.Errorf("disabled ask body = %s, want error agent_disabled", rr.Body.String())
	}

	rr = doReq(t, s.StatusHandler(), http.MethodGet, "/v1/agent/status", "", nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("status: code = %d, want 200", rr.Code)
	}
	var status struct {
		Enabled bool `json:"enabled"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &status); err != nil || status.Enabled {
		t.Errorf("status = %s, want enabled false", rr.Body.String())
	}

	on := NewServer(enabledCfg("http://127.0.0.1:1"), nil, nil)
	rr = doReq(t, on.StatusHandler(), http.MethodGet, "/v1/agent/status", "", nil)
	if err := json.Unmarshal(rr.Body.Bytes(), &status); err != nil || !status.Enabled {
		t.Errorf("enabled status = %s, want enabled true", rr.Body.String())
	}
}

func TestAskRequiresQuestion(t *testing.T) {
	s := NewServer(enabledCfg("http://127.0.0.1:1"), nil, nil)
	for _, body := range []string{`{}`, `{"question":"   "}`, `{`} {
		rr := postAsk(t, s.AskHandler(), body)
		if rr.Code != http.StatusBadRequest {
			t.Errorf("ask %q: code = %d, want 400", body, rr.Code)
		}
	}
}

func TestAskProviderDownClassified(t *testing.T) {
	stub := newLLMStub(t,
		stubResponse{status: http.StatusInternalServerError, body: `{"error":{"message":"boom"}}`},
	)
	s := NewServer(enabledCfg(stub.srv.URL), nil, nil)
	s.llm.backoff = nil // one attempt keeps the test fast

	rr := postAsk(t, s.AskHandler(), `{"question":"anything"}`)
	if rr.Code != http.StatusBadGateway {
		t.Fatalf("provider 500: code = %d, want 502 (body %s)", rr.Code, rr.Body.String())
	}
	body := rr.Body.String()
	if !strings.Contains(body, "provider") {
		t.Errorf("body = %s, want a classified provider error", body)
	}
	if strings.Contains(body, stub.srv.URL) || strings.Contains(body, "sekret-key") {
		t.Errorf("body leaks the endpoint or key: %s", body)
	}

	// Transport failure: a closed listener must classify cleanly too.
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := l.Addr().String()
	_ = l.Close()
	down := NewServer(enabledCfg("http://"+addr), nil, nil)
	down.llm.backoff = nil
	rr = postAsk(t, down.AskHandler(), `{"question":"anything"}`)
	if rr.Code != http.StatusBadGateway {
		t.Fatalf("provider down: code = %d, want 502", rr.Code)
	}
	if strings.Contains(rr.Body.String(), addr) {
		t.Errorf("body leaks the endpoint: %s", rr.Body.String())
	}
}

func TestAskTimesOut(t *testing.T) {
	release := make(chan struct{})
	slow := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		select {
		case <-release:
		case <-r.Context().Done():
		}
	}))
	t.Cleanup(slow.Close)
	cfg := enabledCfg(slow.URL)
	cfg.Agent.TimeoutS = 1
	s := NewServer(cfg, nil, nil)

	start := time.Now()
	rr := postAsk(t, s.AskHandler(), `{"question":"hang?"}`)
	elapsed := time.Since(start)
	close(release)
	if rr.Code != http.StatusGatewayTimeout {
		t.Fatalf("timeout: code = %d, want 504 (body %s)", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), "timed out") {
		t.Errorf("body = %s, want the timeout detail", rr.Body.String())
	}
	if elapsed > 2500*time.Millisecond {
		t.Errorf("ask took %v, want it bounded by the 1s timeout", elapsed)
	}
}

// TestAskBearerPolicy pins that the mounted handlers inherit the REST
// bearer policy (the 501 with a valid token proves the route is reached).
func TestAskBearerPolicy(t *testing.T) {
	apiCfg := config.Config{}
	apiCfg.Server.APIKey = "sekret"
	rest := api.NewServer(apiCfg, nil, nil)
	agentSrv := NewServer(config.Config{}, nil, nil)
	rest.Mount("POST /v1/agent/ask", agentSrv.AskHandler())
	rest.Mount("GET /v1/agent/status", agentSrv.StatusHandler())
	h := rest.Handler()

	rr := postAsk(t, h, `{"question":"hi"}`)
	if rr.Code != http.StatusUnauthorized {
		t.Errorf("ask without token: code = %d, want 401", rr.Code)
	}
	rr = doReq(t, h, http.MethodPost, "/v1/agent/ask", `{"question":"hi"}`, map[string]string{
		"Authorization": "Bearer sekret",
	})
	if rr.Code != http.StatusNotImplemented {
		t.Errorf("ask with token (disabled agent): code = %d, want the handler's 501", rr.Code)
	}

	rr = doReq(t, h, http.MethodGet, "/v1/agent/status", "", nil)
	if rr.Code != http.StatusUnauthorized {
		t.Errorf("status without token: code = %d, want 401", rr.Code)
	}
	rr = doReq(t, h, http.MethodGet, "/v1/agent/status", "", map[string]string{
		"Authorization": "Bearer sekret",
	})
	if rr.Code != http.StatusOK {
		t.Errorf("status with token: code = %d, want 200", rr.Code)
	}
}
