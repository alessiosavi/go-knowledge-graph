package ingest

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"

	"github.com/alessiosavi/go-knowledge-graph/internal/graph"
)

// ErrSpool and ErrSpoolFull distinguish infrastructure failures from input
// errors; a full spool is temporary backpressure (HTTP 503).
var (
	ErrSpool     = errors.New("ingest: spool failure")
	ErrSpoolFull = errors.New("ingest: spool capacity reached; retry later")
)

// Only pipeline-owned files are counted or swept. A document id already
// includes the content hash; keeping both in the filename makes recovery
// explicitly select the submitted version.
var spoolNameRE = regexp.MustCompile(`^[a-f0-9]{32}-[a-f0-9]{64}\.json$`)

type spoolEntry struct {
	Loaded Loaded `json:"loaded"`
	Labels string `json:"labels"`
}

type submissionSpool struct {
	mu       sync.Mutex // admission, graph publication and sweeping share this lock
	dir      string
	maxCount int
	maxBytes int64
}

func spoolName(id, hash string) string { return id + "-" + hash + ".json" }

// putLocked commits the converted payload before the graph can advertise a
// recoverable document. The caller holds mu until graph publication completes.
func (s *submissionSpool) putLocked(id string, sub submission) error {
	name := spoolName(id, sub.loaded.Hash)
	if !spoolNameRE.MatchString(name) {
		return errors.New("invalid spool identity")
	}
	data, err := json.Marshal(spoolEntry{Loaded: sub.loaded, Labels: sub.labels})
	if err != nil {
		return err
	}
	if err := os.MkdirAll(s.dir, 0o700); err != nil {
		return err
	}
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		return err
	}
	count := 1
	bytes := int64(len(data))
	for _, e := range entries {
		if e.Name() == name || !spoolNameRE.MatchString(e.Name()) {
			continue
		}
		info, err := e.Info()
		if err != nil {
			return err
		}
		count++
		bytes += info.Size()
	}
	if count > s.maxCount || bytes > s.maxBytes {
		return ErrSpoolFull
	}
	f, err := os.CreateTemp(s.dir, ".spool-*") // CreateTemp uses 0600
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	defer f.Close()
	if _, err := f.Write(data); err != nil {
		return err
	}
	if err := f.Sync(); err != nil {
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := os.Rename(f.Name(), filepath.Join(s.dir, name)); err != nil {
		return err
	}
	// Sync the directory too: the rename must survive a host crash, not just
	// a process restart. A failure is surfaced before acceptance.
	dir, err := os.Open(s.dir)
	if err != nil {
		return err
	}
	defer dir.Close()
	return dir.Sync()
}

func (s *submissionSpool) load(doc *graph.Document) (submission, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	name := spoolName(doc.ID, doc.Hash)
	if !spoolNameRE.MatchString(name) {
		return submission{}, errors.New("invalid spool identity")
	}
	data, err := os.ReadFile(filepath.Join(s.dir, name))
	if err != nil {
		return submission{}, err
	}
	var entry spoolEntry
	if err := json.Unmarshal(data, &entry); err != nil {
		return submission{}, fmt.Errorf("decode spool: %w", err)
	}
	// Process checks the text hash against the graph, just as for URL reloads.
	return submission{loaded: entry.Loaded, labels: entry.Labels}, nil
}

func (s *submissionSpool) remove(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.removeLocked(id)
}

func (s *submissionSpool) removeLocked(id string) error {
	return s.sweepLocked(func(name string) bool {
		return !strings.HasPrefix(name, id+"-")
	}, false)
}

// sweep runs after recovery has queued the graph's in-flight documents.
// Query under mu so a concurrent Submit cannot publish between the snapshot
// and removal. Workers may advance after the snapshot; their own cleanup
// removes those entries, so keeping them temporarily is safe.
func (s *submissionSpool) sweep(ctx context.Context, st pipelineStore) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	docs, err := st.InFlight(ctx)
	if err != nil {
		return err
	}
	keep := make(map[string]bool)
	for _, doc := range docs {
		if doc.Status == graph.StatusReceived || doc.Status == graph.StatusFetched {
			keep[spoolName(doc.ID, doc.Hash)] = true
		}
	}
	return s.sweepLocked(func(name string) bool { return keep[name] }, true)
}

func (s *submissionSpool) sweepLocked(keep func(string) bool, temps bool) error {
	entries, err := os.ReadDir(s.dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	for _, e := range entries {
		owned := spoolNameRE.MatchString(e.Name()) || (temps && strings.HasPrefix(e.Name(), ".spool-"))
		if !e.IsDir() && owned && !keep(e.Name()) {
			if err := os.Remove(filepath.Join(s.dir, e.Name())); err != nil && !errors.Is(err, os.ErrNotExist) {
				return err
			}
		}
	}
	return nil
}
