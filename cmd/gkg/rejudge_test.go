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
	"sync/atomic"
	"testing"
	"time"

	"github.com/alessiosavi/go-knowledge-graph/internal/graph"
	"github.com/alessiosavi/go-knowledge-graph/internal/graph/graphtest"
	"github.com/alessiosavi/go-knowledge-graph/internal/judge"
)

func TestRejudgeFlags(t *testing.T) {
	for _, args := range [][]string{
		{"-plan", "-judge", "-out", "m"},
		{"-plan", "-ids", "audit", "-limit", "0", "-out", "m"},
		{"-judge", "-manifest", "m", "-out", "v"},
		{"-plan", "-out", "m", "-raise-cap"},
	} {
		var out, stderr bytes.Buffer
		if code := run(append([]string{"judge", "rejudge"}, args...), &out, &stderr); code != 2 ||
			!strings.Contains(stderr.String(), "usage: gkg judge rejudge") {
			t.Errorf("%v: exit=%d stderr=%s", args, code, &stderr)
		}
	}
}

func TestRejudgeCLIResume(t *testing.T) {
	var requests atomic.Int32
	dir := t.TempDir()
	attempts := filepath.Join(dir, "attempts.json")
	manifest, verdicts := filepath.Join(dir, "manifest.jsonl"), filepath.Join(dir, "verdicts.jsonl")
	doc, labels := rejudgeCLIFixture(t, func(w http.ResponseWriter, _ *http.Request) {
		n := requests.Add(1)
		data, err := os.ReadFile(attempts)
		var ledger struct{ Attempts int }
		if err != nil || json.Unmarshal(data, &ledger) != nil || ledger.Attempts != int(n) {
			t.Errorf("attempt %d before durable debit: %s %v", n, data, err)
		}
		if n == 2 {
			http.Error(w, "retry", http.StatusTooManyRequests)
			return
		}
		fmt.Fprint(w, `{"choices":[{"message":{"content":"{\"verdict\":\"confirmed\",\"confidence\":0.9,\"reason\":\"supported\"}"}}]}`)
	})
	labelPath := filepath.Join(dir, "labels.jsonl")
	writeRejudgeLabels(t, labelPath, labels[:2])
	before := graphtest.Snapshot(t)
	plan := []string{"-plan", "-ids", labelPath, "-out", manifest}
	if code, out := callRejudge(t, plan...); code != 0 || !strings.Contains(out, "planned=2") || !strings.Contains(out, "calls_needed=2") || requests.Load() != 0 {
		t.Fatalf("plan exit=%d calls=%d: %s", code, requests.Load(), out)
	}
	before.AssertUnchanged(t, "plan")
	original := readRejudgeFile(t, manifest)
	if !bytes.Contains(original, []byte(`"prior":"unjudged"`)) ||
		bytes.Contains(original, []byte(labels[2].ID)) {
		t.Fatal("manifest failed to restrict to labelled confirmed/unjudged edges")
	}
	firstRequest := firstRejudgeRequest(t, original)
	if code, _ := callRejudge(t, plan...); code == 0 || !bytes.Equal(original, readRejudgeFile(t, manifest)) {
		t.Fatal("plan overwrote an existing manifest")
	}

	judgeArgs := func(cap string, extra ...string) (int, string) {
		return callRejudge(t, append([]string{"-judge", "-manifest", manifest, "-out", verdicts, "-budget-attempts", cap}, extra...)...)
	}
	if code, out := judgeArgs("2"); code != 3 || !strings.Contains(out, "GATE FAIL") || requests.Load() != 2 {
		t.Fatalf("budget stop exit=%d calls=%d: %s", code, requests.Load(), out)
	} else {
		for _, want := range []string{"fingerprint=" + firstRequest.Profile.Fingerprint, "reconciled/expected", "judged/reconciled"} {
			if !strings.Contains(out, want) {
				t.Errorf("partial gate missing %q: %s", want, out)
			}
		}
		for _, line := range strings.Split(out, "\n") {
			if fields := strings.Fields(line); len(fields) > 2 && fields[0] == "rows" &&
				(fields[1] != "2/2" || fields[2] != "1/2") {
				t.Errorf("partial gate row counts: %s", line)
			}
		}
	}
	if lines := bytes.Count(readRejudgeFile(t, verdicts), []byte("\n")); lines != 1 {
		t.Fatalf("verdict count after budget stop = %d", lines)
	}
	if code, out := callRejudge(t, "-plan", "-ids", labelPath, "-out", filepath.Join(dir, "second-plan.jsonl")); code != 0 ||
		!strings.Contains(out, "calls_needed=1") || requests.Load() != 2 {
		t.Fatalf("cache-adjusted plan exit=%d calls=%d: %s", code, requests.Load(), out)
	}
	// Removing the cache proves the completed row is recovered from the
	// append-only ledger, even when the provider would otherwise be called.
	migrationQuery(t, `MATCH (v:JudgeVerdict {doc_id: $doc}) DELETE v`, map[string]any{"doc": doc})
	if code, _ := judgeArgs("3"); code == 0 || requests.Load() != 2 {
		t.Fatal("cap changed without -raise-cap")
	}
	if code, out := judgeArgs("1"); code != 3 || requests.Load() != 2 ||
		!strings.Contains(out, "cap=2") || !strings.Contains(out, "effective_budget=1") {
		t.Errorf("lower invocation budget exit=%d calls=%d: %s", code, requests.Load(), out)
	}
	if code, out := judgeArgs("3", "-raise-cap"); code != 0 || !strings.Contains(out, "GATE PASS") || requests.Load() != 3 {
		t.Fatalf("resume exit=%d calls=%d: %s", code, requests.Load(), out)
	}
	records := bytes.Split(bytes.TrimSpace(readRejudgeFile(t, verdicts)), []byte("\n"))
	var resumed struct{ Attempts int }
	if len(records) != 2 || json.Unmarshal(records[1], &resumed) != nil || resumed.Attempts != 2 {
		t.Fatalf("retried verdict does not count both attempts across resumes: %q", records)
	}
	if code, out := judgeArgs("3"); code != 0 || requests.Load() != 3 {
		t.Fatalf("completed resume exit=%d calls=%d: %s", code, requests.Load(), out)
	}
	for _, file := range []string{manifest, verdicts, attempts} {
		fi, err := os.Stat(file)
		if err != nil || fi.Mode().Perm() != 0600 {
			t.Fatalf("permissions %s: %v %v", file, fi, err)
		}
	}
	if got := migrationQuery(t, `MATCH (v:JudgeVerdict {doc_id: $doc}) RETURN v.key`, map[string]any{"doc": doc}); len(got) != 1 {
		t.Fatalf("resume cached %d new verdicts", len(got))
	}
	migrationQuery(t, `MATCH (v:JudgeVerdict {doc_id: $doc}) DELETE v`, map[string]any{"doc": doc})
	before.AssertUnchanged(t, "judge except for verdict cache")
}

func TestRejudgeCLIReconciliation(t *testing.T) {
	doc, labels := rejudgeCLIFixture(t, func(http.ResponseWriter, *http.Request) {
		t.Error("plan called provider")
	})
	migrationQuery(t, `MATCH (h:Entity)-[r:uses {doc_id:$doc}]->(t:Entity)
SET h.name='Acme Capital', t.name=t.name + ' Platform'`, map[string]any{"doc": doc})
	canonical := labels[0]
	canonical.Head, canonical.Tail = "**ACME CAPITAL**", "`Zorbo Platform`"
	wrong := labels[1]
	wrong.Head = "Neither Name"
	dir := t.TempDir()
	labelPath := filepath.Join(dir, "labels.jsonl")
	tests := []struct {
		name string
		rows []judge.AuditRow
		code int
		want string
	}{
		{"canonical names", []judge.AuditRow{canonical}, 0, "reconciled=1"},
		{"mismatch", []judge.AuditRow{labels[0], wrong}, 0, `reason="head differs"`},
		{"all mismatched", []judge.AuditRow{wrong}, 1, "no rows to plan"},
		{"empty selection", []judge.AuditRow{}, 1, "no rows to plan"},
		{"missing", []judge.AuditRow{labels[0], {ID: "missing-id", Label: "ok"}}, 1, "missing-id"},
		{"unverifiable", []judge.AuditRow{labels[2]}, 1, labels[2].ID},
		{"duplicate", []judge.AuditRow{labels[0], labels[0]}, 2, labels[0].ID},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			writeRejudgeLabels(t, labelPath, tt.rows)
			path := filepath.Join(dir, tt.name+".jsonl")
			code, out := callRejudge(t, "-plan", "-ids", labelPath, "-out", path)
			if code != tt.code || !strings.Contains(out, tt.want) {
				t.Fatalf("exit=%d: %s", code, out)
			}
			if tt.code != 0 {
				if _, err := os.Stat(path); !os.IsNotExist(err) {
					t.Fatalf("failed plan left manifest: %v", err)
				}
			}
		})
	}
}

func TestRejudgeCLIIncomplete(t *testing.T) {
	for _, tt := range []struct {
		name    string
		labels  []string
		code    int
		message string
	}{
		{"unlabelled", []string{"", ""}, 4, "incomplete: 1 of 2 rows judged; budget reached"},
		{"labelled with unjudged skip", []string{"ok", "skip"}, 3, "GATE FAIL"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			var calls atomic.Int32
			_, rows := rejudgeCLIFixture(t, func(w http.ResponseWriter, _ *http.Request) {
				calls.Add(1)
				writeConfirmedRejudgeResponse(w)
			})
			for i, label := range tt.labels {
				rows[i].Label = label
			}
			dir := t.TempDir()
			manifest, verdicts, audit := filepath.Join(dir, "m.jsonl"), filepath.Join(dir, "v.jsonl"), filepath.Join(dir, "a.jsonl")
			writeRejudgeLabels(t, audit, rows[:2])
			if code, out := callRejudge(t, "-plan", "-ids", audit, "-out", manifest); code != 0 {
				t.Fatalf("plan exit=%d: %s", code, out)
			}
			args := []string{"-judge", "-manifest", manifest, "-out", verdicts}
			if code, out := callRejudge(t, append(args, "-budget-attempts", "1")...); code != tt.code ||
				!strings.Contains(out, tt.message) || calls.Load() != 1 {
				t.Errorf("partial run exit=%d calls=%d: %s", code, calls.Load(), out)
			}
			if tt.code == 4 {
				req := firstRejudgeRequest(t, readRejudgeFile(t, manifest))
				if code, out := callRejudge(t, append(args, "-budget-attempts", "2", "-raise-cap")...); code != 0 ||
					!strings.Contains(out, "fingerprint="+req.Profile.Fingerprint) || calls.Load() != 2 {
					t.Errorf("completed unlabelled run exit=%d calls=%d: %s", code, calls.Load(), out)
				}
			}
		})
	}
}

func TestRejudgeCLIRejectsInvalidVerdictKeys(t *testing.T) {
	for _, test := range []struct {
		name string
		edit func(map[string]json.RawMessage)
	}{
		{"wrong key", func(record map[string]json.RawMessage) { record["key"] = json.RawMessage(`"wrong"`) }},
		{"missing key", func(record map[string]json.RawMessage) { delete(record, "key") }},
	} {
		t.Run(test.name, func(t *testing.T) {
			var calls atomic.Int32
			_, rows := rejudgeCLIFixture(t, func(w http.ResponseWriter, _ *http.Request) {
				calls.Add(1)
				writeConfirmedRejudgeResponse(w)
			})
			dir := t.TempDir()
			manifest, verdicts, audit := filepath.Join(dir, "m.jsonl"), filepath.Join(dir, "v.jsonl"), filepath.Join(dir, "a.jsonl")
			writeRejudgeLabels(t, audit, rows[:2])
			if code, out := callRejudge(t, "-plan", "-ids", audit, "-out", manifest); code != 0 {
				t.Fatalf("plan exit=%d: %s", code, out)
			}
			args := []string{"-judge", "-manifest", manifest, "-out", verdicts}
			if code, out := callRejudge(t, append(args, "-budget-attempts", "1")...); code != 3 || calls.Load() != 1 {
				t.Fatalf("partial run exit=%d calls=%d: %s", code, calls.Load(), out)
			}
			var record map[string]json.RawMessage
			original := readRejudgeFile(t, verdicts)
			if err := json.Unmarshal(original, &record); err != nil {
				t.Fatal(err)
			}
			req := firstRejudgeRequest(t, readRejudgeFile(t, manifest))
			wantKey := judge.RequestKeyV2(req.RequestBytes, req.Profile.Fingerprint)
			var key string
			if err := json.Unmarshal(record["key"], &key); err != nil || key != wantKey {
				t.Errorf("verdict key = %q, want %q (decode: %v)", key, wantKey, err)
			}
			test.edit(record)
			var edited bytes.Buffer
			if err := json.NewEncoder(&edited).Encode(record); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(verdicts, edited.Bytes(), 0600); err != nil {
				t.Fatal(err)
			}
			attempts := filepath.Join(dir, "attempts.json")
			debits := readRejudgeFile(t, attempts)
			if code, out := callRejudge(t, append(args, "-budget-attempts", "2", "-raise-cap")...); code != 1 ||
				!strings.Contains(out, "request key") || calls.Load() != 1 {
				t.Errorf("invalid key exit=%d calls=%d: %s", code, calls.Load(), out)
			}
			if !bytes.Equal(readRejudgeFile(t, verdicts), edited.Bytes()) || !bytes.Equal(readRejudgeFile(t, attempts), debits) {
				t.Error("invalid key changed verdicts or attempt ledger")
			}
		})
	}
}

func writeConfirmedRejudgeResponse(w http.ResponseWriter) {
	fmt.Fprint(w, `{"choices":[{"message":{"content":"{\"verdict\":\"confirmed\",\"confidence\":0.9,\"reason\":\"supported\"}"}}]}`)
}

func rejudgeCLIFixture(t *testing.T, handler http.HandlerFunc) (string, []judge.AuditRow) {
	t.Helper()
	st := graphtest.Store(t)
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	configureRejudgeCLI(t, server.URL)
	const sentence = "Acme uses Zorbo. Acme uses Quillon. Acme uses Broken."
	tag := "rejudge-" + t.Name()
	doc, err := st.UpsertDocument(t.Context(), graph.DocumentInput{Kind: "text", Source: tag, Hash: tag, Title: "Fixture"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := st.DeleteDocument(context.Background(), doc.ID); err != nil {
			t.Error(err)
		}
	})
	chunk := doc.ID + "-c0"
	if err := st.PutChunks(t.Context(), doc.ID, []graph.Chunk{{Text: sentence, End: len(sentence)}}); err != nil {
		t.Fatal(err)
	}
	head := "#" + doc.ID + ":acme"
	ents := []graph.EntityUpsert{{Key: head, Name: "Acme", Type: "organization"}}
	var facts []graph.FactUpsert
	var labels []judge.AuditRow
	for i, name := range []string{"Zorbo", "Quillon", "Broken"} {
		key := fmt.Sprintf("%s:%d", tag, i)
		snippet := "Acme uses " + name + "."
		start, tail := strings.Index(sentence, snippet), strings.Index(sentence, name)
		if i == 2 {
			tail = len(sentence) + 1
		}
		ents = append(ents, graph.EntityUpsert{Key: key, Name: name, Type: "product"})
		facts = append(facts, graph.FactUpsert{
			HeadKey: head, TailKey: key, Rel: "uses", Confidence: 0.8,
			HeadMention: graph.Span{Start: start, End: start + 4},
			TailMention: graph.Span{Start: tail, End: tail + len(name)},
		})
		labels = append(labels, judge.AuditRow{
			ID: graph.FactAuditID(head, "uses", key, chunk), Head: "ACME", Tail: name,
			Relation: "uses", Evidence: snippet, Label: "ok",
		})
	}
	if err := st.UpsertExtractionOpts(t.Context(), doc.ID, chunk, "fake:1", ents, facts, graph.WriteOptions{}); err != nil {
		t.Fatal(err)
	}
	migrationQuery(t, `MATCH (:Entity {key:$head})-[r:uses]->(:Entity {key:$tail}) SET r.judge='confirmed'`,
		map[string]any{"head": head, "tail": ents[1].Key})
	return doc.ID, labels
}

func callRejudge(t *testing.T, args ...string) (int, string) {
	t.Helper()
	code, stdout, stderr := callRejudgeStreams(args...)
	return code, stdout + stderr
}

func callRejudgeStreams(args ...string) (int, string, string) {
	var out, stderr bytes.Buffer
	code := run(append([]string{"judge", "rejudge"}, args...), &out, &stderr)
	return code, out.String(), stderr.String()
}

func firstRejudgeRequest(t *testing.T, manifest []byte) judge.Request {
	t.Helper()
	_, rows, _ := bytes.Cut(manifest, []byte("\n"))
	line, _, _ := bytes.Cut(rows, []byte("\n"))
	var row struct{ Request judge.Request }
	if err := json.Unmarshal(line, &row); err != nil {
		t.Fatal(err)
	}
	return row.Request
}

func writeRejudgeLabels(t *testing.T, path string, rows []judge.AuditRow) {
	t.Helper()
	var buf bytes.Buffer
	for _, row := range rows {
		if err := json.NewEncoder(&buf).Encode(row); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(path, buf.Bytes(), 0600); err != nil {
		t.Fatal(err)
	}
}

func readRejudgeFile(t *testing.T, path string) []byte {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func TestRejudgeCLIStopsAfterVerdictCacheMissedWrite(t *testing.T) {
	var paid atomic.Int32
	doc, labels := rejudgeCLIFixture(t, func(w http.ResponseWriter, _ *http.Request) {
		paid.Add(1)
		fmt.Fprint(w, `{"choices":[{"message":{"content":"{\"verdict\":\"confirmed\",\"confidence\":0.85,\"reason\":\"supported\"}"}}]}`)
	})
	dir := t.TempDir()
	m, v, a := filepath.Join(dir, "m.jsonl"), filepath.Join(dir, "v.jsonl"), filepath.Join(dir, "a.jsonl")
	writeRejudgeLabels(t, a, labels[:2])
	if code, out := callRejudge(t, "-plan", "-ids", a, "-out", m); code != 0 {
		t.Fatalf("plan exit=%d: %s", code, out)
	}
	// Keep the frozen requests, but remove the document's original id so
	// the verdict cache query matches no document and writes no verdict.
	migrationQuery(t, `MATCH (d:Document {id:$doc}) REMOVE d.id`, map[string]any{"doc": doc})
	defer migrationQuery(t, `MATCH (d:Document {source:$source}) SET d.id=$doc`, map[string]any{"doc": doc, "source": "rejudge-" + t.Name()})
	before := graphtest.Snapshot(t)
	args := []string{"-judge", "-manifest", m, "-budget-attempts", "2", "-out", v}
	if code, out := callRejudge(t, args...); code != 1 || !strings.Contains(out, "write verdict cache") || paid.Load() != 1 {
		t.Fatalf("no document exit=%d paid=%d: %s", code, paid.Load(), out)
	}
	first := readRejudgeFile(t, v)
	if bytes.Count(first, []byte("\n")) != 1 || !bytes.Contains(first, []byte(`"confidence":0.85`)) {
		t.Fatalf("paid verdict missing from ledger: %s", first)
	}
	if code, out := callRejudge(t, args...); code != 1 || !strings.Contains(out, "resumed=1") || paid.Load() != 2 {
		t.Fatalf("resume exit=%d paid=%d: %s", code, paid.Load(), out)
	}
	if got := readRejudgeFile(t, v); !bytes.HasPrefix(got, first) || bytes.Count(got, []byte("\n")) != 2 {
		t.Fatalf("resume replaced or repeated prior verdict: %s", got)
	}
	before.AssertUnchanged(t, "strict write with missing document")
}

func TestRejudgeCLILeaseBlocksAnotherRun(t *testing.T) {
	_, labels := rejudgeCLIFixture(t, func(http.ResponseWriter, *http.Request) {
		t.Error("called provider despite live lease")
	})
	dir := t.TempDir()
	m, v, a := filepath.Join(dir, "m.jsonl"), filepath.Join(dir, "v.jsonl"), filepath.Join(dir, "a.jsonl")
	writeRejudgeLabels(t, a, labels[:1])
	if code, out := callRejudge(t, "-plan", "-ids", a, "-out", m); code != 0 {
		t.Fatalf("plan exit=%d: %s", code, out)
	}
	st := graphtest.Store(t)
	if ok, err := st.AcquireLease(t.Context(), "judge-rejudge", "test-owner", time.Minute, false); err != nil || !ok {
		t.Fatalf("acquire fixture lease: %v %v", ok, err)
	}
	t.Cleanup(func() {
		if err := st.ReleaseLease(context.Background(), "judge-rejudge", "test-owner"); err != nil {
			t.Error(err)
		}
	})
	before := graphtest.Snapshot(t)
	if code, out := callRejudge(t, "-judge", "-manifest", m, "-budget-attempts", "1", "-out", v); code != 1 || !strings.Contains(out, "lease is held") {
		t.Fatalf("live lease exit=%d: %s", code, out)
	}
	if _, err := os.Stat(filepath.Join(dir, "attempts.json")); !os.IsNotExist(err) {
		t.Fatalf("blocked caller created attempt ledger: %v", err)
	}
	before.AssertUnchanged(t, "blocked judge")
}

func TestRejudgeCLIGateFailsAt84PercentPrecision(t *testing.T) {
	st := graphtest.Store(t)
	var paid atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paid.Add(1)
		var request struct {
			Messages []struct{ Content string }
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Error(err)
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		verdict, reason := "confirmed", "endorsed"
		if strings.Contains(request.Messages[1].Content, "Lost") {
			verdict, reason = "unclear", "ambiguous"
		}
		content := fmt.Sprintf(`{"verdict":%q,"confidence":0.8,"reason":%q}`, verdict, reason)
		fmt.Fprintf(w, `{"choices":[{"message":{"content":%q}}]}`, content)
	}))
	t.Cleanup(server.Close)
	configureRejudgeCLI(t, server.URL)
	const domain = "rejudge-gate"
	doc, err := st.UpsertDocument(t.Context(), graph.DocumentInput{Kind: "text", Source: domain, Hash: domain, Domain: domain})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := st.DeleteDocument(context.Background(), doc.ID); err != nil {
			t.Error(err)
		}
	})
	h := "rejudge-gate-acme"
	var fixtures []map[string]any
	var labels []judge.AuditRow
	for i := 0; i < 119; i++ {
		name, label := "Kept", "ok"
		if i >= 100 {
			name, label = "Bad", "wrong"
		} else if i >= 98 {
			name = "Lost"
		}
		tail, chunk := "rejudge-gate-"+name, fmt.Sprintf("%s-c%d", doc.ID, i)
		text := fmt.Sprintf("Acme uses %s.", name)
		fixtures = append(fixtures, map[string]any{"cid": chunk, "i": i, "tail": tail, "name": name, "text": text, "end": 10 + len(name)})
		labels = append(labels, judge.AuditRow{
			ID: graph.FactAuditID(h, "uses", tail, chunk), Head: "Acme", Tail: name,
			Domain: domain, Relation: "uses", Evidence: text, Label: label,
		})
	}
	migrationQuery(t, `MATCH (d:Document {id:$doc})
CREATE (h:Entity {key:$head, domain:$domain, name:'Acme'})
WITH d, h UNWIND $rows AS f
MERGE (t:Entity {key:f.tail}) SET t.name=f.name, t.domain=$domain
CREATE (d)-[:HAS_CHUNK]->(c:Chunk {id:f.cid, doc_id:d.id, seq:f.i, text:f.text}),
 (h)-[:uses {judge:'confirmed', doc_id:d.id, chunk_id:f.cid, head_start:0, head_end:4,
             tail_start:10, tail_end:f.end, confidence:0.8}]->(t),
 (c)-[:MENTIONS]->(h), (c)-[:MENTIONS]->(t)`, map[string]any{"doc": doc.ID, "head": h, "domain": domain, "rows": fixtures})
	dir := t.TempDir()
	m, v, a := filepath.Join(dir, "m.jsonl"), filepath.Join(dir, "v.jsonl"), filepath.Join(dir, "a.jsonl")
	writeRejudgeLabels(t, a, labels)
	if code, out := callRejudge(t, "-plan", "-domain", domain, "-ids", a, "-out", m); code != 0 || !strings.Contains(out, "calls_needed=3") {
		t.Fatalf("plan exit=%d: %s", code, out)
	}
	code, out := callRejudge(t, "-judge", "-manifest", m, "-budget-attempts", "3", "-out", v)
	if code != 3 || paid.Load() != 3 {
		t.Fatalf("gate exit=%d paid=%d: %s", code, paid.Load(), out)
	}
	for _, want := range []string{"GATE FAIL", "119/119", "98/2/0/0", "19/0/0/0", "0.980", "0.838",
		"[0.760, 0.894] n=117", "distinct facts", "3/3", "0.500", "domain \"rejudge-gate\""} {
		if !strings.Contains(out, want) {
			t.Errorf("gate missing %q: %s", want, out)
		}
	}
	for _, row := range labels[98:100] {
		if !strings.Contains(out, "lost-ok "+row.ID+` reason="unclear: ambiguous"`) {
			t.Errorf("gate did not list lost ok %s", row.ID)
		}
	}
	for _, row := range labels[100:] {
		if !strings.Contains(out, "kept-bad "+row.ID+` reason="endorsed"`) {
			t.Errorf("gate did not list kept bad %s", row.ID)
		}
	}
}

func configureRejudgeCLI(t *testing.T, url string) {
	t.Helper()
	cfg := filepath.Join(t.TempDir(), "gkg.yaml")
	if err := os.WriteFile(cfg, []byte("{}\n"), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GKG_CONFIG", cfg)
	t.Setenv("GKG_MEMGRAPH_URI", os.Getenv(graphtest.URIEnv))
	t.Setenv("GKG_JUDGE_BASE_URL", url)
	t.Setenv("GKG_JUDGE_API_KEY", "fake-key")
	t.Setenv("GKG_JUDGE_MODEL", "fake-rejudge")
}

func TestRejudgeCLIPlanUsesStoredPresetAndAdjacentChunk(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		t.Error("plan contacted provider")
		http.Error(w, "unexpected", http.StatusBadRequest)
	}))
	t.Cleanup(server.Close)
	configureRejudgeCLI(t, server.URL)
	presets, err := filepath.Abs("../../labels")
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("GKG_LABELS_PRESETS_DIR", presets)
	const domain = "rejudge-plan-source"
	head, label := seedRejudgeDocumentationFact(t, domain)
	// Fill one head-key page with irrelevant nodes to exercise the next page
	// without relying on database insertion order or global fixture counts.
	migrationQuery(t, `UNWIND range(1, 260) AS i CREATE (:Entity {key:'!rejudge-filler-'+toString(i), domain:$domain, filler:true})`,
		map[string]any{"domain": domain})
	t.Cleanup(func() {
		migrationQuery(t, `MATCH (e:Entity {domain:$domain, filler:true}) DELETE e`, map[string]any{"domain": domain})
	})
	audit := filepath.Join(t.TempDir(), "audit.jsonl")
	writeRejudgeLabels(t, audit, []judge.AuditRow{label})
	path := filepath.Join(t.TempDir(), "m.jsonl")
	if code, out := callRejudge(t, "-plan", "-domain", domain, "-ids", audit, "-out", path); code != 0 ||
		!strings.Contains(out, "reconciled=1") || !strings.Contains(out, "header_present=1 header_null=0") {
		t.Fatalf("plan exit=%d: %s", code, out)
	}
	lines := bytes.Split(bytes.TrimSpace(readRejudgeFile(t, path)), []byte("\n"))
	if len(lines) != 2 {
		t.Fatalf("manifest line count = %d", len(lines))
	}
	var row struct {
		graph.RejudgeIdentity
		Request judge.Request
		Before  map[string]any
	}
	if err := json.Unmarshal(lines[1], &row); err != nil {
		t.Fatal(err)
	}
	if row.ID != label.ID || row.HeadKey != head || !row.LowConfidence || row.ExtractorVersion != "fake:1" ||
		row.Before["extra"] != "keep this property" {
		t.Fatalf("incorrect identity/before-image: %+v", row)
	}
	var payload struct {
		Fact struct {
			Head, Tail string
		}
		Definition   string `json:"relation_definition"`
		HeadType     string `json:"head_mention_type"`
		TailType     string `json:"tail_mention_type"`
		HeadFallback bool   `json:"head_type_fallback"`
		TailFallback bool   `json:"tail_type_fallback"`
		Header       string `json:"table_header"`
		Title        string `json:"doc_title"`
	}
	if err := json.Unmarshal(row.Request.RequestBytes, &payload); err != nil {
		t.Fatal(err)
	}
	if payload.Fact.Head != "Acme" || payload.Fact.Tail != "/ready" ||
		payload.Definition != rejudgeExposesDefinition ||
		payload.HeadType != "service" || payload.HeadFallback || payload.TailType != "endpoint" || !payload.TailFallback ||
		payload.Header != "| Service | Endpoint |\n| --- | --- |" || payload.Title != "API reference" {
		t.Fatalf("unexpected source payload: %+v", payload)
	}
}

const rejudgeExposesDefinition = "a service, API, component, endpoint or function (head) offers the tail (an endpoint, field, parameter, action or interface)"

func TestRejudgeCLIJudgeRejectsEditedPreset(t *testing.T) {
	var calls atomic.Int32
	wirePayload := make(chan string, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		call := calls.Add(1)
		var body struct{ Messages []struct{ Content string } }
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil || len(body.Messages) != 2 {
			t.Errorf("decode judge request: %v, messages=%d", err, len(body.Messages))
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		if call == 1 {
			wirePayload <- body.Messages[1].Content
		}
		writeConfirmedRejudgeResponse(w)
	}))
	t.Cleanup(server.Close)
	configureRejudgeCLI(t, server.URL)
	presets := t.TempDir()
	preset := filepath.Join(presets, "documentation.yaml")
	original := readRejudgeFile(t, "../../labels/documentation.yaml")
	if err := os.WriteFile(preset, original, 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GKG_LABELS_PRESETS_DIR", presets)
	_, label := seedRejudgeDocumentationFact(t, "rejudge-edited-preset")
	dir := t.TempDir()
	manifest, verdicts, audit := filepath.Join(dir, "m.jsonl"), filepath.Join(dir, "v.jsonl"), filepath.Join(dir, "a.jsonl")
	writeRejudgeLabels(t, audit, []judge.AuditRow{label})
	if code, out := callRejudge(t, "-plan", "-ids", audit, "-out", manifest); code != 0 ||
		!strings.Contains(out, "planned=1") || calls.Load() != 0 {
		t.Fatalf("plan exit=%d calls=%d: %s", code, calls.Load(), out)
	}
	args := []string{"-judge", "-manifest", manifest, "-out", verdicts, "-budget-attempts", "1"}
	if code, out := callRejudge(t, args...); code != 0 || !strings.Contains(out, "GATE PASS") || calls.Load() != 1 {
		t.Fatalf("judge exit=%d calls=%d: %s", code, calls.Load(), out)
	}
	req := firstRejudgeRequest(t, readRejudgeFile(t, manifest))
	sent := <-wirePayload
	if sent != string(req.RequestBytes) || !strings.Contains(sent, rejudgeExposesDefinition) {
		t.Fatalf("provider did not receive the frozen preset payload: %s", sent)
	}
	saved := readRejudgeFile(t, verdicts)
	definitionEnd := []byte("parameter, action or interface)\n")
	if bytes.Count(original, definitionEnd) != 1 {
		t.Fatal("could not uniquely locate the exposes definition")
	}
	edited := bytes.Replace(original, definitionEnd, []byte("parameter, action or interface) (edited)\n"), 1)
	if err := os.WriteFile(preset, edited, 0600); err != nil {
		t.Fatal(err)
	}
	if code, _, stderr := callRejudgeStreams(args...); code != 1 ||
		!strings.Contains(stderr, "definitions digest") || calls.Load() != 1 {
		t.Fatalf("edited preset exit=%d calls=%d: %s", code, calls.Load(), stderr)
	}
	if !bytes.Equal(readRejudgeFile(t, verdicts), saved) {
		t.Fatal("edited preset changed the verdict file")
	}
}

func seedRejudgeDocumentationFact(t *testing.T, domain string) (string, judge.AuditRow) {
	t.Helper()
	st := graphtest.Store(t)
	doc, err := st.UpsertDocument(t.Context(), graph.DocumentInput{
		Kind: "text", Source: domain, Hash: domain, Title: "API reference", Labels: "documentation", Domain: domain,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := st.DeleteDocument(context.Background(), doc.ID); err != nil {
			t.Error(err)
		}
	})
	chunk := doc.ID + "-c1"
	head, tail := "#"+doc.ID+":acme", "#"+doc.ID+":ready"
	migrationQuery(t, `MATCH (d:Document {id:$doc})
CREATE (d)-[:HAS_CHUNK]->(prev:Chunk {id:$previous, seq:0, doc_id:$doc, text:'| Service | Endpoint |\n| --- | --- |'}),
 (d)-[:HAS_CHUNK]->(c:Chunk {id:$chunk, seq:1, doc_id:$doc, text:'| Acme | /ready |'}),
 (h:Entity {key:$head, name:'Stored canonical name', type:'organization', domain:$domain}),
 (t:Entity {key:$tail, name:'Stored route name', type:'endpoint', domain:$domain}),
 (c)-[:MENTIONS {start:2, end:6, type:'service'}]->(h),
 (c)-[:MENTIONS {start:1, end:6, type:'ignored'}]->(h),
 (c)-[:MENTIONS {start:9, end:14, type:'ignored'}]->(t),
 (h)-[:exposes {doc_id:$doc, chunk_id:$chunk, judge:'', head_start:2, head_end:6, tail_start:9, tail_end:15,
     confidence:0.6, low_confidence:true, extractor_version:'fake:1', extra:'keep this property'}]->(t)`,
		map[string]any{"doc": doc.ID, "previous": doc.ID + "-c0", "chunk": chunk, "head": head, "tail": tail, "domain": domain})
	return head, judge.AuditRow{ID: graph.FactAuditID(head, "exposes", tail, chunk), Head: "**Acme**",
		Relation: "exposes", Tail: "`/ready`", Evidence: "| Acme | /ready |", Label: "ok"}
}
