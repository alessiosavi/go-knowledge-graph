package judge

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/alessiosavi/go-knowledge-graph/internal/config"
	"github.com/alessiosavi/go-knowledge-graph/internal/graph"
	"github.com/alessiosavi/go-knowledge-graph/internal/graph/graphtest"
)

type failingRejudgeCache struct{ failure error }

func (s failingRejudgeCache) JudgeVerdict(context.Context, string) (graph.JudgeVerdict, bool, error) {
	return graph.JudgeVerdict{}, false, s.failure
}

func (s failingRejudgeCache) PutJudgeVerdict(context.Context, graph.JudgeVerdict) error {
	panic("cache read error must stop before the write")
}

func TestRejudgeCacheReadErrorStopsBeforeDebit(t *testing.T) {
	st := graphtest.Store(t)
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		t.Error("strict cache read failure reached provider")
		http.Error(w, "unexpected", http.StatusBadRequest)
	}))
	defer provider.Close()
	var cfg config.Config
	cfg.Judge.BaseURL, cfg.Judge.Model = provider.URL, "fake-judge"
	client := NewClient(cfg)
	dir := t.TempDir()
	opt := RejudgeOptions{Manifest: filepath.Join(dir, "m.jsonl"), Out: filepath.Join(dir, "v.jsonl"), BudgetAttempts: 10}
	defs, err := loadRejudgeDefinitions("", "")
	if err != nil {
		t.Fatal(err)
	}
	const head, tail, relation, chunk = "acme", "quillon", "uses", "document-c0"
	row := rejudgeManifestRow{
		RejudgeIdentity: graph.RejudgeIdentity{ID: graph.FactAuditID(head, relation, tail, chunk), HeadKey: head, TailKey: tail, Relation: relation, ChunkID: chunk},
		Request: Request{
			Profile: NewV2Profile(client.Model(), map[string]map[string]string{"": defs}), DocID: "document",
		},
	}
	row.Request.RequestBytes, _ = BuildV2Request(V2Input{
		ChunkText: "Acme uses Quillon.", Head: EvidenceMention{Start: 0, End: 4}, Tail: EvidenceMention{Start: 10, End: 17},
		Rel: relation, Definition: defs[relation],
	})
	if err := writeRejudgeAtomic(opt.Manifest, false, func(w io.Writer) error {
		enc := json.NewEncoder(w)
		if err := enc.Encode(rejudgeManifestHeader{Version: rejudgeManifestVersion}); err != nil {
			return err
		}
		return enc.Encode(row)
	}); err != nil {
		t.Fatal(err)
	}
	before := graphtest.Snapshot(t)
	cacheErr := errors.New("test cache offline")
	result, err := judgeRejudge(t.Context(), st, failingRejudgeCache{cacheErr}, client, "", opt)
	if !errors.Is(err, cacheErr) || result.Attempts != 0 {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	if f, err := os.Stat(opt.Out); err != nil || f.Size() != 0 {
		t.Fatalf("read error wrote verdicts: %v %v", f, err)
	}
	before.AssertUnchanged(t, "failed cache lookup")
}

func TestRejudgeManifestAtomicPublication(t *testing.T) {
	path := filepath.Join(t.TempDir(), "manifest.jsonl")
	abort := errors.New("cannot finish manifest")
	err := writeRejudgeAtomic(path, false, func(w io.Writer) error {
		_, _ = io.WriteString(w, "incomplete manifest")
		if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("manifest visible while writing: %v", err)
		}
		return abort
	})
	if !errors.Is(err, abort) {
		t.Fatalf("write error = %v", err)
	}
	if entries, err := os.ReadDir(filepath.Dir(path)); err != nil || len(entries) != 0 {
		t.Fatalf("failed write left artifacts: %v %v", entries, err)
	}
	err = writeRejudgeAtomic(path, false, func(w io.Writer) error {
		// Simulate an independent writer arriving after the work has begun.
		if err := os.WriteFile(path, []byte("winner"), 0600); err != nil {
			return err
		}
		_, err := io.WriteString(w, "loser")
		return err
	})
	if !errors.Is(err, os.ErrExist) {
		t.Fatalf("exclusive publication error = %v", err)
	}
	if got, err := os.ReadFile(path); err != nil || string(got) != "winner" {
		t.Fatalf("existing artifact = %q, %v", got, err)
	}
}
