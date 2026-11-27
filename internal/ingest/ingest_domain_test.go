package ingest

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/alessiosavi/go-knowledge-graph/internal/domain"
	"github.com/alessiosavi/go-knowledge-graph/internal/graph"
)

type domainAdmissionStore struct {
	*spoolStore
	resolved             []string
	lookupErr, upsertErr error
}

func (s *domainAdmissionStore) ResolveIngestDomain(_ context.Context, choice string) (string, error) {
	s.resolved = append(s.resolved, choice)
	if s.lookupErr != nil {
		return "", s.lookupErr
	}
	if choice == "Archive / Notes" {
		return "archive", nil
	}
	return choice, nil
}

func (s *domainAdmissionStore) CheckIngestDomain(_ context.Context, doc *graph.Document, _ string) error {
	code := "domain_conflict"
	if doc.Domain == "" {
		code = "document_busy"
	}
	return &graph.IngestDomainError{Code: code, Conflict: true}
}

func (s *domainAdmissionStore) UpsertDocument(ctx context.Context, in graph.DocumentInput) (graph.UpsertResult, error) {
	if s.upsertErr != nil {
		return graph.UpsertResult{}, s.upsertErr
	}
	return s.spoolStore.UpsertDocument(ctx, in)
}

func (*domainAdmissionStore) ClearDuplicateOf(context.Context, []string) error { return nil }
func (*domainAdmissionStore) UpdateNameProfile(context.Context, string, domain.Profile) error {
	return nil
}
func (*domainAdmissionStore) MentionNamesForChunks(context.Context, []string) (map[string][]string, error) {
	return nil, nil
}

func TestChosenDomainAdmissionRecovery(t *testing.T) {
	p, base := spoolPipeline(t)
	st := &domainAdmissionStore{spoolStore: base}
	p.store = st
	p.SetWriteMode(WriteMode{Domains: true})
	var writes int
	base.beforeUpsert = func(in graph.DocumentInput) {
		writes++
		if !in.DomainChosen || in.Domain != "archive" {
			t.Errorf("admission got %+v", in)
		}
	}
	in := Input{Source: "notes", Text: "Ada wrote a note.", Domain: " Archive / Notes "}
	res := submitSpool(t, p, in)
	if got := base.docs[res.DocumentID]; got.Domain != "archive" || got.Status != graph.StatusReceived {
		t.Fatalf("document at admission = %+v", got)
	}
	dup := submitSpool(t, p, in)
	if !dup.Noop || dup.DocumentID != res.DocumentID || writes != 1 || st.resolved[0] != "Archive / Notes" {
		t.Fatalf("duplicate = %+v, writes = %d, lookups = %v", dup, writes, st.resolved)
	}
	restarted := NewPipeline(p.cfg, nil, &spoolProvider{}, nil, nil)
	restarted.store = st
	restarted.SetWriteMode(p.WriteMode())
	recoverCtx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	if err := restarted.Recover(recoverCtx); err != nil {
		t.Fatal(err)
	}
	select {
	case id := <-restarted.jobs:
		if id != res.DocumentID {
			t.Fatalf("recovered %q, want %q", id, res.DocumentID)
		}
	case <-recoverCtx.Done():
		t.Fatalf("waiting for the admitted document: %v", recoverCtx.Err())
	}
	if err := restarted.Process(t.Context(), res.DocumentID); err != nil {
		t.Fatal(err)
	}
	if got := base.docs[res.DocumentID]; got.Domain != "archive" || got.Status != graph.StatusDone {
		t.Fatalf("document after restart = %+v", got)
	}
}

func TestChosenDomainFailsBeforeLoading(t *testing.T) {
	for _, tc := range []struct {
		name      string
		mode      WriteMode
		lookupErr error
		want      error
	}{
		{name: "legacy", want: graph.ErrSchemaNotV4},
		{name: "lookup unavailable", mode: WriteMode{Domains: true}, lookupErr: errors.New("store offline"), want: ErrDomainLookup},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p, base := spoolPipeline(t)
			p.store = &domainAdmissionStore{spoolStore: base, lookupErr: tc.lookupErr}
			p.SetWriteMode(tc.mode)
			p.load = func(context.Context, Input) (Loaded, error) {
				t.Fatal("loader called after domain validation failed")
				return Loaded{}, nil
			}
			_, err := p.Submit(t.Context(), Input{Kind: "url", URL: "https://example.test/", Domain: "archive"})
			if !errors.Is(err, tc.want) || (tc.lookupErr != nil && (!errors.Is(err, tc.lookupErr) || !errors.Is(err, ErrStore))) {
				t.Fatalf("Submit: %v, want %v", err, tc.want)
			}
		})
	}
}

func TestChosenDomainAutoAndDefiniteRejection(t *testing.T) {
	p, base := spoolPipeline(t)
	st := &domainAdmissionStore{spoolStore: base}
	p.store = st
	p.SetWriteMode(WriteMode{Domains: true})
	in := Input{Text: "Some text.", Title: "Old"}
	original := submitSpool(t, p, in)
	id := original.DocumentID
	in.Domain = " \t "
	if dup := submitSpool(t, p, in); !dup.Noop || dup.DocumentID != id || len(st.resolved) != 0 {
		t.Fatalf("Auto duplicate = %+v, lookups = %v", dup, st.resolved)
	}
	path := filepath.Join(p.cfg.Ingest.SpoolDir, spoolName(id, base.docs[id].Hash))
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct{ existing, code string }{{"", "document_busy"}, {"other", "domain_conflict"}} {
		d := base.docs[id]
		d.Domain = tc.existing
		base.docs[id] = d
		for _, force := range []bool{false, true} {
			_, err := p.Submit(t.Context(), Input{Text: in.Text, Domain: "archive", Force: force})
			var rejection *graph.IngestDomainError
			if !errors.As(err, &rejection) || !rejection.Conflict || rejection.Code != tc.code {
				t.Fatalf("existing %q, force %t: %v, want %s", tc.existing, force, err, tc.code)
			}
		}
	}
	d := base.docs[id]
	d.Domain = "archive"
	base.docs[id] = d
	for _, rejection := range []error{
		&graph.IngestDomainError{Code: "unknown_domain", Conflict: true},
		graph.ErrSchemaNotV4,
	} {
		st.upsertErr = rejection
		_, err = p.Submit(t.Context(), Input{Text: in.Text, Domain: "archive", Title: "Rejected", Force: true})
		if !errors.Is(err, rejection) {
			t.Fatalf("upsert rejection = %v, want %v", err, rejection)
		}
		after, err := os.ReadFile(path)
		if err != nil || !bytes.Equal(after, before) {
			t.Errorf("restored payload after %v = %s, %v, want %s", rejection, after, err, before)
		}
		if _, err := os.Stat(path + ".prev"); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("spool backup after %v: %v", rejection, err)
		}
	}
}
