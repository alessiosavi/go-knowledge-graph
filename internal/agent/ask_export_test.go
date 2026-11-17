package agent

import (
	"context"
	"strings"
	"testing"

	"github.com/alessiosavi/go-knowledge-graph/internal/config"
)

func TestExportedAsk(t *testing.T) {
	stub := newLLMStub(t, contentReply("Acme owns Zorbo."))
	cfg := config.Config{}
	cfg.Agent.BaseURL = stub.srv.URL
	cfg.Agent.Model = "stub-model"
	s := NewServer(cfg, nil, nil)
	if _, err := s.Ask(context.Background(), AskRequest{Question: "Who?", Domain: "quillon"}); err == nil || err.Error() != "domain scope not supported yet" {
		t.Fatalf("domain error=%v", err)
	}
	if len(stub.requests) != 0 {
		t.Fatal("scoped ask called provider")
	}
	r, err := s.Ask(context.Background(), AskRequest{Question: "Who owns Zorbo?"})
	if err != nil || !strings.Contains(r.Answer, "Acme") {
		t.Fatalf("Ask=%+v err=%v", r, err)
	}
}
