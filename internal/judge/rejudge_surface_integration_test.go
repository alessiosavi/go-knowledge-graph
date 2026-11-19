package judge_test

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/alessiosavi/go-knowledge-graph/internal/graph"
	"github.com/alessiosavi/go-knowledge-graph/internal/graph/graphtest"
	"github.com/alessiosavi/go-knowledge-graph/internal/judge"
)

func TestPlanRejudgeReattachedHead(t *testing.T) {
	const target = "Quill Pro"
	const prose = "Introduction. Nebula uses Nova. Closing sentence."
	const header = "| Material | API |\n| --- | --- |"
	const definition = "the head itself employs the tail (a tool, library, technology or service)."
	st := graphtest.Store(t)
	for _, tc := range []struct {
		name, text, previous     string
		surface                  any
		mentionOnTarget, invalid bool
	}{
		{name: "prose", text: prose, surface: target},
		{name: "table", text: "| Nebula | Nova |", previous: header, surface: target},
		{name: "typed mention on target at source span", text: prose, surface: target, mentionOnTarget: true},
		{name: "empty surface", text: prose, surface: "", invalid: true},
		{name: "whitespace surface", text: prose, surface: " \t\u00a0", invalid: true},
		{name: "non-string surface", text: prose, surface: []any{target}, invalid: true},
		{name: "untrimmed surface", text: prose, surface: " Quill Pro ", invalid: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			in := v2Input(t, tc.text, "Nebula", "Nova")
			in.PreviousChunkText = tc.previous
			in.DocTitle, in.Definition = target, definition
			in.Head.MentionType, in.Tail.MentionType = "product", "technology"
			domain := t.Name()
			id := seedReattachedHead(t, domain, in, tc.surface, tc.mentionOnTarget)
			path := filepath.Join(t.TempDir(), "manifest.jsonl")
			selection := judge.RejudgeSelection{Domain: domain, Labels: []judge.AuditRow{{ID: id}}}
			result, err := judge.PlanRejudge(t.Context(), st, "fake-judge", "../../labels", path, selection)
			if tc.invalid {
				if !errors.Is(err, judge.ErrRejudgeAuditInput) || result.Planned != 0 || result.Unverifiable["invalid head_surface"] != 1 {
					t.Fatalf("plan = %+v, %v, want an unverifiable head_surface", result, err)
				}
				if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("invalid edge published a manifest, stat error = %v", err)
				}
				return
			}
			if err != nil || result.Planned != 1 {
				t.Fatalf("plan = %+v, %v, want one rewritten edge", result, err)
			}
			manifest, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			_, rawRow, _ := bytes.Cut(manifest, []byte("\n"))
			var row struct {
				ID      string
				Request judge.Request
			}
			if err := json.Unmarshal(rawRow, &row); err != nil || row.ID != id {
				t.Fatalf("manifest row = %s, error = %v, want id %q", rawRow, err, id)
			}
			var payload v2Payload
			if err := json.Unmarshal(row.Request.RequestBytes, &payload); err != nil {
				t.Fatal(err)
			}
			if payload.Fact.Head != target || payload.HeadMentionType != "product" || !payload.HeadTypeFallback {
				t.Errorf("rewritten head in manifest = %s", row.Request.RequestBytes)
			}
			if tc.previous != "" && (payload.TableHeader == nil || *payload.TableHeader != header) {
				t.Errorf("previous-chunk header missing from %s", row.Request.RequestBytes)
			}
			surface := target
			in.HeadSurface = &surface
			_, want := buildV2(t, in)
			if !bytes.Equal(row.Request.RequestBytes, want) {
				t.Errorf("manifest request = %s, want builder bytes %s", row.Request.RequestBytes, want)
			}
			if err := st.WalkRejudgeFacts(t.Context(), domain, []string{id}, 0, func(facts []graph.RejudgeFact) error {
				if len(facts) != 1 || facts[0].HeadType != "product" || !facts[0].HeadTypeFallback {
					t.Errorf("hydrated facts = %+v, want target entity type and fallback", facts)
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func seedReattachedHead(t *testing.T, domain string, in judge.V2Input, surface any, mentionOnTarget bool) string {
	t.Helper()
	docID := domain + "-doc"
	chunk, head, tail := docID+"-c2", domain+"-head", domain+"-tail"
	params := map[string]any{
		"domain": domain, "doc": docID, "title": in.DocTitle, "text": in.ChunkText,
		"head": head, "tail": tail, "source": domain + "-source", "chunk": chunk,
		"target_chunk": docID + "-c0", "prev_chunk": docID + "-c1", "previous": in.PreviousChunkText,
		"surface": surface, "hs": in.Head.Start, "he": in.Head.End, "ts": in.Tail.Start, "te": in.Tail.End,
		"lineage": graph.FactAuditID(domain+"-source", "uses", tail, chunk),
	}
	backfillQuery(t, `CREATE (d:Document {id:$doc, title:$title, labels:'documentation', domain:$domain}),
 (d)-[:HAS_CHUNK]->(other:Chunk {id:$target_chunk, doc_id:$doc, seq:0, text:$title}),
 (d)-[:HAS_CHUNK]->(:Chunk {id:$prev_chunk, doc_id:$doc, seq:1, text:$previous}),
 (d)-[:HAS_CHUNK]->(c:Chunk {id:$chunk, doc_id:$doc, seq:2, text:$text}),
 (h:Entity {key:$head, domain:$domain, name:$title, type:'product'}),
 (source:Entity {key:$source, domain:$domain, name:'Nebula', type:'material'}),
 (t:Entity {key:$tail, domain:$domain, name:'Nova', type:'service'}),
 (other)-[:MENTIONS {start:0, end:size($title), type:'product'}]->(h),
 (c)-[:MENTIONS {start:$hs, end:$he, type:'material'}]->(source),
 (c)-[:MENTIONS {start:$ts, end:$te, type:'technology'}]->(t),
 (h)-[:uses {head_key:$head, tail_key:$tail, chunk_id:$chunk, doc_id:$doc, judge:'confirmed',
             head_start:$hs, head_end:$he, tail_start:$ts, tail_end:$te, head_surface:$surface,
             reattached_from:$lineage, validity_rule:'fixture', validity_run:'fixture',
             confidence:0.8, low_confidence:false, extractor_version:'fixture:1'}]->(t)`, params)
	t.Cleanup(func() {
		backfillQuery(t, `MATCH (n) WHERE n.domain = $domain OR n.doc_id = $doc DETACH DELETE n`, params)
	})
	if mentionOnTarget {
		backfillQuery(t, `MATCH (c:Chunk {id:$chunk}), (h:Entity {key:$head})
CREATE (c)-[:MENTIONS {start:$hs, end:$he, type:'material'}]->(h)`, params)
	}
	return graph.FactAuditID(head, "uses", tail, chunk)
}
