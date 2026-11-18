package agent

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"
)

// A base URL that fails to parse must not echo the endpoint, or a credential
// embedded in it, to stream clients or the server log.
func TestBuildRequestFailureHidesEndpoint(t *testing.T) {
	const secret = "url-embedded-secret"
	for _, stream := range []bool{false, true} {
		var logs bytes.Buffer
		previous := slog.Default()
		slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
		cfg := enabledCfg("http://provider.invalid:bad-port/?api-key=" + secret)
		cfg.Agent.Stream = stream
		s := NewServer(cfg, nil, nil)
		s.llm.backoff = nil
		rr := postStream(t, s.StreamHandler(), "anything")
		slog.SetDefault(previous)
		if body := rr.Body.String(); strings.Contains(body, secret) || strings.Contains(body, "provider.invalid") {
			t.Errorf("stream=%v: client payload exposes the endpoint", stream)
		}
		if strings.Contains(logs.String(), secret) {
			t.Errorf("stream=%v: server log exposes the endpoint credential", stream)
		}
	}
}
