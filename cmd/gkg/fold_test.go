package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/alessiosavi/go-knowledge-graph/internal/graph"
	"github.com/alessiosavi/go-knowledge-graph/internal/quality"
)

func TestFoldRebuildCLIUsage(t *testing.T) {
	malformed := filepath.Join(t.TempDir(), "malformed.txt")
	if err := os.WriteFile(malformed, []byte("@:zorbulo\n"), 0600); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		args []string
		code int
		want string
	}{
		{[]string{"help"}, 0, "fold rebuild"},
		{[]string{"fold", "rebuild", "-h"}, 0, "-clear-exceptions"},
		{[]string{"fold", "rebuild", "-exceptions", malformed, "-clear-exceptions"}, 2, "mutually exclusive"},
		{[]string{"fold", "rebuild", "-exceptions", malformed}, 2, "line 1"},
		{[]string{"fold", "rebuild", "-exceptions", malformed + ".absent"}, 2, "open"},
		{[]string{"fold", "rebuild", "-exceptions="}, 2, "file path"},
		{[]string{"fold", "rebuild", "surplus"}, 2, "unexpected argument"},
	} {
		var out, stderr bytes.Buffer
		if code := run(tc.args, &out, &stderr); code != tc.code || !strings.Contains(out.String()+stderr.String(), tc.want) {
			t.Errorf("%v = %d, stdout %q, stderr %q; want exit %d and %q", tc.args, code, &out, &stderr, tc.code, tc.want)
		}
	}
}

func TestFoldRebuildCLIExceptionsAndQuality(t *testing.T) {
	st := preflightCommandStore(t)
	ctx := t.Context()
	invoke := func(wantCode int, args ...string) (string, string) {
		t.Helper()
		var out, stderr bytes.Buffer
		if code := run(args, &out, &stderr); code != wantCode {
			t.Fatalf("%v = %d, stdout %q, stderr %q; want exit %d", args, code, &out, &stderr, wantCode)
		}
		return out.String(), stderr.String()
	}
	_, stderr := invoke(1, "fold", "rebuild", "-clear-exceptions")
	if !strings.Contains(stderr, "requires the current extraction schema") {
		t.Fatal(stderr)
	}
	if err := st.Bootstrap(ctx); err != nil {
		t.Fatal(err)
	}
	_, stderr = invoke(2, "fold", "rebuild")
	if !strings.Contains(stderr, "first rebuild requires") {
		t.Fatal(stderr)
	}
	report, _ := invoke(0, "quality", "--json")
	if !strings.Contains(report, `"fold": null`) {
		t.Fatalf("quality without a record: %s", report)
	}
	if rec, err := st.ReadFoldRecord(ctx); err != nil || rec != nil {
		t.Fatalf("usage and quality created a record: %+v, %v", rec, err)
	}
	migrationQuery(t, `CREATE (:Entity {id:'one', key:'@demo:zorbulo', name:'Zorbulo'}),
(:Entity {id:'two', key:'@demo:zorbon', name:'Zorbon'})`, nil)
	path := filepath.Join(t.TempDir(), "exceptions.txt")
	write := func(body string) {
		t.Helper()
		if err := os.WriteFile(path, []byte(body), 0600); err != nil {
			t.Fatal(err)
		}
	}
	write("zorbulo\n")
	_, stderr = invoke(2, "fold", "rebuild", "-exceptions", path)
	if !strings.Contains(stderr, "bare key") {
		t.Fatal(stderr)
	}
	write("@demo:zorbulo\n@demo:absent\n")
	out, stderr := invoke(0, "fold", "rebuild", "-exceptions", path)
	if out != "rows_written=1 rows_skipped=0\n" || !strings.Contains(stderr, `"@demo:absent" names no entity`) {
		t.Fatalf("first rebuild output = %q, %q", out, stderr)
	}
	out, _ = invoke(0, "fold", "rebuild")
	if out != "rows_written=0 rows_skipped=0\n" {
		t.Fatal(out)
	}
	write("@demo:zorbon\n")
	out, stderr = invoke(0, "fold", "rebuild", "-exceptions", path)
	if out != "rows_written=2 rows_skipped=0\n" || stderr != "" {
		t.Fatalf("replacement output = %q, %q", out, stderr)
	}
	migrationQuery(t, `MATCH (e:Entity {id:'one'}) SET e.fold_key='stale'`, nil)
	const snapshot = `MATCH (n) RETURN labels(n) AS labels, properties(n) AS props ORDER BY n.id`
	before := migrationQuery(t, snapshot, nil)
	rec, err := st.ReadFoldRecord(ctx)
	if err != nil || rec == nil {
		t.Fatalf("read record = %+v, %v", rec, err)
	}
	report, stderr = invoke(0, "quality", "--json", "--fold-check")
	var got quality.Report
	if err := json.Unmarshal([]byte(report), &got); err != nil {
		t.Fatal(err)
	}
	fold := got.Fold
	if fold == nil || fold.Status != "complete" || fold.Version != graph.FoldKeyVersion || fold.ExceptionsCount != 1 ||
		fold.ExceptionsDigest != rec.ExceptionsDigest || fold.RebuiltAt != rec.RebuiltAt || fold.RebuiltAt == "" ||
		fold.RowsWritten != 2 || fold.RowsSkipped != 0 || fold.RowsStale == nil || *fold.RowsStale != 1 || stderr != "" {
		t.Fatalf("quality JSON fold = %+v, stderr %q", fold, stderr)
	}
	if strings.Contains(report, "@demo:zorbon") {
		t.Fatal("quality disclosed an exception key")
	}
	out, stderr = invoke(0, "quality")
	for _, line := range []string{"fold.version: 1", "fold.exceptions_count: 1",
		"fold.exceptions_digest: " + rec.ExceptionsDigest, "fold.rebuilt_at: " + rec.RebuiltAt,
		"fold.rows_written: 2", "fold.rows_skipped: 0", "fold.status: complete\n"} {
		if !strings.Contains(out, line) {
			t.Errorf("quality text lacks %q", line)
		}
	}
	if stderr != "" || strings.Contains(out, "fold.rows_stale") {
		t.Fatalf("quality checked folds without --fold-check: %q, %q", out, stderr)
	}
	if after := migrationQuery(t, snapshot, nil); !reflect.DeepEqual(before, after) {
		t.Fatal("quality modified graph")
	}
	out, stderr = invoke(0, "fold", "rebuild", "-clear-exceptions")
	if out != "rows_written=2 rows_skipped=0\n" || stderr != "" {
		t.Fatalf("clear output = %q, %q", out, stderr)
	}
	migrationQuery(t, `MATCH (m:GkgMeta {id:'fold'}) SET m.status = 'running'
REMOVE m.rebuilt_at, m.rows_written, m.rows_skipped`, nil)
	out, _ = invoke(0, "quality")
	if !strings.Contains(out, "fold.status: running\n") {
		t.Errorf("quality text lacks running rebuild status: %s", out)
	}
	out, _ = invoke(0, "quality", "--json")
	got = quality.Report{}
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatal(err)
	}
	if got.Fold == nil || got.Fold.Status != "running" || got.Fold.RebuiltAt != "" ||
		got.Fold.RowsWritten != 0 || got.Fold.RowsSkipped != 0 {
		t.Errorf("quality JSON running fold = %+v", got.Fold)
	}
}

func TestFoldQualitySkipsLegacyEntityWithoutID(t *testing.T) {
	st := preflightCommandStore(t)
	if err := st.Bootstrap(t.Context()); err != nil {
		t.Fatal(err)
	}
	migrationQuery(t, `CREATE (:Entity {id:'one', key:'zorbon', name:'Zorbon'}),
	(:Entity {key:'no-id', name:'Zorbulo'})`, nil)

	var out, stderr bytes.Buffer
	if code := run([]string{"fold", "rebuild", "-clear-exceptions"}, &out, &stderr); code != 0 || stderr.Len() != 0 {
		t.Fatalf("rebuild exit %d, stderr %q", code, &stderr)
	}
	if out.String() != "rows_written=1 rows_skipped=1\n" {
		t.Fatalf("rebuild output = %q", &out)
	}

	out.Reset()
	if code := run([]string{"quality", "--json", "--fold-check"}, &out, &stderr); code != 0 || stderr.Len() != 0 {
		t.Fatalf("quality exit %d, stderr %q", code, &stderr)
	}
	var report quality.Report
	if err := json.Unmarshal(out.Bytes(), &report); err != nil {
		t.Fatal(err)
	}
	fold := report.Fold
	if fold == nil || fold.Status != "complete" || fold.RowsSkipped != 1 || fold.RowsStale == nil || *fold.RowsStale != 0 {
		t.Fatalf("quality must report one skipped row and zero stale rows after rebuild: %s", &out)
	}
}

func TestFoldQualityVersionMismatch(t *testing.T) {
	st := preflightCommandStore(t)
	if err := st.Bootstrap(t.Context()); err != nil {
		t.Fatal(err)
	}
	migrationQuery(t, `CREATE (:GkgMeta {id:'fold', fold_version:$version})`,
		map[string]any{"version": graph.FoldKeyVersion + 1})
	for _, tc := range []struct {
		args         []string
		want         string
		legacyStatus string
	}{
		{[]string{"quality", "--fold-check"}, "fold.matching: off (version mismatch)\n", "fold.status: unknown\n"},
		{[]string{"quality", "--fold-check", "--json"}, `"matching": "off (version mismatch)"`, `"status": "unknown"`},
	} {
		var out, stderr bytes.Buffer
		if code := run(tc.args, &out, &stderr); code != 0 || stderr.Len() != 0 {
			t.Fatalf("%v exited %d, stderr %q", tc.args, code, &stderr)
		}
		if !strings.Contains(out.String(), tc.want) || strings.Contains(out.String(), "rows_stale") {
			t.Errorf("%v should report %q and omit rows_stale: %s", tc.args, tc.want, &out)
		}
		if !strings.Contains(out.String(), tc.legacyStatus) {
			t.Errorf("%v should report %q for a legacy record", tc.args, tc.legacyStatus)
		}
	}
}
