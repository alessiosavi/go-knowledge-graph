package main

import (
	"bytes"
	"fmt"
	"strings"
	"testing"

	"github.com/alessiosavi/go-knowledge-graph/internal/llmauth"
)

func TestGCPV3CommandsRefuseBeforeGraph(t *testing.T) {
	t.Setenv("GKG_CONFIG", "")
	t.Setenv("GKG_MEMGRAPH_URI", "invalid://graph-must-not-open")
	t.Setenv("GKG_GCP_ROUTE_HOST", "aiplatform.googleapis.com")
	t.Setenv("GKG_GCP_ROUTE_PROJECT", "test-project")
	t.Setenv("GKG_GCP_ROUTE_LOCATION", "global")
	t.Setenv("GKG_JUDGE_AUTH", "gcp")
	t.Setenv("GKG_JUDGE_API_KEY", "")
	t.Setenv("GKG_JUDGE_BASE_URL", "https://aiplatform.googleapis.com/v1/projects/test-project/locations/global/endpoints/openapi")
	for _, args := range [][]string{
		{"judge", "rejudge", "-plan", "-out", "manifest.jsonl"},
		{"judge", "rejudge", "-judge", "-manifest", "manifest.jsonl", "-budget-attempts", "1", "-out", "verdicts.jsonl"},
		{"repair-validity", "-plan", "-manifest", "manifest.jsonl", "-verdicts", "verdicts.jsonl", "-out", "result"},
	} {
		var out, stderr bytes.Buffer
		if code := run(args, &out, &stderr); code != 2 || out.Len() != 0 || !strings.Contains(stderr.String(), "auth=gcp") {
			t.Errorf("%v: exit=%d stdout=%q stderr=%q", args, code, out.String(), stderr.String())
		}
	}
}

func TestJudgeAuthExitCodes(t *testing.T) {
	for _, tc := range []struct {
		err  error
		want int
	}{
		{&llmauth.AuthPermanent{Detail: "IAM denied"}, 2},
		{&llmauth.AuthTransient{Detail: "metadata timeout"}, 3},
		{fmt.Errorf("graph failed"), 1},
	} {
		if got := judgeRunErrorExitCode(fmt.Errorf("attempt stopped: %w", tc.err)); got != tc.want {
			t.Errorf("exit=%d, want %d for %v", got, tc.want, tc.err)
		}
	}
}
