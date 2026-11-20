package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/neo4j/neo4j-go-driver/v6/neo4j"

	"github.com/alessiosavi/go-knowledge-graph/internal/graph"
	"github.com/alessiosavi/go-knowledge-graph/internal/graph/graphtest"
	"github.com/alessiosavi/go-knowledge-graph/internal/judge"
	"github.com/alessiosavi/go-knowledge-graph/internal/validity"
)

const validityFixtureMatch = `MATCH (h:Entity {key:$head})-[r:uses]->(t:Entity {key:$tail}) WHERE r.chunk_id = $chunk`

func validityKeyParams(f graph.RejudgeIdentity) map[string]any {
	return map[string]any{"head": f.HeadKey, "tail": f.TailKey, "chunk": f.ChunkID}
}

func storedValidityEdges(t *testing.T, f graph.RejudgeIdentity) []map[string]any {
	t.Helper()
	var edges []map[string]any
	for _, edge := range storedValidityRelationships(t, f) {
		edges = append(edges, edge.Props)
	}
	return edges
}

func storedValidityRelationships(t *testing.T, f graph.RejudgeIdentity) []neo4j.Relationship {
	t.Helper()
	var edges []neo4j.Relationship
	for _, row := range migrationQuery(t, validityFixtureMatch+` RETURN r ORDER BY id(r)`, validityKeyParams(f)) {
		edges = append(edges, row.Values[0].(neo4j.Relationship))
	}
	return edges
}

func storedValidityAudit(t *testing.T, id string) map[string]any {
	t.Helper()
	if audit := storedValidityAuditNode(t, id); audit != nil {
		return audit.Props
	}
	return nil
}

func storedValidityAuditNode(t *testing.T, id string) *neo4j.Node {
	t.Helper()
	rows := migrationQuery(t, `MATCH (n:RejectedFact {id:$id}) RETURN n`, map[string]any{"id": id})
	switch len(rows) {
	case 0:
		return nil
	case 1:
		node := rows[0].Values[0].(neo4j.Node)
		return &node
	default:
		t.Fatalf("found %d audits for %s", len(rows), id)
		return nil
	}
}

func validityGraphData(t *testing.T) (map[int64]neo4j.Node, map[int64]neo4j.Relationship) {
	t.Helper()
	nodes := make(map[int64]neo4j.Node)
	for _, row := range migrationQuery(t, `MATCH (n) RETURN n`, nil) {
		n := row.Values[0].(neo4j.Node)
		slices.Sort(n.Labels)
		nodes[n.Id] = n
	}
	edges := make(map[int64]neo4j.Relationship)
	for _, row := range migrationQuery(t, `MATCH ()-[r]->() RETURN r`, nil) {
		r := row.Values[0].(neo4j.Relationship)
		edges[r.Id] = r
	}
	return nodes, edges
}

func assertValidityGraphProjection[T any](t *testing.T, kind string, want, got map[int64]T) {
	t.Helper()
	for id, projected := range want {
		if actual, ok := got[id]; !ok || !reflect.DeepEqual(actual, projected) {
			t.Errorf("%s id %d = %+v (present=%t), want %+v", kind, id, actual, ok, projected)
		}
	}
	for id, actual := range got {
		if _, ok := want[id]; !ok {
			t.Errorf("unexpected %s id %d: %+v", kind, id, actual)
		}
	}
}

type validityLedgerRow struct {
	ID             string            `json:"id"`
	PriorJudge     string            `json:"prior_judge"`
	PlannedOutcome validity.Outcome  `json:"planned_outcome"`
	ActualOutcome  *validity.Outcome `json:"actual_outcome"`
	NewID          string            `json:"new_id"`
	Rule           string            `json:"rule"`
	Reason         string            `json:"reason"`
	SourceVerdict  string            `json:"source_verdict"`
	Evidence       string            `json:"evidence"`
}

type validityLedgerTotals struct {
	TypedEdges graph.ValidityEdgeCounts                       `json:"typed_edges"`
	Counts     map[validity.Outcome]map[string]map[string]int `json:"counts"`
}

type validityLedgerSummary struct {
	Status       string                   `json:"status"`
	ValidityRun  string                   `json:"validity_run"`
	FinalDigest  string                   `json:"final_digest"`
	StopRow      string                   `json:"stop_row"`
	StopReason   string                   `json:"stop_reason"`
	Before       graph.ValidityEdgeCounts `json:"before"`
	Actual       validityLedgerTotals     `json:"actual"`
	Projected    validityLedgerTotals     `json:"projected"`
	BucketCounts map[string]int           `json:"bucket_counts"`
}

func readValidityLedger(t *testing.T, path string) ([]validityLedgerRow, validityLedgerSummary) {
	t.Helper()
	raw := validityRows[json.RawMessage](t, path)
	if len(raw) == 0 {
		t.Fatal("empty validity ledger")
	}
	var summary validityLedgerSummary
	if err := json.Unmarshal(raw[len(raw)-1], &summary); err != nil {
		t.Fatal(err)
	}
	var rows []validityLedgerRow
	for _, line := range raw[:len(raw)-1] {
		var row validityLedgerRow
		if err := json.Unmarshal(line, &row); err != nil {
			t.Fatal(err)
		}
		rows = append(rows, row)
	}
	info, err := os.Stat(path)
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatalf("report mode must be 0600, got %v (%v)", info, err)
	}
	return rows, summary
}

func assertMixedValidityApply(t *testing.T, st *graph.Store, planDir, report string, plan validity.PlanSummary, reject, confirmedRewrite string,
	baseline graph.ValidityEdgeCounts, inputs map[string]validity.InputsRow, sources map[string]validityManifestRow) {
	t.Helper()
	rejected := sources[reject]
	migrationQuery(t, `CREATE (:RejectedFact {id:$id, doc_id:$doc, reason:'old invented reason', prior_score:1.0, prior_count:1})`,
		map[string]any{"id": reject, "doc": rejected.Request.DocID})
	priorAudit := storedValidityAuditNode(t, reject)
	if priorAudit == nil {
		t.Fatal("missing prior audit fixture")
	}
	priorEdges := make(map[string]neo4j.Relationship)
	for id, row := range sources {
		edges := storedValidityRelationships(t, row.RejudgeIdentity)
		if len(edges) != 1 {
			t.Fatalf("fixture source %s has %d edges", id, len(edges))
		}
		priorEdges[id] = edges[0]
	}
	final := validityRows[validity.FinalRow](t, filepath.Join(planDir, "final.jsonl"))
	wantNodes, wantEdges := validityGraphData(t)

	args := []string{"-apply", "-plan", planDir, "-approved-digest", plan.FinalDigest, "-quiescent", "-report", report}
	validityCommandOK(t, args...)
	assertValidityLeaseReleased(t)
	gotNodes, gotEdges := validityGraphData(t)
	rows, summary := readValidityLedger(t, report)
	if len(rows) != len(final) {
		t.Fatalf("ledger has %d source rows, want %d", len(rows), len(final))
	}
	// The cached rejudge that built the plan already created the fence.
	// Apply commits one increment on acquisition and one per approved row.
	for _, node := range wantNodes {
		if slices.Equal(node.Labels, []string{"GkgMeta"}) && node.Props["id"] == "judge_lease_fence" {
			node.Props["seq"] = node.Props["seq"].(int64) + 1 + int64(len(final))
			break
		}
	}
	wantBefore := graph.ValidityEdgeCounts{Default: baseline.Default + 22, ConfirmedOnly: baseline.ConfirmedOnly + 20}
	wantAfter := graph.ValidityEdgeCounts{Default: baseline.Default + 8, ConfirmedOnly: baseline.ConfirmedOnly + 6}
	wantActual := validityLedgerTotals{TypedEdges: wantAfter, Counts: plan.Counts}
	if summary.Status != "applied" || summary.ValidityRun != plan.FinalDigest[:16] || summary.FinalDigest != plan.FinalDigest ||
		summary.StopRow != "" || summary.StopReason != "" || summary.Before != wantBefore ||
		!reflect.DeepEqual(summary.Projected, summary.Actual) || !reflect.DeepEqual(summary.Actual, wantActual) ||
		!maps.Equal(summary.BucketCounts, plan.BucketCounts) || summary.BucketCounts["ambiguous"] != 1 ||
		summary.BucketCounts["unverifiable:missing-chunk"] != 1 {
		t.Errorf("apply summary = %+v, want before %+v and actual %+v", summary, wantBefore, wantActual)
	}
	rewriteRun := fmt.Sprintf("%x", sha256.Sum256(readRejudgeFile(t, filepath.Join(planDir, "rewrites.jsonl"))))[:16]
	marks := func(row validity.FinalRow, source validityManifestRow) map[string]any {
		run := plan.ManifestDigest[:16]
		if row.RewriteVerdict != "" {
			run = rewriteRun
		}
		return map[string]any{
			"judge_run": run, "judge_prompt": source.Request.Profile.Fingerprint, "validity_run": plan.FinalDigest[:16],
		}
	}
	for i, approved := range final {
		line := rows[i]
		source, input := sources[approved.ID], inputs[approved.ID]
		if line.ID != approved.ID || line.PriorJudge != input.PriorJudge || line.PlannedOutcome != approved.FinalOutcome ||
			line.ActualOutcome == nil || *line.ActualOutcome != approved.FinalOutcome || line.NewID != approved.NewID ||
			line.Rule != approved.Rule || line.Reason != approved.Reason || line.SourceVerdict != approved.SourceVerdict || line.Evidence != input.Evidence {
			t.Errorf("lineage row %d = %+v, approved %+v", i, line, approved)
		}
		before := priorEdges[approved.ID]
		props := before.Props
		switch approved.FinalOutcome {
		case validity.OutcomeDemote, validity.OutcomeStamp:
			want := maps.Clone(props)
			maps.Copy(want, marks(approved, source))
			want["judge"], want["judge_prior"] = approved.SourceVerdict, input.PriorJudge
			before.Props = want
			wantEdges[before.Id] = before
		case validity.OutcomeReject, validity.OutcomeValidityDrop:
			want := marks(approved, source)
			want["id"], want["validity_rule"] = approved.ID, approved.Rule
			for _, key := range []string{"head_key", "tail_key", "doc_id", "chunk_id", "extractor_version"} {
				want[key] = props[key]
			}
			want["rel"], want["score"] = source.Relation, props["confidence"]
			want["judge_verdict"] = "validity"
			rule := approved.Reason
			if rule == "" {
				rule = approved.Rule
			}
			want["reason"] = "validity: " + rule
			if approved.FinalOutcome == validity.OutcomeReject {
				want["judge_verdict"], want["reason"] = "refuted", "v3: invented fixture"
			}
			delete(wantEdges, before.Id)
			audit := storedValidityAuditNode(t, approved.ID)
			if audit == nil {
				t.Fatalf("%s lacks an audit", approved.ID)
			}
			checkValidityAuditJSON(t, audit.Props["edge_before"], props)
			want["edge_before"], want["at"] = audit.Props["edge_before"], audit.Props["at"]
			id, elementID := audit.Id, audit.ElementId
			if approved.ID == reject {
				checkValidityAuditJSON(t, audit.Props["prior_audit"], priorAudit.Props)
				want["prior_audit"] = audit.Props["prior_audit"]
				id, elementID = priorAudit.Id, priorAudit.ElementId
				if !strings.Contains(fmt.Sprint(audit.Props["prior_audit"]), `"prior_score":1.0`) ||
					!strings.Contains(fmt.Sprint(audit.Props["prior_audit"]), `"prior_count":1,`) ||
					!strings.Contains(fmt.Sprint(audit.Props["edge_before"]), `"invented_float":1.0`) {
					t.Errorf("audit lost numeric types: edge %v, prior %v", audit.Props["edge_before"], audit.Props["prior_audit"])
				}
			}
			if _, err := time.Parse(time.RFC3339Nano, fmt.Sprint(audit.Props["at"])); err != nil {
				t.Error(err)
			}
			wantNodes[id] = neo4j.Node{Id: id, ElementId: elementID, Labels: []string{"RejectedFact"}, Props: want}
		case validity.OutcomeRewriteConfirmed, validity.OutcomeRewriteUnclear:
			want := marks(approved, source)
			for _, key := range []string{"tail_key", "chunk_id", "doc_id", "head_start", "head_end", "tail_start",
				"tail_end", "confidence", "low_confidence", "extractor_version"} {
				want[key] = props[key]
			}
			want["head_key"], want["head_surface"] = input.Target.Key, strings.TrimSpace(input.Target.Name)
			want["reattached_from"], want["validity_rule"], want["judge"] = approved.ID, approved.Rule, approved.RewriteVerdict
			target := source.RejudgeIdentity
			target.HeadKey = input.Target.Key
			rewritten := storedValidityRelationships(t, target)
			if len(rewritten) != 1 {
				t.Fatalf("%s has %d rewritten edges, want 1", approved.ID, len(rewritten))
			}
			delete(wantEdges, before.Id)
			head := wantNodes[rewritten[0].StartId]
			before.Id, before.ElementId = rewritten[0].Id, rewritten[0].ElementId
			before.StartId, before.StartElementId, before.Props = head.Id, head.ElementId, want
			wantEdges[before.Id] = before
		default:
			t.Fatalf("unexpected final outcome %s", approved.FinalOutcome)
		}
	}
	assertValidityGraphProjection(t, "node", wantNodes, gotNodes)
	assertValidityGraphProjection(t, "relationship", wantEdges, gotEdges)
	target := inputs[confirmedRewrite].Target.Key
	view, err := st.EntityViewOpts(t.Context(), target, graph.ViewOptions{ConfirmedOnly: true})
	if err != nil || len(view.Facts) != 1 || !view.Facts[0].Outgoing || view.Facts[0].Other.Key != sources[confirmedRewrite].TailKey {
		t.Errorf("target EntityView = %+v, %v", view, err)
	}
	replanned := filepath.Join(t.TempDir(), "rewritten.jsonl")
	if code, out := callRejudge(t, "-plan", "-domain", sources[confirmedRewrite].Domain, "-out", replanned); code != 0 {
		t.Fatalf("replan rewritten fact exit=%d: %s", code, out)
	}
	frozen := validityRows[validityManifestRow](t, filepath.Join(planDir, "rewrites.jsonl"))
	for _, candidate := range frozen[1:] {
		if candidate.SourceID == confirmedRewrite {
			live := validityRows[validityManifestRow](t, replanned)[1:]
			if len(live) != 1 || live[0].ID != candidate.ID || !bytes.Equal(live[0].Request.RequestBytes, candidate.Request.RequestBytes) {
				t.Errorf("rewritten request differs from approved request: %+v", live)
			}
		}
	}
}

func checkValidityAuditJSON(t *testing.T, got any, want map[string]any) {
	t.Helper()
	wantRaw, err := json.Marshal(want)
	if err != nil {
		t.Fatal(err)
	}
	var actual, expected map[string]any
	raw, ok := got.(string)
	if !ok || json.Unmarshal([]byte(raw), &actual) != nil || json.Unmarshal(wantRaw, &expected) != nil || !reflect.DeepEqual(actual, expected) {
		t.Errorf("audit JSON = %v, want %s", got, wantRaw)
	}
}

func assertValidityLeaseReleased(t *testing.T) {
	t.Helper()
	if rows := migrationQuery(t, `MATCH (l:JudgeLease {id:'repair-validity'}) RETURN l`, nil); len(rows) != 0 {
		t.Errorf("repair-validity lease remains")
	}
}

func applyFixturePlan(t *testing.T, st *graph.Store, statuses map[string]string) string {
	t.Helper()
	m, v := validityBulkRun(t, st, statuses)
	dir := t.TempDir()
	validityCommandOK(t, "-plan", "-manifest", m, "-verdicts", v, "-out", dir)
	return dir
}

func finalizeApplyFixture(t *testing.T, dir, rewriteVerdicts string) string {
	t.Helper()
	args := []string{"-finalize", "-plan", dir}
	if rewriteVerdicts == "" {
		args = append(args, "-no-rewrites")
	} else {
		args = append(args, "-rewrite-verdicts", rewriteVerdicts)
	}
	validityCommandOK(t, args...)
	summary, err := validity.DecodePlanSummary(readRejudgeFile(t, filepath.Join(dir, "final-plan.json")))
	if err != nil {
		t.Fatal(err)
	}
	return summary.FinalDigest
}

func applyFixtureSource(t *testing.T, suffix, name string, target bool) (validityDoc, graph.RejudgeIdentity) {
	t.Helper()
	d := seedValidityDoc(t, suffix, "catalog", "Zorbs in wib "+suffix, "zorbs use Quarin "+suffix+".")
	head, tail := d.entity(t, "source", name, false), d.entity(t, "tail", "Quarin", false)
	if target {
		d.entity(t, "target", d.title, true)
	}
	id := d.edge(t, head, "uses", tail, d.id, d.chunk, "confirmed", 0.8, 0, 5, 10, 16)
	return d, graph.RejudgeIdentity{ID: id, HeadKey: head, TailKey: tail, Relation: "uses", ChunkID: d.chunk}
}

func rewriteFixtureDraft(t *testing.T, dir string, updateInputs func([]byte) []byte, updateRewrites func([]byte) []byte) {
	t.Helper()
	files := validityFileSnapshot(t, dir)
	summary, err := validity.DecodePlanSummary(files["plan.json"])
	if err != nil {
		t.Fatal(err)
	}
	inputs, rewrites := files["inputs.jsonl"], files["rewrites.jsonl"]
	if updateInputs != nil {
		inputs = updateInputs(inputs)
	}
	if updateRewrites != nil {
		rewrites = updateRewrites(rewrites)
	}
	summary.DraftDigest = validityTestDigest(t, []byte(summary.ManifestDigest), []byte(summary.VerdictSeal),
		inputs, rewrites, []byte(summary.RulesID), []byte(summary.LexiconDigest))
	encoded, err := json.Marshal(summary)
	if err != nil {
		t.Fatal(err)
	}
	for name, raw := range map[string][]byte{"inputs.jsonl": inputs, "rewrites.jsonl": rewrites, "plan.json": encoded} {
		if err := os.WriteFile(filepath.Join(dir, name), raw, 0600); err != nil {
			t.Fatal(err)
		}
	}
}

func assertValidityPrecondition(t *testing.T, dir, digest, report, want string, quiescent bool, details ...string) {
	t.Helper()
	assertValidityApplyRejected(t, dir, digest, report, want, quiescent, 0, details...)
}

func assertValidityApplyRejected(t *testing.T, dir, digest, report, want string, quiescent bool, fenceAdvances int64, details ...string) {
	t.Helper()
	before := graphtest.Snapshot(t)
	content, beforeErr := os.ReadFile(report)
	args := []string{"-apply", "-plan", dir, "-approved-digest", digest, "-report", report}
	if quiescent {
		args = append(args, "-quiescent")
	}
	var stdout, stderr bytes.Buffer
	if code := run(append([]string{"repair-validity"}, args...), &stdout, &stderr); code != 2 {
		t.Errorf("apply exit=%d, want 2: %s", code, &stderr)
	}
	for _, detail := range append([]string{want}, details...) {
		if !strings.Contains(stderr.String(), detail) {
			t.Errorf("stderr missing %q: %s", detail, &stderr)
		}
	}
	if fenceAdvances == 0 {
		before.AssertUnchanged(t, "failed apply precondition")
	} else {
		before.AssertJudgeLeaseFenceAdvanced(t, "failed apply precondition", fenceAdvances)
	}
	after, afterErr := os.ReadFile(report)
	if (beforeErr == nil && (afterErr != nil || !bytes.Equal(content, after))) ||
		(errors.Is(beforeErr, os.ErrNotExist) && !errors.Is(afterErr, os.ErrNotExist)) {
		t.Errorf("report changed on exit 2: before %q (%v), after %q (%v)", content, beforeErr, after, afterErr)
	}
}

func TestRepairValidityApplyDeferred(t *testing.T) {
	st := validityFixture(t)
	_, source := applyFixtureSource(t, "no-target", "zorbs", false)
	migrationQuery(t, validityFixtureMatch+` SET r.fixture_values = [1, 2, 3], r.judge_run = 'previous-run'`, validityKeyParams(source))
	dir := applyFixturePlan(t, st, nil)
	validityCommandOK(t, "-finalize", "-plan", dir, "-no-rewrites", "-defer-unconfirmed")
	planPath := filepath.Join(dir, "final-plan.json")
	planBytes := readRejudgeFile(t, planPath)
	plan, err := validity.DecodePlanSummary(planBytes)
	if err != nil {
		t.Fatal(err)
	}
	report := filepath.Join(t.TempDir(), "apply.jsonl")

	t.Run("changed deferral mode", func(t *testing.T) {
		changed := bytes.Replace(planBytes, []byte(`"deferral":"unconfirmed-destination"`), []byte(`"deferral":""`), 1)
		if bytes.Equal(changed, planBytes) {
			t.Fatal("final plan did not contain the deferral mode")
		}
		if err := os.WriteFile(planPath, changed, 0600); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			if err := os.WriteFile(planPath, planBytes, 0600); err != nil {
				t.Error(err)
			}
		})
		assertValidityPrecondition(t, dir, plan.FinalDigest, report, "final.jsonl differs", true)
	})

	// A deferred source still has to match its frozen request at preflight.
	presetPath := filepath.Join(os.Getenv("GKG_LABELS_PRESETS_DIR"), "catalog.yaml")
	preset := readRejudgeFile(t, presetPath)
	changed := bytes.Replace(preset, []byte("employs the tail"), []byte("links the head to the tail"), 1)
	if err := os.WriteFile(presetPath, changed, 0600); err != nil {
		t.Fatal(err)
	}
	assertValidityPrecondition(t, dir, plan.FinalDigest, report, "bulk manifest request bytes differ", true, source.ID)
	if err := os.WriteFile(presetPath, preset, 0600); err != nil {
		t.Fatal(err)
	}

	before := graphtest.Snapshot(t)
	edges := storedValidityRelationships(t, source)
	if len(edges) != 1 {
		t.Fatalf("deferred fixture edges = %d, want 1", len(edges))
	}
	validityCommandOK(t, "-apply", "-plan", dir, "-approved-digest", plan.FinalDigest, "-quiescent", "-report", report)
	before.AssertJudgeLeaseFenceAdvanced(t, "apply with only deferred rows", 2)
	if after := storedValidityRelationships(t, source); !reflect.DeepEqual(after, edges) {
		t.Errorf("deferred edge = %+v, want %+v", after, edges)
	}
	rows, summary := readValidityLedger(t, report)
	if len(rows) != 1 || rows[0].ID != source.ID || rows[0].PlannedOutcome != validity.OutcomeValidityDrop ||
		rows[0].ActualOutcome == nil || *rows[0].ActualOutcome != validity.OutcomeDeferred || rows[0].NewID != "" || rows[0].Reason != "no-target" {
		t.Fatalf("deferred lineage rows = %+v", rows)
	}
	if summary.Status != "applied" || !reflect.DeepEqual(summary.Projected, summary.Actual) || summary.Before != summary.Actual.TypedEdges ||
		summary.Actual.Counts[validity.OutcomeDeferred][source.Relation][rows[0].Rule] != 1 {
		t.Errorf("deferred lineage summary = %+v", summary)
	}
}

func TestRepairValidityApplyPreconditions(t *testing.T) {
	st := validityFixture(t)
	d, source := applyFixtureSource(t, "preconditions", "zorbs", true)
	dir := applyFixturePlan(t, st, nil)
	verdicts := runCachedValidityJudge(t, st, filepath.Join(dir, "rewrites.jsonl"), nil)
	digest := finalizeApplyFixture(t, dir, verdicts)
	report := filepath.Join(t.TempDir(), "apply.jsonl")

	testCases := []struct {
		name, want string
		edit       func(*testing.T) string
	}{
		{"live lease", "live judge lease", func(t *testing.T) string {
			expires := time.Now().Add(time.Minute).UnixMilli()
			migrationQuery(t, `CREATE (:JudgeLease {id:'invented-other-job', owner:'invented-owner', expires_at:$expires})`, map[string]any{"expires": expires})
			t.Cleanup(func() {
				migrationQuery(t, `MATCH (l:JudgeLease {id:'invented-other-job'}) DELETE l`, nil)
			})
			return fmt.Sprintf(`name="invented-other-job" owner="invented-owner" expires_at=%d`, expires)
		}},
		{"edge marker", "already carries validity_run", func(t *testing.T) string {
			rows := migrationQuery(t, validityFixtureMatch+` SET r.validity_run = 'old-run' RETURN id(r)`, validityKeyParams(source))
			t.Cleanup(func() { migrationQuery(t, validityFixtureMatch+` REMOVE r.validity_run`, validityKeyParams(source)) })
			return fmt.Sprintf("edge id=%d", rows[0].Values[0])
		}},
		{"audit marker", "already carries validity_run", func(t *testing.T) string {
			params := map[string]any{"id": source.ID, "doc": d.id}
			migrationQuery(t, `CREATE (:RejectedFact {id:$id, doc_id:$doc, validity_run:'old-run'})`, params)
			t.Cleanup(func() { migrationQuery(t, `MATCH (a:RejectedFact {id:$id}) DELETE a`, params) })
			return fmt.Sprintf("RejectedFact id=%q", source.ID)
		}},
		{"inventory", "classifier inventory differs", func(t *testing.T) string {
			migrationQuery(t, validityFixtureMatch+` SET r.judge = ''`, validityKeyParams(source))
			t.Cleanup(func() {
				migrationQuery(t, validityFixtureMatch+` SET r.judge = 'confirmed'`, validityKeyParams(source))
			})
			return source.ID
		}},
		{"occupant", "live occupants differ", func(t *testing.T) string {
			d.edge(t, d.target, "uses", source.TailKey, d.id, d.chunk, "unclear", 0.7, 0, 5, 10, 16)
			target := source
			target.HeadKey = d.target
			t.Cleanup(func() { migrationQuery(t, validityFixtureMatch+` DELETE r`, validityKeyParams(target)) })
			return source.ID
		}},
		{"ranking", "candidate ranking differs", func(t *testing.T) string {
			migrationQuery(t, validityFixtureMatch+` SET r.confidence = 0.9`, validityKeyParams(source))
			t.Cleanup(func() { migrationQuery(t, validityFixtureMatch+` SET r.confidence = 0.8`, validityKeyParams(source)) })
			return source.ID
		}},
		{"non-terminal", "non-terminal status", func(t *testing.T) string {
			params := map[string]any{"doc": d.id}
			migrationQuery(t, `MATCH (d:Document {id:$doc}) SET d.status = 'verifying'`, params)
			t.Cleanup(func() { migrationQuery(t, `MATCH (d:Document {id:$doc}) SET d.status = 'done'`, params) })
			return fmt.Sprintf(`document id=%q is in a non-terminal status (status="verifying")`, d.id)
		}},
		{"missing status", "non-terminal status", func(t *testing.T) string {
			params := map[string]any{"doc": d.id}
			migrationQuery(t, `MATCH (d:Document {id:$doc}) REMOVE d.status`, params)
			t.Cleanup(func() { migrationQuery(t, `MATCH (d:Document {id:$doc}) SET d.status = 'done'`, params) })
			return fmt.Sprintf(`document id=%q is in a non-terminal status (status="<missing>")`, d.id)
		}},
	}
	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			assertValidityPrecondition(t, dir, digest, report, tc.want, true, tc.edit(t))
		})
	}
	t.Run("digest", func(t *testing.T) {
		assertValidityPrecondition(t, dir, strings.Repeat("0", 64), report, "approved digest", true)
	})
	t.Run("quiescence", func(t *testing.T) {
		assertValidityPrecondition(t, dir, digest, report, "usage:", false)
	})
	t.Run("live lexicon", func(t *testing.T) {
		path := filepath.Join(os.Getenv("GKG_LABELS_PRESETS_DIR"), "catalog.yaml")
		original := readRejudgeFile(t, path)
		changed := bytes.Replace(original, []byte("cutoffon"), []byte("frendel"), 1)
		if err := os.WriteFile(path, changed, 0600); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			if err := os.WriteFile(path, original, 0600); err != nil {
				t.Error(err)
			}
		})
		assertValidityPrecondition(t, dir, digest, report, "lexicon digest differs", true)
	})
	t.Run("changed final file", func(t *testing.T) {
		path := filepath.Join(dir, "final.jsonl")
		original := readRejudgeFile(t, path)
		if err := os.WriteFile(path, append(bytes.Clone(original), '\n'), 0600); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			if err := os.WriteFile(path, original, 0600); err != nil {
				t.Error(err)
			}
		})
		assertValidityPrecondition(t, dir, digest, report, "final.jsonl differs", true)
	})
	t.Run("report exists", func(t *testing.T) {
		if err := os.WriteFile(report, []byte("invented existing report\n"), 0600); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.Remove(report) })
		assertValidityPrecondition(t, dir, digest, report, "already exists", true, report)
	})
	t.Run("report parent missing", func(t *testing.T) {
		missing := filepath.Join(t.TempDir(), "missing", "apply.jsonl")
		assertValidityApplyRejected(t, dir, digest, missing, "open validity apply report", true, 1, missing)
	})
	t.Run("decision drift", func(t *testing.T) {
		drift := t.TempDir()
		for name, raw := range validityFileSnapshot(t, dir) {
			switch name {
			case "final.jsonl", "final-plan.json", "affected.jsonl", "affected-summary.json", "rewrite-verdicts.jsonl", "rewrite-attempts.json":
				continue
			}
			if err := os.WriteFile(filepath.Join(drift, name), raw, 0600); err != nil {
				t.Fatal(err)
			}
		}
		rewriteFixtureDraft(t, drift, func(raw []byte) []byte {
			changed := bytes.Replace(raw, []byte(`"action":"reattach"`), []byte(`"action":"keep"`), 1)
			if bytes.Equal(raw, changed) {
				t.Fatal("fixture did not contain a reattach decision")
			}
			return changed
		}, nil)
		driftDigest := finalizeApplyFixture(t, drift, verdicts)
		assertValidityPrecondition(t, drift, driftDigest, report, "decision differs from Decide", true, source.ID)
	})
}

func TestRepairValidityApplyRequestPreflight(t *testing.T) {
	t.Run("bulk definition", func(t *testing.T) {
		st := validityFixture(t)
		d, uses := applyFixtureSource(t, "bulk", "Zorbs", true)
		madeOf := d.edge(t, uses.HeadKey, "made_of", uses.TailKey, d.id, d.chunk, "confirmed", 0.8, 0, 5, 10, 16)
		plan := applyFixturePlan(t, st, map[string]string{madeOf: "refuted"})
		digest := finalizeApplyFixture(t, plan, "")

		// Change the later row's relation so the old apply commits the first row.
		id, definition := uses.ID, "employs the tail"
		if madeOf > id {
			id, definition = madeOf, "consists of the tail"
		}
		path := filepath.Join(os.Getenv("GKG_LABELS_PRESETS_DIR"), "catalog.yaml")
		original := readRejudgeFile(t, path)
		changed := bytes.Replace(original, []byte(definition), []byte("links the head to the tail"), 1)
		if bytes.Equal(original, changed) {
			t.Fatal("fixture relation definition was missing")
		}
		if err := os.WriteFile(path, changed, 0600); err != nil {
			t.Fatal(err)
		}
		assertValidityPrecondition(t, plan, digest, filepath.Join(t.TempDir(), "apply.jsonl"), "bulk manifest", true, id)
	})
	t.Run("rewrite target type", func(t *testing.T) {
		st := validityFixture(t)
		doc, source := applyFixtureSource(t, "one", "Zorbs", true)
		otherDoc, other := applyFixtureSource(t, "two", "Zorbs", true)
		if source.ID < other.ID {
			doc, source = otherDoc, other
		}
		migrationQuery(t, validityFixtureMatch+` SET h.name = 'zorbs'`, validityKeyParams(source))
		plan := applyFixturePlan(t, st, nil)
		verdicts := runCachedValidityJudge(t, st, filepath.Join(plan, "rewrites.jsonl"), nil)
		digest := finalizeApplyFixture(t, plan, verdicts)

		migrationQuery(t, `MATCH (e:Entity {key:$target}) SET e.type = 'glyph'`, map[string]any{"target": doc.target})
		candidate := graph.FactAuditID(doc.target, source.Relation, source.TailKey, source.ChunkID)
		assertValidityPrecondition(t, plan, digest, filepath.Join(t.TempDir(), "apply.jsonl"), "rewrites manifest", true, source.ID, candidate)
	})
	t.Run("refuted rewrite bytes", func(t *testing.T) {
		st := validityFixture(t)
		doc, source := applyFixtureSource(t, "refuted", "zorbs", true)
		plan := applyFixturePlan(t, st, nil)
		replaceRewriteEvidence(t, plan)
		candidate := graph.FactAuditID(doc.target, source.Relation, source.TailKey, source.ChunkID)
		verdicts := runCachedValidityJudge(t, st, filepath.Join(plan, "rewrites.jsonl"), map[string]string{candidate: "refuted"})
		digest := finalizeApplyFixture(t, plan, verdicts)
		assertValidityPrecondition(t, plan, digest, filepath.Join(t.TempDir(), "apply.jsonl"), "rewrites manifest", true, source.ID, candidate)
	})
}

func TestRepairValidityApplyReportRace(t *testing.T) {
	st := validityFixture(t)
	applyFixtureSource(t, "report-race", "Zorbs", true)
	plan := applyFixturePlan(t, st, nil)
	digest := finalizeApplyFixture(t, plan, "")
	report := filepath.Join(t.TempDir(), "apply.jsonl")
	const competing = "competing report\n"

	// Taking over an expired lease must be reversible if opening the report fails.
	migrationQuery(t, `CREATE (:JudgeLease {id:'repair-validity', owner:'previous-owner', expires_at:0})`, nil)
	t.Cleanup(func() { migrationQuery(t, `MATCH (l:JudgeLease {id:'repair-validity'}) DELETE l`, nil) })
	before := graphtest.Snapshot(t)
	writes := 0
	st.SetWriteLockWaitObserver(func(time.Duration) {
		writes++
		if writes != 1 {
			return
		}
		if _, err := os.Lstat(report); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("report path should be absent when acquisition commits: %v", err)
		}
		if err := os.WriteFile(report, []byte(competing), 0600); err != nil {
			t.Error(err)
		}
	})
	var stderr bytes.Buffer
	code := runValidityApply(t.Context(), st, os.Getenv("GKG_LABELS_PRESETS_DIR"), plan, digest, report, &stderr)
	st.SetWriteLockWaitObserver(nil)
	if code != 2 || !strings.Contains(stderr.String(), "open validity apply report") || writes != 1 {
		t.Errorf("apply exit=%d after %d observed writes: %s", code, writes, &stderr)
	}
	if got := readRejudgeFile(t, report); string(got) != competing {
		t.Errorf("competing report changed: %q", got)
	}
	before.AssertJudgeLeaseFenceAdvanced(t, "report creation race", 1)
}

func TestRepairValidityApplyStops(t *testing.T) {
	for _, tc := range []struct {
		name, query, reason string
		candidate           bool
	}{
		{"zero", validityFixtureMatch + ` DELETE r`, "source logical key matched 0 edges", false},
		{"duplicate", validityFixtureMatch + ` CREATE (h)-[:uses {chunk_id:r.chunk_id, judge:'unclear'}]->(t)`, "source logical key matched 2 edges", false},
		{"identity", validityFixtureMatch + ` SET r.extractor_version = 'invented-v2'`, "sealed edge identity", false},
		{"counted delete rollback", validityFixtureMatch + ` SET r.head_start = 0.5`, "delete source edge count 0", false},
		{"parity", validityFixtureMatch + ` SET h.name = 'Frendel'`, "live parity inputs", false},
		{"request", validityFixtureMatch + ` SET t.type = 'glyph'`, "source request bytes differ", false},
		{"target", `MATCH (e:Entity {key:$target}) SET e.name = 'Frendel'`, "resolved target differ", true},
		{"document", `MATCH (d:Document {id:$doc}) DETACH DELETE d`, "write RejectedFact count 0", false},
		{"destination", `MATCH (h:Entity {key:$target}), (t:Entity {key:$tail}) CREATE (h)-[:uses {chunk_id:$chunk, judge:'unclear'}]->(t)`,
			"rewrite destination matched 1 edges", true},
		{"lease", `MATCH (l:JudgeLease {id:'repair-validity'}) SET l.expires_at = 0`, "lease lost", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st := validityFixture(t)
			type item struct {
				doc validityDoc
				f   graph.RejudgeIdentity
			}
			var items []item
			for i := range 3 {
				d, f := applyFixtureSource(t, fmt.Sprintf("stop%d", i), "Zorbs", true)
				items = append(items, item{d, f})
			}
			slices.SortFunc(items, func(a, b item) int { return strings.Compare(a.f.ID, b.f.ID) })
			middle := items[1]
			params := validityKeyParams(middle.f)
			params["target"], params["doc"] = middle.doc.target, middle.doc.id
			var statuses map[string]string
			if tc.candidate {
				migrationQuery(t, validityFixtureMatch+` SET h.name = 'zorbs'`, params)
			} else {
				statuses = map[string]string{middle.f.ID: "refuted"}
			}
			plan := applyFixturePlan(t, st, statuses)
			var verdicts string
			if tc.candidate {
				verdicts = runCachedValidityJudge(t, st, filepath.Join(plan, "rewrites.jsonl"), nil)
			}
			digest := finalizeApplyFixture(t, plan, verdicts)
			report := filepath.Join(t.TempDir(), "apply.jsonl")
			middleBefore, lastBefore := storedValidityEdges(t, middle.f), storedValidityEdges(t, items[2].f)

			// The lock observer runs after releasing Store.mu. Alter the
			// graph between commits using a second Bolt connection.
			writes := 0
			st.SetWriteLockWaitObserver(func(time.Duration) {
				writes++
				if writes == 2 {
					migrationQuery(t, tc.query, params)
					middleBefore = storedValidityEdges(t, middle.f)
				}
			})
			var stderr bytes.Buffer
			code := runValidityApply(t.Context(), st, os.Getenv("GKG_LABELS_PRESETS_DIR"), plan, digest, report, &stderr)
			st.SetWriteLockWaitObserver(nil)
			if code != 4 || !strings.Contains(stderr.String(), tc.reason) {
				t.Fatalf("apply exit=%d, want 4 and %q: %s", code, tc.reason, &stderr)
			}
			rows, summary := readValidityLedger(t, report)
			if len(rows) != 3 || rows[0].ID != items[0].f.ID || rows[0].ActualOutcome == nil || *rows[0].ActualOutcome != validity.OutcomeStamp ||
				rows[1].ID != middle.f.ID || rows[1].ActualOutcome != nil || rows[2].ID != items[2].f.ID || rows[2].ActualOutcome != nil ||
				summary.Status != "stopped" || summary.StopRow != middle.f.ID || !strings.Contains(summary.StopReason, tc.reason) {
				t.Errorf("stopped ledger: rows %+v, summary %+v", rows, summary)
			}
			if first := storedValidityEdges(t, items[0].f); len(first) != 1 || first[0]["validity_run"] != digest[:16] {
				t.Errorf("first write did not persist: %v", first)
			}
			if current := storedValidityEdges(t, middle.f); !reflect.DeepEqual(current, middleBefore) || storedValidityAudit(t, middle.f.ID) != nil {
				t.Errorf("stopped row changed: got %v, before %v", current, middleBefore)
			}
			if current := storedValidityEdges(t, items[2].f); !reflect.DeepEqual(current, lastBefore) {
				t.Errorf("later row changed: got %v, before %v", current, lastBefore)
			}
			assertValidityLeaseReleased(t)
			if tc.name == "counted delete rollback" {
				assertValidityPrecondition(t, plan, digest, filepath.Join(t.TempDir(), "retry.jsonl"), "already carries validity_run", true)
			}
		})
	}
}

func TestRepairValidityApplyStopsOnNewMarker(t *testing.T) {
	st := validityFixture(t)
	_, a := applyFixtureSource(t, "marker-a", "Zorbs", true)
	_, b := applyFixtureSource(t, "marker-b", "Zorbs", true)
	if a.ID > b.ID {
		a, b = b, a
	}
	plan := applyFixturePlan(t, st, nil)
	digest := finalizeApplyFixture(t, plan, "")
	report := filepath.Join(t.TempDir(), "apply.jsonl")
	before := graphtest.Snapshot(t)
	params := validityKeyParams(b)
	writes := 0
	st.SetWriteLockWaitObserver(func(time.Duration) {
		writes++
		if writes == 1 {
			// Mark the later source after acquisition, so the first row's
			// own source check cannot detect the intervening write.
			migrationQuery(t, validityFixtureMatch+` SET r.validity_run = 'intervening-run'`, params)
		}
	})
	var stderr bytes.Buffer
	code := runValidityApply(t.Context(), st, os.Getenv("GKG_LABELS_PRESETS_DIR"), plan, digest, report, &stderr)
	st.SetWriteLockWaitObserver(nil)
	const reason = "validity_run appeared after preflight"
	if code != 4 || !strings.Contains(stderr.String(), reason) {
		t.Errorf("apply exit=%d, want 4 and %q: %s", code, reason, &stderr)
	}
	rows, summary := readValidityLedger(t, report)
	if len(rows) != 2 || rows[0].ID != a.ID || rows[0].ActualOutcome != nil || rows[1].ID != b.ID || rows[1].ActualOutcome != nil ||
		summary.Status != "stopped" || summary.StopRow != a.ID || !strings.Contains(summary.StopReason, reason) || len(summary.Actual.Counts) != 0 {
		t.Errorf("stopped ledger: rows %+v, summary %+v", rows, summary)
	}
	if edges := storedValidityEdges(t, b); len(edges) != 1 || edges[0]["validity_run"] != "intervening-run" {
		t.Errorf("intervening marker missing: %v", edges)
	}
	migrationQuery(t, validityFixtureMatch+` REMOVE r.validity_run`, params)
	before.AssertJudgeLeaseFenceAdvanced(t, "apply stopped at the first row", 1)
}

func replaceRewriteEvidence(t *testing.T, plan string) {
	t.Helper()
	rewriteFixtureDraft(t, plan, nil, func(raw []byte) []byte {
		header, content, _ := bytes.Cut(raw, []byte("\n"))
		var row map[string]json.RawMessage
		if err := json.Unmarshal(content, &row); err != nil {
			t.Fatal(err)
		}
		var request judge.Request
		if err := json.Unmarshal(row["request"], &request); err != nil {
			t.Fatal(err)
		}
		request.RequestBytes = bytes.Replace(request.RequestBytes, []byte(" use Quarin "), []byte(" made Quarin "), 1)
		if !bytes.Contains(request.RequestBytes, []byte(" made Quarin ")) {
			t.Fatal("could not change the invented request evidence")
		}
		encodedRequest, err := json.Marshal(request)
		if err != nil {
			t.Fatal(err)
		}
		row["request"] = encodedRequest
		encodedRow, err := json.Marshal(row)
		if err != nil {
			t.Fatal(err)
		}
		return bytes.Join([][]byte{header, encodedRow, nil}, []byte("\n"))
	})
}
