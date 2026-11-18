package judge

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

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
	writeRejudgeTestManifest(t, opt.Manifest, client.Model())
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

func writeRejudgeTestManifest(t *testing.T, path, model string) rejudgeManifestRow {
	t.Helper()
	defs, err := loadRejudgeDefinitions("", "")
	if err != nil {
		t.Fatal(err)
	}
	const head, tail, relation, chunk = "acme", "quillon", "uses", "document-c0"
	row := rejudgeManifestRow{
		RejudgeIdentity: graph.RejudgeIdentity{ID: graph.FactAuditID(head, relation, tail, chunk), HeadKey: head, TailKey: tail, Relation: relation, ChunkID: chunk},
		Request: Request{
			Profile: NewV2Profile(model, map[string]map[string]string{"": defs}), DocID: "document",
		},
	}
	row.Request.RequestBytes, _ = BuildV2Request(V2Input{
		ChunkText: "Acme uses Quillon.", Head: EvidenceMention{Start: 0, End: 4}, Tail: EvidenceMention{Start: 10, End: 17},
		Rel: relation, Definition: defs[relation],
	})
	if err := writeRejudgeAtomic(path, false, func(w io.Writer) error {
		enc := json.NewEncoder(w)
		if err := enc.Encode(rejudgeManifestHeader{Version: rejudgeManifestVersion}); err != nil {
			return err
		}
		return enc.Encode(row)
	}); err != nil {
		t.Fatal(err)
	}
	return row
}

type rejudgeLeaseHook struct {
	*graph.Store
	beforeAcquire func() error
}

func (s rejudgeLeaseHook) AcquireLease(ctx context.Context, name, owner string, ttl time.Duration, force bool) (bool, error) {
	if err := s.beforeAcquire(); err != nil {
		return false, err
	}
	return s.Store.AcquireLease(ctx, name, owner, ttl, force)
}

type rejudgeTestTransport func(*http.Request) (*http.Response, error)

func (f rejudgeTestTransport) RoundTrip(req *http.Request) (*http.Response, error) { return f(req) }

func rejudgeNoProvider(t *testing.T) *Client {
	t.Helper()
	var cfg config.Config
	cfg.Judge.BaseURL, cfg.Judge.Model = "http://rejudge.test", "fake-judge"
	client := NewClient(cfg)
	client.SetBackoff(nil)
	client.client.Transport = rejudgeTestTransport(func(*http.Request) (*http.Response, error) {
		t.Error("unexpected provider call")
		return nil, errors.New("unexpected provider call")
	})
	return client
}

func TestRejudgeRejectsNonPrivateManifest(t *testing.T) {
	client := rejudgeNoProvider(t)
	private := filepath.Join(t.TempDir(), "private.jsonl")
	writeRejudgeTestManifest(t, private, client.Model())
	data, err := os.ReadFile(private)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name string
		make func(string) error
	}{
		{"0644 copy", func(path string) error {
			if err := os.WriteFile(path, data, 0600); err != nil {
				return err
			}
			return os.Chmod(path, 0644)
		}},
		{"symlink", func(path string) error { return os.Symlink(private, path) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			opt := RejudgeOptions{Manifest: filepath.Join(dir, "m.jsonl"), Out: filepath.Join(dir, "v.jsonl"), BudgetAttempts: 1}
			if err := tc.make(opt.Manifest); err != nil {
				t.Fatal(err)
			}
			leases := rejudgeLeaseHook{beforeAcquire: func() error {
				t.Error("non-private manifest reached lease acquisition")
				return errors.New("unexpected lease acquisition")
			}}
			if _, err := judgeRejudge(t.Context(), leases, nil, client, "", opt); err == nil ||
				!strings.Contains(err.Error(), "read manifest "+opt.Manifest+": m.jsonl must be a regular 0600 file") {
				t.Errorf("non-private manifest error = %v", err)
			}
			if entries, err := os.ReadDir(dir); err != nil || len(entries) != 1 {
				t.Errorf("non-private manifest left output artifacts: %v, %v", entries, err)
			}
		})
	}
}

func TestRejudgeRejectsSnapshotChangedBeforeLease(t *testing.T) {
	st := graphtest.Store(t)
	for _, tc := range []struct {
		name, changed, wantDetail string
	}{
		{"attempts rewrite", "attempts.json", ""},
		{"verdict append", "verdicts.jsonl", "file size changed"},
		{"same-size verdict edit", "verdicts.jsonl", "file digest changed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			client := rejudgeNoProvider(t)
			dir := t.TempDir()
			attempts := filepath.Join(dir, "attempts.json")
			opt := RejudgeOptions{Manifest: filepath.Join(dir, "m.jsonl"), Out: filepath.Join(dir, "verdicts.jsonl"), BudgetAttempts: 2, RaiseCap: true}
			row := writeRejudgeTestManifest(t, opt.Manifest, client.Model())
			_, digest, err := readRejudgeManifest(opt.Manifest)
			if err != nil {
				t.Fatal(err)
			}
			ledger := rejudgeAttempts{Manifest: digest, Verdicts: filepath.Base(opt.Out), Cap: 1}
			if err := ledger.save(attempts, false); err != nil {
				t.Fatal(err)
			}
			verdict := rejudgeVerdict{ID: row.ID, Fingerprint: row.Request.Profile.Fingerprint,
				Key:     RequestKeyV2(row.Request.RequestBytes, row.Request.Profile.Fingerprint),
				Verdict: "confirmed", Confidence: 0.9, Reason: "before"}
			var encoded bytes.Buffer
			if err := json.NewEncoder(&encoded).Encode(verdict); err != nil {
				t.Fatal(err)
			}
			var initial []byte
			if tc.name == "same-size verdict edit" {
				initial = encoded.Bytes()
			}
			if err := os.WriteFile(opt.Out, initial, 0600); err != nil {
				t.Fatal(err)
			}
			var afterMutation map[string][]byte
			leases := rejudgeLeaseHook{Store: st, beforeAcquire: func() error {
				switch tc.name {
				case "attempts rewrite":
					ledger.Cap++
					if err := ledger.save(attempts, true); err != nil {
						return err
					}
				case "verdict append":
					out, err := os.OpenFile(opt.Out, os.O_WRONLY|os.O_APPEND, 0)
					if err != nil {
						return err
					}
					_, err = out.Write(encoded.Bytes())
					if err := errors.Join(err, out.Close()); err != nil {
						return err
					}
				case "same-size verdict edit":
					edited := bytes.Replace(initial, []byte(`"before"`), []byte(`"after!"`), 1)
					if err := os.WriteFile(opt.Out, edited, 0600); err != nil {
						return err
					}
				}
				afterMutation = make(map[string][]byte)
				for _, path := range []string{attempts, opt.Out} {
					data, err := os.ReadFile(path)
					if err != nil {
						return err
					}
					afterMutation[path] = data
				}
				return nil
			}}
			if _, err := judgeRejudge(t.Context(), leases, strictRejudgeStore{st}, client, "", opt); err == nil ||
				!strings.Contains(err.Error(), tc.changed+" changed before acquiring the rejudge lease") ||
				!strings.Contains(err.Error(), tc.wantDetail) {
				t.Errorf("changed snapshot error = %v", err)
			}
			if len(afterMutation) != 2 {
				t.Fatal("lease hook did not record the mutated files")
			}
			for path, want := range afterMutation {
				if got, err := os.ReadFile(path); err != nil || !bytes.Equal(got, want) {
					t.Errorf("stale run rewrote %s: %q, %v", path, got, err)
				}
			}
			const owner = "rejudge-snapshot-test"
			if ok, err := st.AcquireLease(t.Context(), rejudgeLeaseName, owner, time.Minute, false); err != nil || !ok {
				t.Fatalf("stale run did not release the lease: %v, %v", ok, err)
			}
			if err := st.ReleaseLease(t.Context(), rejudgeLeaseName, owner); err != nil {
				t.Fatal(err)
			}
		})
	}
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
