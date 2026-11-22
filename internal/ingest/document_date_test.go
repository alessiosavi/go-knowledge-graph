package ingest

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/alessiosavi/go-knowledge-graph/internal/graph"
	"github.com/alessiosavi/go-knowledge-graph/internal/graph/graphtest"
)

const (
	initialDocumentDate = "2024-03-01T00:00:00.000000000Z"
	changedDocumentDate = "2023-03-01T00:00:00.000000000Z"
)

func documentDatePipeline(t *testing.T) (*Pipeline, *graph.Store, Input) {
	t.Helper()
	st := graphtest.Store(t)
	p, _ := spoolPipeline(t)
	p.store = st
	p.cfg.Facts.RequireConfirmation = false
	in := Input{Source: fmt.Sprintf("date-%d", time.Now().UnixNano()), Text: "The original catalogue page.", DocumentDate: initialDocumentDate}
	return p, st, in
}

func submitDateDocument(t *testing.T, p *Pipeline, st *graph.Store, in Input) string {
	t.Helper()
	res := submitSpool(t, p, in)
	if res.Noop || res.DocumentID == "" {
		t.Fatalf("initial Submit = %+v", res)
	}
	t.Cleanup(func() { _ = st.DeleteDocument(context.Background(), res.DocumentID) })
	select {
	case got := <-p.jobs:
		if got != res.DocumentID {
			t.Fatalf("queued %q, want %q", got, res.DocumentID)
		}
	default:
		t.Fatal("initial submit enqueued nothing")
	}
	return res.DocumentID
}

func readDateDocument(t *testing.T, st *graph.Store, id string) *graph.Document {
	t.Helper()
	doc, err := st.Document(t.Context(), id)
	if err != nil {
		t.Fatal(err)
	}
	return doc
}

func getStoredVersion(t *testing.T, st *graph.Store, id string) string {
	t.Helper()
	r, err := st.RunReadQuery(t.Context(), "MATCH (d:Document {id:$id}) RETURN d.version_at AS version",
		map[string]any{"id": id}, 1)
	if err != nil || len(r.Rows) != 1 {
		t.Fatalf("read version for %s: %+v, %v", id, r, err)
	}
	return r.Rows[0]["version"].(string)
}

func setDateDocumentDone(t *testing.T, st *graph.Store, id string) {
	t.Helper()
	if err := st.SetStatus(t.Context(), id, graph.StatusDone, ""); err != nil {
		t.Fatal(err)
	}
}

func expectDateNoop(t *testing.T, p *Pipeline, in Input, id string) {
	t.Helper()
	res := submitSpool(t, p, in)
	if !res.Noop || res.DocumentID != id {
		t.Fatalf("duplicate Submit = %+v, want no-op on %s", res, id)
	}
	select {
	case job := <-p.jobs:
		t.Fatalf("duplicate enqueued %q", job)
	default:
	}
}

type dateUpsertObserver struct {
	*graph.Store
	beforeUpsert func()
}

func (s dateUpsertObserver) UpsertDocument(ctx context.Context, in graph.DocumentInput) (graph.UpsertResult, error) {
	s.beforeUpsert()
	return s.Store.UpsertDocument(ctx, in)
}

func TestSubmitDatedDuplicateUpdatesDateAndRestoresSpool(t *testing.T) {
	p, st, in := documentDatePipeline(t)
	in.Title = "Previous"
	id := submitDateDocument(t, p, st, in)
	setDateDocumentDone(t, st, id)
	before := readDateDocument(t, st, id)
	version := getStoredVersion(t, st, id)
	path := filepath.Join(p.spool.dir, spoolName(id, before.Hash))
	payload, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	p.store = dateUpsertObserver{Store: st, beforeUpsert: func() {
		backup, err := os.ReadFile(path + spoolBackupSuffix)
		if err != nil || !bytes.Equal(backup, payload) {
			t.Errorf("before upsert, backup = %s, %v", backup, err)
		}
		written, err := p.spool.loadForTest(path)
		if err != nil || written.Loaded.Title != "Candidate" {
			t.Errorf("before upsert, candidate = %+v, %v", written, err)
		}
	}}
	in.Title, in.DocumentDate = "Candidate", changedDocumentDate
	expectDateNoop(t, p, in, id)
	after := readDateDocument(t, st, id)
	if after.RawDocumentDate != changedDocumentDate || after.Status != before.Status ||
		!after.UpdatedAt.Equal(before.UpdatedAt) || after.SpoolPayloadDigest != before.SpoolPayloadDigest ||
		getStoredVersion(t, st, id) != version {
		t.Errorf("after duplicate = %+v, want only the date changed from %+v", after, before)
	}
	restored, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(restored, payload) {
		t.Errorf("after no-op, payload = %s, %v, want %s", restored, err, payload)
	}
	if _, err := os.Stat(path + spoolBackupSuffix); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("after no-op, backup stat = %v, want not exist", err)
	}
}

func TestSubmitDatedDuplicateBusyThenRetry(t *testing.T) {
	p, st, in := documentDatePipeline(t)
	id := submitDateDocument(t, p, st, in)
	setDateDocumentDone(t, st, id)
	before := readDateDocument(t, st, id)
	in.DocumentDate = changedDocumentDate
	lock := p.docLock(id)
	lock.Lock()
	res, err := p.Submit(t.Context(), in)
	lock.Unlock()
	if !errors.Is(err, ErrBusy) || res.Noop {
		t.Fatalf("locked duplicate = %+v, %v, want ErrBusy", res, err)
	}
	if after := readDateDocument(t, st, id); !reflect.DeepEqual(before, after) {
		t.Fatalf("document changed after busy: before=%+v, after=%+v", before, after)
	}
	if len(p.jobs) != 0 {
		t.Fatal("busy duplicate enqueued work")
	}
	expectDateNoop(t, p, in, id)
	if d := readDateDocument(t, st, id); d.RawDocumentDate != changedDocumentDate {
		t.Fatalf("date after retry = %q, want %q", d.RawDocumentDate, changedDocumentDate)
	}
}

func TestSubmitUndatedOrSameDateDuplicateSkipsLockAndSpool(t *testing.T) {
	p, st, in := documentDatePipeline(t)
	id := submitDateDocument(t, p, st, in)
	dir := filepath.Join(t.TempDir(), "unwritable")
	if err := os.WriteFile(dir, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	p.spool.dir = dir
	lock := p.docLock(id)
	lock.Lock()
	defer lock.Unlock()
	for _, date := range []string{"", initialDocumentDate} {
		in.DocumentDate = date
		expectDateNoop(t, p, in, id)
	}
	if d := readDateDocument(t, st, id); d.RawDocumentDate != initialDocumentDate {
		t.Fatalf("date after duplicates = %q, want %q", d.RawDocumentDate, initialDocumentDate)
	}
}

func TestSubmitDatedDuplicateMatchingFallbackBecomesCaller(t *testing.T) {
	p, st, in := documentDatePipeline(t)
	in.DocumentDate = ""
	id := submitDateDocument(t, p, st, in)
	setDateDocumentDone(t, st, id)
	version := getStoredVersion(t, st, id)
	in.DocumentDate = version
	expectDateNoop(t, p, in, id)
	d := readDateDocument(t, st, id)
	if d.RawDocumentDate != version || d.DocumentDateSource == nil || *d.DocumentDateSource != "caller" {
		t.Fatalf("date/source = %q, %v, want explicit %q from caller", d.RawDocumentDate, d.DocumentDateSource, version)
	}
}

func TestSubmitDatedDuplicateWithChangedRefsKeepsDate(t *testing.T) {
	p, st, in := documentDatePipeline(t)
	id := submitDateDocument(t, p, st, in)
	setDateDocumentDone(t, st, id)
	in.Products = []string{"unbound-date-test.invalid:1"}
	in.DocumentDate = changedDocumentDate
	reachedUpsert := false
	p.store = dateUpsertObserver{Store: st, beforeUpsert: func() { reachedUpsert = true }}
	_, err := p.Submit(t.Context(), in)
	var refsErr *graph.DocumentRefsError
	if !reachedUpsert || !errors.As(err, &refsErr) || refsErr.Code != "refs_changed_use_force" {
		t.Fatalf("dated duplicate with changed refs = %v (reached upsert: %t), want refs_changed_use_force from the transaction", err, reachedUpsert)
	}
	if d := readDateDocument(t, st, id); d.RawDocumentDate != initialDocumentDate {
		t.Fatalf("date after rejected refs = %q, want %q", d.RawDocumentDate, initialDocumentDate)
	}
}

func TestSubmitBackdatedVersionStillSupersedes(t *testing.T) {
	p, st, in := documentDatePipeline(t)
	first := submitDateDocument(t, p, st, in)
	if err := p.Process(t.Context(), first); err != nil {
		t.Fatal(err)
	}
	in.Text, in.DocumentDate = "The revised catalogue page.", changedDocumentDate
	next := submitDateDocument(t, p, st, in)
	if err := p.Process(t.Context(), next); err != nil {
		t.Fatal(err)
	}
	older, newer := readDateDocument(t, st, first), readDateDocument(t, st, next)
	if older.Status != graph.StatusSuperseded || newer.Status != graph.StatusDone ||
		newer.Supersedes != older.ID || newer.RawDocumentDate != changedDocumentDate ||
		getStoredVersion(t, st, next) <= getStoredVersion(t, st, first) {
		t.Fatalf("supersession with backdated content: old=%+v, new=%+v", older, newer)
	}
}
