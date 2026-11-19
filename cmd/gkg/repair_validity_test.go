package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/alessiosavi/go-knowledge-graph/internal/graph"
	"github.com/alessiosavi/go-knowledge-graph/internal/graph/graphtest"
	"github.com/alessiosavi/go-knowledge-graph/internal/judge"
	"github.com/alessiosavi/go-knowledge-graph/internal/validity"
)

func callValidity(args ...string) (int, string) {
	var out bytes.Buffer
	code := run(append([]string{"repair-validity"}, args...), &out, &out)
	return code, out.String()
}

func validityFixture(t *testing.T) *graph.Store {
	t.Helper()
	st := graphtest.Store(t)
	configureRejudgeCLI(t, "http://127.0.0.1:1")
	presets := t.TempDir()
	raw, err := os.ReadFile("../../internal/validity/testdata/invented.yaml")
	if err != nil {
		t.Fatal(err)
	}
	raw = append(raw, []byte("\nrelations:\n  - {name: uses, threshold: 0.5, definition: employs the tail}\n  - {name: made_of, threshold: 0.5, definition: consists of the tail}\n")...)
	for _, preset := range []string{"catalog", "invented"} {
		if err := os.WriteFile(filepath.Join(presets, preset+".yaml"), raw, 0600); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("GKG_LABELS_PRESETS_DIR", presets)
	return st
}

type validityDoc struct{ id, chunk, title, target string }

func seedValidityDoc(t *testing.T, suffix, preset, title, text string) validityDoc {
	t.Helper()
	doc := "validity-" + t.Name() + "-" + suffix
	d := validityDoc{doc, doc + "-c2", title, doc + "-target"}
	migrationQuery(t, `CREATE (d:Document {id:$doc, title:$title, labels:$preset, domain:$doc}),
(d)-[:HAS_CHUNK]->(:Chunk {id:$title_chunk, doc_id:$doc, seq:0, text:$title}),
(d)-[:HAS_CHUNK]->(:Chunk {id:$chunk, doc_id:$doc, seq:2, text:$text})`,
		map[string]any{"doc": d.id, "title": title, "preset": preset, "title_chunk": doc + "-c0", "chunk": d.chunk, "text": text})
	t.Cleanup(func() {
		migrationQuery(t, `MATCH (n) WHERE n.domain = $doc OR n.doc_id = $doc DETACH DELETE n`, map[string]any{"doc": doc})
	})
	return d
}

func (d validityDoc) entity(t *testing.T, suffix, name string, titleMention bool) string {
	t.Helper()
	key := d.id + "-" + suffix
	params := map[string]any{"key": key, "name": name, "domain": d.id, "chunk": d.id + "-c0"}
	migrationQuery(t, `CREATE (:Entity {key:$key, name:$name, domain:$domain, type:'fabric'})`, params)
	if titleMention {
		migrationQuery(t, `MATCH (c:Chunk {id:$chunk}), (e:Entity {key:$key}) CREATE (c)-[:MENTIONS]->(e)`, params)
	}
	return key
}

func (d validityDoc) edge(t *testing.T, head, rel, tail, doc, chunk, state string, confidence float64, hs, he, ts, te int) string {
	t.Helper()
	if rel != "uses" && rel != "made_of" {
		t.Fatal("unexpected fixture relation", rel)
	}
	migrationQuery(t, fmt.Sprintf(`MATCH (h:Entity {key:$head}), (t:Entity {key:$tail})
CREATE (h)-[:%s {head_key:$head, tail_key:$tail, doc_id:$doc, chunk_id:$chunk, judge:$judge, confidence:$confidence,
 head_start:$hs, head_end:$he, tail_start:$ts, tail_end:$te}]->(t)`, rel),
		map[string]any{"head": head, "tail": tail, "doc": doc, "chunk": chunk, "judge": state, "confidence": confidence,
			"hs": hs, "he": he, "ts": ts, "te": te})
	return graph.FactAuditID(head, rel, tail, chunk)
}

func validityRows[T any](t *testing.T, path string) []T {
	t.Helper()
	raw := bytes.TrimSpace(readRejudgeFile(t, path))
	if len(raw) == 0 {
		return nil
	}
	var rows []T
	for line := range bytes.SplitSeq(raw, []byte("\n")) {
		var row T
		if err := json.Unmarshal(line, &row); err != nil {
			t.Fatalf("%s: %v", path, err)
		}
		rows = append(rows, row)
	}
	return rows
}

func TestRepairValidityFlags(t *testing.T) {
	for _, args := range [][]string{
		{"-classify", "-out", "dir", "-manifest", "manifest"},
		{"-plan", "-manifest", "manifest", "-out", "dir"},
		{"-finalize", "-plan", "dir"},
		{"-finalize", "-plan", "dir", "-rewrite-verdicts", "v", "-no-rewrites"},
		{"-finalize", "-plan", "dir", "-no-rewrites", "-out", "other"},
		{"-apply", "-plan", "dir"},
	} {
		if code, _ := callValidity(args...); code != 2 {
			t.Errorf("%v exit=%d, want 2", args, code)
		}
	}
}

func TestRepairValidityClassifier(t *testing.T) {
	validityFixture(t)
	d := seedValidityDoc(t, "selected", "invented", "Zorbs in wib", "zorbs use Quarin.")
	other := seedValidityDoc(t, "other", "catalog", "Zorbs in wib", "zorbs use Quarin.")
	h, tail := d.entity(t, "head", "zorbs", false), d.entity(t, "tail", "Quarin", false)
	add := func(suffix, doc, chunk, judge string, he int) string {
		return d.edge(t, d.entity(t, suffix, "zorbs", false), "uses", tail, doc, chunk, judge, 0.8, 0, he, 10, 16)
	}
	planned := add("planned", d.id, d.chunk, "", 5)
	ambiguous := d.edge(t, h, "uses", tail, d.id, d.chunk, "confirmed", 0.8, 0, 5, 10, 16)
	d.edge(t, h, "uses", tail, d.id, d.chunk, "unclear", 0.8, 0, 5, 10, 16)
	missingDoc := add("missing-doc", "missing", d.chunk, "confirmed", 5)
	missingChunk := add("missing-chunk", d.id, "absent", "confirmed", 5)
	orphan := add("orphan", "missing", "absent", "confirmed", 5)
	table := seedValidityDoc(t, "broken-table", "invented", "Zorbs in wib", "| zorbs | Quarin |")
	tableID := table.edge(t, table.entity(t, "head", "zorbs", false), "uses",
		table.entity(t, "tail", "Quarin", false), table.id, table.chunk, "confirmed", 0.8, 2, 7, 10, 16)
	migrationQuery(t, `MATCH (d:Document {id:$doc}) CREATE (d)-[:HAS_CHUNK]->(:Chunk {id:$one, doc_id:$doc, seq:1, text:'| A | B |'}),
	(d)-[:HAS_CHUNK]->(:Chunk {id:$two, doc_id:$doc, seq:1, text:'| C | D |'})`,
		map[string]any{"doc": table.id, "one": table.id + "-prev", "two": table.id + "-prev-duplicate"})
	unowned := add("unowned", d.id, other.chunk, "confirmed", 5)
	invalid := add("invalid", d.id, d.chunk, "confirmed", 500)
	add("unclear", d.id, d.chunk, "unclear", 5)
	add("refuted", d.id, d.chunk, "refuted", 5)
	add("other-preset", other.id, other.chunk, "confirmed", 5)
	before := graphtest.Snapshot(t)
	dir := t.TempDir()
	args := []string{"-classify", "-preset", "invented", "-out", dir}
	if code, out := callValidity(args...); code != 0 {
		t.Fatalf("classify exit=%d %s", code, out)
	}
	before.AssertUnchanged(t, "classify")
	type inventoryRow struct {
		ID, Bucket string
		Prior      string `json:"prior_judge"`
	}
	rows := validityRows[inventoryRow](t, filepath.Join(dir, "inventory.jsonl"))
	want := map[string]string{planned: "planned", ambiguous: "ambiguous", missingDoc: "unverifiable:missing-document",
		missingChunk: "unverifiable:missing-chunk", orphan: "unverifiable:missing-document",
		tableID: "unverifiable:multiple preceding chunks", unowned: "unverifiable:unowned-chunk", invalid: "unverifiable:invalid or empty head span"}
	for _, row := range rows {
		if bucket, ok := want[row.ID]; !ok || bucket != row.Bucket || (row.ID == planned && row.Prior != "") || (row.ID != planned && row.Prior != "confirmed") {
			t.Errorf("unexpected row %+v, want %s", row, bucket)
		}
		delete(want, row.ID)
	}
	if len(want) != 0 {
		t.Errorf("missing inventory rows %v", want)
	}
	manifest := filepath.Join(t.TempDir(), "manifest.jsonl")
	if code, out := callRejudge(t, "-plan", "-ids", filepath.Join(dir, "planned-ids.txt"), "-out", manifest); code != 0 || !strings.Contains(out, "planned=1") {
		t.Fatalf("rejudge selection exit=%d %s", code, out)
	}
	if code, out := callValidity(args...); code != 2 {
		t.Fatalf("overwrite exit=%d %s", code, out)
	}
}

type validityManifestRow struct {
	graph.RejudgeIdentity
	Request  judge.Request
	SourceID string `json:"source_id"`
}

func validityCommandOK(t *testing.T, args ...string) {
	t.Helper()
	if code, out := callValidity(args...); code != 0 {
		t.Fatalf("%v exit=%d %s", args, code, out)
	}
}

func runCachedValidityJudge(t *testing.T, st *graph.Store, manifest string, statuses map[string]string) string {
	t.Helper()
	rows := validityRows[validityManifestRow](t, manifest)[1:]
	for _, row := range rows {
		status := statuses[row.ID]
		if status == "" {
			status = "confirmed"
		}
		request := row.Request
		v := graph.JudgeVerdict{Key: judge.RequestKeyV2(request.RequestBytes, request.Profile.Fingerprint),
			Status: status, Confidence: 0.9, Reason: "invented fixture", Model: request.Profile.Model,
			PromptVersion: request.Profile.Fingerprint, DocID: request.DocID}
		if err := st.PutJudgeVerdictStrict(t.Context(), v); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { migrationQuery(t, `MATCH (v:JudgeVerdict {key:$key}) DELETE v`, map[string]any{"key": v.Key}) })
	}
	verdicts := filepath.Join(t.TempDir(), "verdicts.jsonl")
	if code, out := callRejudge(t, "-judge", "-manifest", manifest, "-out", verdicts, "-budget-attempts", "1"); code != 0 ||
		!strings.Contains(out, fmt.Sprintf("cache_hits=%d attempts=0", len(rows))) {
		t.Fatalf("rejudge exit=%d %s", code, out)
	}
	assertRejudgeSeal(t, verdicts, true)
	return verdicts
}

func validityBulkRun(t *testing.T, st *graph.Store, statuses map[string]string) (string, string) {
	t.Helper()
	dir := t.TempDir()
	validityCommandOK(t, "-classify", "-out", dir)
	manifest := filepath.Join(t.TempDir(), "manifest.jsonl")
	if code, out := callRejudge(t, "-plan", "-ids", filepath.Join(dir, "planned-ids.txt"), "-out", manifest); code != 0 {
		t.Fatalf("bulk manifest exit=%d %s", code, out)
	}
	return manifest, runCachedValidityJudge(t, st, manifest, statuses)
}

func TestRepairValidityProjection(t *testing.T) {
	st := validityFixture(t)
	const prefix = "zorbs use Quarin"
	const relation = "uses"
	addDoc := func(suffix string) (validityDoc, string) {
		d := seedValidityDoc(t, suffix, "catalog", "Zorbs in wib "+suffix, prefix+" "+suffix+".")
		return d, d.entity(t, "tail", "Quarin", false)
	}
	addSource := func(d validityDoc, tail, tag, name string, confidence float64) string {
		h := d.entity(t, tag, name, false)
		return d.edge(t, h, relation, tail, d.id, d.chunk, "confirmed", confidence, 0, 5, 10, 16)
	}
	addTarget := func(d validityDoc) string { return d.entity(t, "target", strings.ToUpper(d.title), true) }
	d, tail := addDoc("absent")
	absent := addSource(d, tail, "source", "zorbs", 0.8)
	d, tail = addDoc("ambiguous")
	targetA := addTarget(d)
	targetB := d.entity(t, "target2", d.title, true)
	ambiguous := addSource(d, tail, "source", "zorbs", 0.8)
	d, tail = addDoc("self")
	selfTarget := d.entity(t, "target", d.title, true)
	self := d.edge(t, selfTarget, relation, tail, d.id, d.chunk, "", 0.8, 0, 5, 10, 16)
	d, _ = addDoc("loop")
	loop := addSource(d, addTarget(d), "source", "zorbs", 0.8)

	type occupiedScenario struct {
		source, occupant, prior, projected string
	}
	var occupied []occupiedScenario
	bulkStatuses := make(map[string]string)
	for _, s := range []struct {
		tag, stored, projected string
		outside, multiple      bool
	}{
		{tag: "confirmed", stored: "confirmed", projected: "confirmed"},
		{tag: "unclear", stored: "unclear", projected: "unclear"},
		{tag: "outside", stored: "confirmed", projected: "confirmed", outside: true},
		{tag: "reject", stored: "confirmed"},
		{tag: "multiple", stored: "confirmed", projected: "confirmed", multiple: true},
	} {
		d, tail := addDoc(s.tag)
		target := addTarget(d)
		owner := d.id
		if s.outside {
			external := seedValidityDoc(t, "noncatalog", "invented", "Vexels in wib", "vexels use Quarin.")
			owner = external.id
		}
		occupant := d.edge(t, target, relation, tail, owner, d.chunk, s.stored, 0.8, 0, 5, 10, 16)
		migrationQuery(t, `MATCH (h:Entity {key:$target})-[r:uses]->(:Entity {key:$tail}) SET r.head_surface=h.name`,
			map[string]any{"target": target, "tail": tail})
		if s.multiple {
			d.edge(t, target, relation, tail, owner, d.chunk, "unclear", 0.8, 0, 5, 10, 16)
		}
		if s.projected == "" {
			bulkStatuses[occupant] = "refuted"
		}
		occupied = append(occupied, occupiedScenario{addSource(d, tail, "source", "zorbs", 0.8), occupant, s.stored, s.projected})
	}

	d, tail = addDoc("merged")
	mergedKey := graph.FactAuditID(addTarget(d), relation, tail, d.chunk)
	loser := addSource(d, tail, "low", "zorbs", 0.4)
	tied := []string{addSource(d, tail, "high1", "zorbs", 0.9), addSource(d, tail, "high2", "zorbs", 0.9)}
	slices.Sort(tied)

	const table = "| zorbs | Quarin |"
	const header = "| Zorbs | Fabric |\n| --- | --- |"
	d = seedValidityDoc(t, "table", "catalog", "Zorbs in wib table", table)
	tail = d.entity(t, "tail", "Quarin", false)
	tableTarget := addTarget(d)
	migrationQuery(t, `MATCH (d:Document {id:$doc}) CREATE (d)-[:HAS_CHUNK]->(:Chunk {id:$chunk, doc_id:$doc, seq:1, text:$header})`,
		map[string]any{"doc": d.id, "chunk": d.id + "-c1", "header": header})
	tableSource := d.edge(t, d.entity(t, "source", "zorbs", false), relation, tail, d.id, d.chunk, "confirmed", 0.8, 2, 7, 10, 16)
	tableKey := graph.FactAuditID(tableTarget, relation, tail, d.chunk)
	tableTitle := d.title

	d, tail = addDoc("unclear-rewrite")
	unclearKey := graph.FactAuditID(addTarget(d), relation, tail, d.chunk)
	unclearSource := addSource(d, tail, "source", "zorbs", 0.8)
	d, tail = addDoc("bulk-unclear")
	demote := addSource(d, tail, "source", "zorbs", 0.8)
	bulkStatuses[demote] = "unclear"
	d, tail = addDoc("rule-drop")
	drop := addSource(d, tail, "source", "drindle", 0.8)

	manifest, verdicts := validityBulkRun(t, st, bulkStatuses)
	planArgs := []string{"-plan", "-manifest", manifest, "-verdicts", verdicts}
	draft1, draft2 := t.TempDir(), t.TempDir()
	before := graphtest.Snapshot(t)
	validityCommandOK(t, append(slices.Clone(planArgs), "-out", draft1)...)
	validityCommandOK(t, append(slices.Clone(planArgs), "-out", draft2)...)
	before.AssertUnchanged(t, "validity plan")
	assertValidityDirsEqual(t, draft1, draft2)
	assertValidityRejected(t, draft1, append(slices.Clone(planArgs), "-out", draft1)...)
	assertValidityRejected(t, draft1, "-finalize", "-plan", draft1, "-no-rewrites")
	assertValidityRejected(t, draft1, "-finalize", "-plan", draft1, "-rewrite-verdicts", verdicts)

	inputs := make(map[string]validity.InputsRow)
	for _, row := range validityRows[validity.InputsRow](t, filepath.Join(draft1, "inputs.jsonl")) {
		inputs[row.ID] = row
	}
	for _, s := range []struct {
		id, reason string
		outcome    validity.Outcome
	}{
		{absent, "no-target", validity.OutcomeValidityDrop},
		{ambiguous, "ambiguous-target", validity.OutcomeValidityDrop},
		{self, "reattach-self", validity.OutcomeStamp},
		{loop, "self-loop", validity.OutcomeValidityDrop},
		{tableSource, "", validity.OutcomeRewriteCandidate},
		{tied[0], "", validity.OutcomeRewriteCandidate},
		{loser, "collision-merged:" + tied[0], validity.OutcomeValidityDrop},
		{tied[1], "collision-merged:" + tied[0], validity.OutcomeValidityDrop},
		{drop, "", validity.OutcomeValidityDrop},
		{demote, "", validity.OutcomeDemote},
	} {
		if row := inputs[s.id]; row.Reason != s.reason || row.Outcome != s.outcome {
			t.Errorf("input %s = %+v, want %s %s", s.id, row, s.outcome, s.reason)
		}
	}
	if got := inputs[ambiguous].Target; got.Key != "" || !slices.Equal(got.TitleMatchKeys, []string{targetA, targetB}) {
		t.Errorf("ambiguous target = %+v", got)
	}
	if got := inputs[tableSource]; got.Target.Key != tableTarget || got.Target.Name != strings.ToUpper(tableTitle) ||
		got.Target.Type != "fabric" || got.Head != "zorbs" || got.Tail != "Quarin" || got.Evidence != table {
		t.Errorf("table parity/target = %+v", got)
	}
	ranks := []validity.CandidateRankInput{{SourceID: tied[0], Confidence: 0.9}, {SourceID: tied[1], Confidence: 0.9}, {SourceID: loser, Confidence: 0.4}}
	for _, id := range []string{tied[0], tied[1], loser} {
		if row := inputs[id]; !slices.Equal(row.CandidateGroup, ranks) || len(row.Occupants) != 0 {
			t.Errorf("collision group for %s = %+v", id, row)
		}
	}
	for i, s := range occupied {
		row := inputs[s.source]
		n := 1
		if i == len(occupied)-1 {
			n = 2
		}
		if row.Reason != "collision-occupied" || len(row.Occupants) != n || len(row.CandidateGroup) != 0 ||
			!slices.ContainsFunc(row.Occupants, func(o validity.Occupant) bool {
				return o.ID == s.occupant && o.Judge == s.prior && o.EdgeID != nil
			}) {
			t.Errorf("occupied row = %+v, want %+v", row, s)
		}
	}

	rewriteManifest := filepath.Join(draft1, "rewrites.jsonl")
	rewrites := validityRows[validityManifestRow](t, rewriteManifest)[1:]
	if len(rewrites) != 3 {
		t.Fatalf("rewrites = %+v", rewrites)
	}
	for _, r := range rewrites {
		switch r.ID {
		case tableKey:
			surface := strings.ToUpper(tableTitle)
			request, reason := judge.BuildV2Request(judge.V2Input{ChunkText: table, PreviousChunkText: header, DocTitle: tableTitle,
				Head:        judge.EvidenceMention{Start: 2, End: 7, MentionType: "fabric"},
				Tail:        judge.EvidenceMention{Start: 10, End: 16, MentionType: "fabric", TypeFallback: true},
				HeadSurface: &surface, Rel: relation, Definition: "employs the tail"})
			if r.SourceID != tableSource || reason != "" || !bytes.Equal(r.Request.RequestBytes, request) {
				t.Errorf("table rewrite = %s, expected %s (%s)", r.Request.RequestBytes, request, reason)
			}
		case mergedKey:
			if r.SourceID != tied[0] {
				t.Errorf("merged rewrite source = %s, want %s", r.SourceID, tied[0])
			}
		case unclearKey:
			if r.SourceID != unclearSource {
				t.Errorf("unclear rewrite source = %s", r.SourceID)
			}
		default:
			t.Errorf("unexpected rewrite %s", r.ID)
		}
	}
	rewriteVerdicts := runCachedValidityJudge(t, st, rewriteManifest, map[string]string{mergedKey: "refuted", unclearKey: "unclear"})
	rewriteRaw := readRejudgeFile(t, rewriteVerdicts)
	first, rest, _ := bytes.Cut(rewriteRaw, []byte("\n"))
	for _, invalid := range [][]byte{rest, append(append(bytes.Clone(rewriteRaw), first...), '\n')} {
		badSeal := writeValidityTestSeal(t, rewriteVerdicts, invalid)
		assertValidityRejected(t, draft1, "-finalize", "-plan", draft1, "-rewrite-verdicts", badSeal)
	}
	before = graphtest.Snapshot(t)
	for _, dir := range []string{draft1, draft2} {
		validityCommandOK(t, "-finalize", "-plan", dir, "-rewrite-verdicts", rewriteVerdicts)
		checkValidityDigests(t, dir)
	}
	before.AssertUnchanged(t, "validity finalize")
	assertValidityDirsEqual(t, draft1, draft2)
	assertValidityRejected(t, draft1, "-finalize", "-plan", draft1, "-rewrite-verdicts", rewriteVerdicts)

	final := make(map[string]validity.FinalRow)
	for _, row := range validityRows[validity.FinalRow](t, filepath.Join(draft1, "final.jsonl")) {
		if _, duplicate := final[row.ID]; duplicate {
			t.Errorf("duplicate final row %s", row.ID)
		}
		final[row.ID] = row
	}
	if len(final) != len(inputs) || final[tableSource].FinalOutcome != validity.OutcomeRewriteConfirmed ||
		final[tableSource].NewID != tableKey || final[unclearSource].FinalOutcome != validity.OutcomeRewriteUnclear ||
		final[unclearSource].NewID != unclearKey || final[tied[0]].Reason != "reattach-refuted" ||
		final[tied[0]].RewriteVerdict != "refuted" || final[tied[0]].NewID != "" ||
		final[occupied[3].occupant].FinalOutcome != validity.OutcomeReject || final[demote].SourceVerdict != "unclear" {
		t.Errorf("unexpected final rows %+v", final)
	}
	for _, source := range []string{tied[1], loser} {
		if row := final[source]; row.FinalOutcome != validity.OutcomeValidityDrop || row.Reason != "collision-merged:"+tied[0] {
			t.Errorf("merged source %+v", row)
		}
	}

	type reportRow struct {
		SourceID     string `json:"source_id"`
		DocID        string `json:"doc_id"`
		ChunkID      string `json:"chunk_id"`
		HasConfirmed bool   `json:"has_confirmed_destination"`
		States       []struct {
			ID, Judge, Outcome string
			Live               bool
		} `json:"destination_states"`
	}
	bulkSources := make(map[string]validityManifestRow)
	for _, row := range validityRows[validityManifestRow](t, manifest)[1:] {
		bulkSources[row.ID] = row
	}
	report := make(map[string]reportRow)
	for _, row := range validityRows[reportRow](t, filepath.Join(draft1, "affected.jsonl")) {
		source := bulkSources[row.SourceID]
		if row.DocID == "" || row.ChunkID == "" || row.DocID != source.Request.DocID || row.ChunkID != source.ChunkID {
			t.Errorf("affected source %s ids = (%q, %q), want (%q, %q)", row.SourceID, row.DocID, row.ChunkID, source.Request.DocID, source.ChunkID)
		}
		report[row.SourceID] = row
	}
	if len(report) != 9 {
		t.Errorf("affected rows = %d, want 9", len(report))
	}
	for _, source := range []string{absent, ambiguous} {
		if r := report[source]; r.SourceID == "" || r.HasConfirmed || len(r.States) != 0 {
			t.Errorf("missing target report = %+v", r)
		}
	}
	for _, source := range []string{tied[1], loser} {
		if r := report[source]; r.HasConfirmed || len(r.States) != 1 || r.States[0].ID != mergedKey ||
			r.States[0].Live || r.States[0].Outcome != "validity-drop" {
			t.Errorf("merged affected report = %+v", r)
		}
	}
	for _, s := range occupied {
		r := report[s.source]
		if r.HasConfirmed != (s.projected == "confirmed") || !slices.ContainsFunc(r.States, func(state struct {
			ID, Judge, Outcome string
			Live               bool
		}) bool {
			return state.ID == s.occupant && state.Judge == s.projected && state.Live == (s.projected != "")
		}) {
			t.Errorf("occupied state = %+v, want %+v", r, s)
		}
	}
	type affectedCounts struct {
		Sources                     int `json:"sources"`
		WithoutConfirmedDestination int `json:"without_confirmed_destination"`
	}
	var summary map[string]affectedCounts
	if err := json.Unmarshal(readRejudgeFile(t, filepath.Join(draft1, "affected-summary.json")), &summary); err != nil {
		t.Fatal(err)
	}
	wantSummary := map[string]affectedCounts{
		"no-target":          {Sources: 2, WithoutConfirmedDestination: 2},
		"collision-occupied": {Sources: 5, WithoutConfirmedDestination: 2},
		"collision-merged":   {Sources: 2, WithoutConfirmedDestination: 2},
	}
	if !maps.Equal(summary, wantSummary) {
		t.Errorf("affected summary = %+v, want %+v", summary, wantSummary)
	}
}

func TestRepairValidityInventoryMismatchAndZeroCandidates(t *testing.T) {
	st := validityFixture(t)
	d := seedValidityDoc(t, "zero", "catalog", "Zorbs in wib", "Nebula Fabric uses Quarin.")
	h, tail := d.entity(t, "head", "Nebula Fabric", false), d.entity(t, "tail", "Quarin", false)
	d.edge(t, h, "uses", tail, d.id, d.chunk, "confirmed", 0.8, 0, 13, 19, 25)
	manifest, verdicts := validityBulkRun(t, st, nil)
	out := t.TempDir()
	args := []string{"-plan", "-manifest", manifest, "-verdicts", verdicts, "-out", out}
	setJudge := func(state string) {
		migrationQuery(t, `MATCH (:Entity {key:$head})-[r:uses]->(:Entity {key:$tail}) SET r.judge=$state`,
			map[string]any{"head": h, "tail": tail, "state": state})
	}
	setJudge("")
	if got := assertValidityRejected(t, out, args...); !strings.Contains(got, "prior judge") {
		t.Errorf("prior mismatch reporting: %s", got)
	}
	setJudge("unclear")
	if got := assertValidityRejected(t, out, args...); !strings.Contains(got, "inventory mismatch") {
		t.Errorf("population mismatch reporting: %s", got)
	}
	setJudge("confirmed")
	setTitle := func(title string) {
		migrationQuery(t, `MATCH (d:Document {id:$doc}) SET d.title=$title`, map[string]any{"doc": d.id, "title": title})
	}
	setTitle(d.title + " revised")
	if got := assertValidityRejected(t, out, args...); !strings.Contains(got, "differs from the live identity or judge request") {
		t.Errorf("request mismatch reporting: %s", got)
	}
	setTitle(d.title)
	raw := readRejudgeFile(t, verdicts)
	if err := os.WriteFile(verdicts, bytes.Replace(raw, []byte("invented fixture"), []byte("altered"), 1), 0600); err != nil {
		t.Fatal(err)
	}
	assertValidityRejected(t, out, args...)
	if err := os.WriteFile(verdicts, raw, 0600); err != nil {
		t.Fatal(err)
	}
	validityCommandOK(t, args...)
	assertValidityRejected(t, out, "-finalize", "-plan", out, "-rewrite-verdicts", verdicts)
	inventory := filepath.Join(out, "inventory.jsonl")
	original := readRejudgeFile(t, inventory)
	if err := os.WriteFile(inventory, append(bytes.Clone(original), original...), 0600); err != nil {
		t.Fatal(err)
	}
	assertValidityRejected(t, out, "-finalize", "-plan", out, "-no-rewrites")
	if err := os.WriteFile(inventory, original, 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GKG_CONFIG", filepath.Join(out, "missing-config.yaml"))
	validityCommandOK(t, "-finalize", "-plan", out, "-no-rewrites")
	if rows := validityRows[validity.FinalRow](t, filepath.Join(out, "final.jsonl")); len(rows) != 1 || rows[0].FinalOutcome != validity.OutcomeStamp {
		t.Errorf("zero-candidate final rows: %+v", rows)
	}
	checkValidityDigests(t, out)
}

func writeValidityTestSeal(t *testing.T, from string, verdicts []byte) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, filepath.Base(from))
	attempts := readRejudgeFile(t, filepath.Join(filepath.Dir(from), "attempts.json"))
	var ledger map[string]any
	if err := json.Unmarshal(attempts, &ledger); err != nil {
		t.Fatal(err)
	}
	ledger["verdicts_sha256"] = fmt.Sprintf("%x", sha256.Sum256(verdicts))
	encoded, err := json.Marshal(ledger)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, verdicts, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "attempts.json"), encoded, 0600); err != nil {
		t.Fatal(err)
	}
	return path
}

func validityFileSnapshot(t *testing.T, dir string) map[string][]byte {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	out := make(map[string][]byte)
	for _, entry := range entries {
		info, err := entry.Info()
		if err != nil || !info.Mode().IsRegular() || info.Mode().Perm() != 0600 {
			t.Fatalf("artifact %s is not a regular 0600 file: %v %v", entry.Name(), info, err)
		}
		out[entry.Name()] = readRejudgeFile(t, filepath.Join(dir, entry.Name()))
	}
	return out
}

func assertValidityFilesEqual(t *testing.T, a, b map[string][]byte) {
	t.Helper()
	if len(a) != len(b) {
		t.Fatalf("artifact count changed from %d to %d", len(a), len(b))
	}
	for key, want := range a {
		if got, ok := b[key]; !ok || !bytes.Equal(got, want) {
			t.Errorf("artifact %s differs", key)
		}
	}
}

func assertValidityDirsEqual(t *testing.T, a, b string) {
	t.Helper()
	assertValidityFilesEqual(t, validityFileSnapshot(t, a), validityFileSnapshot(t, b))
}

func assertValidityRejected(t *testing.T, dir string, args ...string) string {
	t.Helper()
	before := validityFileSnapshot(t, dir)
	code, out := callValidity(args...)
	if code != 2 {
		t.Errorf("%v exit=%d, want 2: %s", args, code, out)
	}
	assertValidityFilesEqual(t, before, validityFileSnapshot(t, dir))
	return out
}

func checkValidityDigests(t *testing.T, dir string) {
	t.Helper()
	// Each tuple component is prefixed with its byte length (uint64 big endian).
	digest := func(parts ...[]byte) string {
		h := sha256.New()
		for _, b := range parts {
			if err := binary.Write(h, binary.BigEndian, uint64(len(b))); err != nil {
				t.Fatal(err)
			}
			_, _ = h.Write(b)
		}
		return fmt.Sprintf("%x", h.Sum(nil))
	}
	files := validityFileSnapshot(t, dir)
	preset := bytes.TrimSuffix(files["preset.txt"], []byte("\n"))
	inventory := digest(preset, files["inventory.jsonl"], files["planned-ids.txt"])
	inputs := validityRows[validity.InputsRow](t, filepath.Join(dir, "inputs.jsonl"))
	if strings.TrimSpace(string(files["inventory.sha256"])) != inventory || inputs[0].InventoryDigest != inventory {
		t.Error("inventory digest or its draft binding differs")
	}
	summary, err := validity.DecodePlanSummary(files["plan.json"])
	if err != nil {
		t.Fatal(err)
	}
	m := fmt.Sprintf("%x", sha256.Sum256(files["source-manifest.jsonl"]))
	v := fmt.Sprintf("%x", sha256.Sum256(files["source-verdicts.jsonl"]))
	lexicon := fmt.Sprintf("%x", sha256.Sum256(files["lexicon.json"]))
	draft := digest([]byte(m), []byte(v), files["inputs.jsonl"], files["rewrites.jsonl"], []byte(summary.RulesID), []byte(lexicon))
	if m != summary.ManifestDigest || v != summary.VerdictSeal || lexicon != summary.LexiconDigest || draft != summary.DraftDigest {
		t.Errorf("draft digest differs: %s", draft)
	}
	final, err := validity.DecodePlanSummary(files["final-plan.json"])
	if err != nil {
		t.Fatal(err)
	}
	v2 := "no-rewrites"
	if rewrite, exists := files["rewrite-verdicts.jsonl"]; exists {
		v2 = fmt.Sprintf("%x", sha256.Sum256(rewrite))
	}
	if final.DraftDigest != draft || final.RewriteVerdictSeal != v2 ||
		final.FinalDigest != digest([]byte(draft), []byte(v2), files["final.jsonl"]) {
		t.Errorf("final digest differs: %+v", final)
	}
}
