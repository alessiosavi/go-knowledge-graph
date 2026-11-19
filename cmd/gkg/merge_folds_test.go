package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/alessiosavi/go-knowledge-graph/internal/graph"
	"github.com/alessiosavi/go-knowledge-graph/internal/graph/graphtest"
)

func TestMergeFoldsCLIUsage(t *testing.T) {
	for _, tc := range []struct {
		args []string
		code int
		want string
	}{
		{[]string{"help"}, 0, "merge-folds -plan -out FILE [-domain SLUG] [-timeout DURATION]"},
		{[]string{"merge-folds", "-h"}, 0, mergeFoldsUsage},
		{[]string{"merge-folds", "-h"}, 0, "(default 10m0s)"},
		{[]string{"merge-folds", "-out", "unused"}, 2, mergeFoldsUsage},
		{[]string{"merge-folds", "-plan"}, 2, mergeFoldsUsage},
		{[]string{"merge-folds", "-plan", "-out=", "unused"}, 2, mergeFoldsUsage},
		{[]string{"merge-folds", "-plan", "-out", "unused", "-domain", "Bad!"}, 2, "invalid domain"},
		{[]string{"merge-folds", "-plan", "-out", "unused", "-timeout", "0s"}, 2, "-timeout must be positive"},
		{[]string{"merge-folds", "-plan", "-out", "unused", "-timeout", "-1m"}, 2, "-timeout must be positive"},
		{[]string{"merge-folds", "-plan", "-out", "unused", "-timeout", "never"}, 2, "invalid value"},
		{[]string{"merge-folds", "-apply"}, 2, "flag provided but not defined"},
	} {
		var out, stderr bytes.Buffer
		if code := run(tc.args, &out, &stderr); code != tc.code || !strings.Contains(out.String()+stderr.String(), tc.want) {
			t.Errorf("%v = %d, stdout %q, stderr %q; want exit %d and %q", tc.args, code, &out, &stderr, tc.code, tc.want)
		}
	}
}

func TestMergeFoldsCLIExistingPath(t *testing.T) {
	path := filepath.Join(t.TempDir(), "plan.jsonl")
	const original = "preserved\n"
	if err := os.WriteFile(path, []byte(original), 0600); err != nil {
		t.Fatal(err)
	}
	var out, stderr bytes.Buffer
	if code := run([]string{"merge-folds", "-plan", "-out", path}, &out, &stderr); code != 1 ||
		out.Len() != 0 || !strings.Contains(stderr.String(), "file exists") {
		t.Fatalf("existing path exit %d, stdout %q, stderr %q", code, &out, &stderr)
	}
	if data, err := os.ReadFile(path); err != nil || string(data) != original {
		t.Fatalf("existing file = %q, %v", data, err)
	}
}

func TestMergeFoldsCLIReport(t *testing.T) {
	preflightCommandStore(t)
	// A missing index makes an accidental Bootstrap observable in the snapshot.
	migrationQuery(t, `DROP INDEX ON :MergedEntity(old_key)`, nil)
	t.Cleanup(func() { migrationQuery(t, `CREATE INDEX ON :MergedEntity(old_key)`, nil) })
	migrationQuery(t, `CREATE
 (a:Entity {id:'a', key:'@zeta:a', name:'Vèlzuno', type:'organization', domain:'zeta', mention_count:2,
            fold_key:'@zeta|velzuno', embedding:[i IN range(1,768) | 1.0]}),
 (b:Entity {id:'b', key:'@zeta:b', name:'Velzuno™', type:'product', domain:'zeta', mention_count:1}),
 (:Entity {key:'@zeta:missing-id', name:'Velzuno', mention_count:99}),
 (c:Chunk {id:'chunk'}), (d:Document {id:'doc'}),
 (d)-[:HAS_CHUNK]->(c), (c)-[:MENTIONS]->(b), (b)-[:uses {judge:'confirmed'}]->(a),
 (:Entity {id:'x', key:'@zeta-two:x', domain:'zeta-two', name:'Velzuno'}),
 (:Entity {id:'y', key:'@zeta-two:y', domain:'zeta-two', name:'Velzuno™'}),
 (:MergedEntity {old_key:'@zeta:z', old_id:'z', survivor_id:'a'}),
 (:FactCandidate {id:$source, head_key:b.key, rel:'uses', tail_key:'x|part', chunk_id:'chunk'}),
 (:FactCandidate {id:$collision, head_key:a.key, rel:'uses', tail_key:'x', chunk_id:'part|chunk'})`,
		map[string]any{
			"source":    graph.CandidateID("@zeta:b", "uses", "x|part", "chunk"),
			"collision": graph.CandidateID("@zeta:a", "uses", "x", "part|chunk"),
		})
	path := filepath.Join(t.TempDir(), "plan.jsonl")
	invoke := func(wantCode int, extraArgs ...string) (string, string) {
		t.Helper()
		var out, stderr bytes.Buffer
		args := append([]string{"merge-folds", "-plan", "-out", path, "-domain", "zeta"}, extraArgs...)
		if code := run(args, &out, &stderr); code != wantCode {
			t.Fatalf("exit %d, stdout %q, stderr %q; want %d", code, &out, &stderr, wantCode)
		}
		return out.String(), stderr.String()
	}
	checkRefusal := func(want string) {
		t.Helper()
		before := graphtest.Snapshot(t)
		out, stderr := invoke(2)
		if out != "" || !strings.Contains(stderr, want) || !strings.Contains(stderr, "run gkg fold rebuild") {
			t.Fatalf("refusal stdout %q, stderr %q; want %q", out, stderr, want)
		}
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatalf("refusal left an output file: stat error %v", err)
		}
		before.AssertUnchanged(t, "merge-folds refusal")
	}
	checkRefusal("fold record is missing")
	migrationQuery(t, `CREATE (:GkgMeta {id:'fold', fold_version:$version, status:'complete', exceptions_digest:'fixture-digest'})`,
		map[string]any{"version": graph.FoldKeyVersion + 1})
	checkRefusal("fold version")
	migrationQuery(t, `MATCH (m:GkgMeta {id:'fold'}) SET m.fold_version=$version, m.status='running'`,
		map[string]any{"version": graph.FoldKeyVersion})
	checkRefusal("status is running")
	migrationQuery(t, `MATCH (m:GkgMeta {id:'fold'}) SET m.status='complete'`, nil)

	before := graphtest.Snapshot(t)
	out, stderr := invoke(1, "-timeout", "1ns")
	if out != "" || (!strings.Contains(stderr, "deadline exceeded") && !strings.Contains(strings.ToLower(stderr), "timeout")) {
		t.Fatalf("timeout stdout %q, stderr %q", out, stderr)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("timeout left an output file: stat error %v", err)
	}
	before.AssertUnchanged(t, "merge-folds timeout")

	out, stderr = invoke(0)
	const summary = "groups=1 members=2 type_mismatches=1 blocking_groups=1 max_rewire=3 p95_rewire=3 stale_hints=1 skipped_rows=1\n"
	if out != summary || stderr != "" {
		t.Fatalf("stdout = %q, stderr = %q", out, stderr)
	}
	t.Log(strings.TrimSpace(out))
	before.AssertUnchanged(t, "merge-folds plan")
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0600 {
		t.Fatalf("output mode = %o, want 600", info.Mode().Perm())
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	lines := bytes.SplitAfter(data, []byte("\n"))
	if len(lines) != 4 || len(lines[3]) != 0 {
		t.Fatalf("expected header, group, trailer and final newline: %s", data)
	}
	var h graph.MergeFoldPlanHeader
	if err := json.Unmarshal(lines[0], &h); err != nil || h.PlanVersion != 1 || h.FoldVersion != graph.FoldKeyVersion ||
		h.ExceptionsDigest != "fixture-digest" || h.EntityCount != 2 || h.SkippedRows != 1 || h.GeneratedAt.IsZero() {
		t.Fatalf("header = %+v, %v", h, err)
	}
	var g graph.MergeFoldGroup
	if err := json.Unmarshal(lines[1], &g); err != nil || g.Fold != "@zeta|velzuno" || g.Canonical != "@zeta:a" ||
		!g.TypeMismatch || g.Rewire != 3 || len(g.Members) != 2 || len(g.Blocking) != 1 {
		t.Fatalf("group = %+v, %v", g, err)
	}
	var trailer map[string]string
	digest := fmt.Sprintf("%x", sha256.Sum256(data[:len(lines[0])+len(lines[1])]))
	if err := json.Unmarshal(lines[2], &trailer); err != nil || len(trailer) != 1 || trailer["sha256"] != digest {
		t.Fatalf("trailer = %+v, %v; want %s", trailer, err, digest)
	}
}
