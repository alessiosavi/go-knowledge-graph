package main

import (
	"fmt"
	"os"
	"strings"
	"testing"
)

func TestJavaAuditRequiresNamedTarget(t *testing.T) {
	const marker = "edgecheck_java_source="
	const target = "ARGS:json.params.arguments.source"
	for _, tc := range []struct {
		name, rule, data string
		baseline         bool
		want             bool
	}{
		{"named Java match", "944100", "Matched Data: java.lang.Runtime found within " + target, false, true},
		{"chained Java match", "944110", "Matched Data: runtime found within MATCHED_VARS:" + target, false, true},
		{"raw body only", "944100", "Matched Data: java.lang.Runtime found within REQUEST_BODY", false, false},
		{"other rule", "941100", "Matched Data: payload found within " + target, false, false},
		{"old Java match", "944100", "Matched Data: java.lang.Runtime found within " + target, true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fixture := fmt.Sprintf(`{"transaction":{"unique_id":"java","request":{"uri":"/mcp?%s1"},"messages":[{"details":{"ruleId":%q,"data":%q}}]}}`, marker, tc.rule, tc.data)
			records, err := parseAudit(fixture)
			if err != nil {
				t.Fatal(err)
			}
			if got := javaProbeMatched(records, map[string]bool{"java": tc.baseline}, marker, target); got != tc.want {
				t.Fatalf("Java named-target proof = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestAuditCapturedFixture(t *testing.T) {
	// Captured from the pinned edge image's DetectionOnly XSS request. Only
	// transport metadata and the unrelated response body were removed.
	data, err := os.ReadFile("../../deploy/edge/testdata/audit-positive.json")
	if err != nil {
		t.Fatal(err)
	}
	records, err := parseAudit(string(data))
	if err != nil || len(records) == 0 {
		t.Fatal("captured audit must parse")
	}
	counts := countAudit(records, nil)
	if counts.Records != 1 || counts.Matches != 5 || counts.WithoutMessages != 0 || counts.Rules["941100"] != 1 {
		t.Fatal("must count details.ruleId, including the XSS positive control")
	}
	baseline := map[string]bool{}
	for _, record := range records {
		baseline[record.ID] = true
	}
	if countAudit(records, baseline).Records != 0 {
		t.Fatal("baseline transaction IDs must not be recounted")
	}
}

func TestAuditRejectsMalformedRecords(t *testing.T) {
	for _, input := range []string{
		`"transaction":{"unique_id":"x"}}`,
		`{"transaction":`,
		`{"transaction":{"unique_id":"x","messages":[{"details":{"ruleId":}}]}}`,
		`{"transaction":{"unique_id":"x","messages":[{"details":{}}]}}`,
		`{"transaction":{"messages":[]}}`,
	} {
		if _, err := parseAudit(input); err == nil {
			t.Error("malformed audit record accepted")
		}
	}
	records, err := parseAudit("startup text\n" + `{"transaction":{"unique_id":"empty","messages":[]}}` + "\naccess log\n")
	counts := countAudit(records, nil)
	if err != nil || counts.Records != 1 || counts.WithoutMessages != 1 || counts.Matches != 0 {
		t.Fatal("ordinary audit transactions must be counted separately")
	}
	// A malformed record following valid evidence must still fail closed.
	if _, err := parseAudit(strings.Repeat(`{"transaction":{"unique_id":"ok"}}`+"\n", 2) + `{"transaction":`); err == nil {
		t.Error("truncated trailing record accepted")
	}
}

func TestAuditPositiveControlIsTheAttack(t *testing.T) {
	health := auditRecord{ID: "health", URI: "/healthz", Rules: []string{"920350"}}
	attack := auditRecord{ID: "attack", URI: "/mcp?edgecheck_attack_marker=1"}
	if positiveAudit([]auditRecord{health, attack}, nil) {
		t.Fatal("background or message-less records cannot satisfy the positive control")
	}
	attack.Rules = []string{"941100"}
	if !positiveAudit([]auditRecord{health, attack}, nil) {
		t.Fatal("attack rule matches must satisfy the positive control")
	}
	if positiveAudit([]auditRecord{attack}, map[string]bool{"attack": true}) {
		t.Fatal("baseline attack cannot satisfy a fresh positive control")
	}
}
