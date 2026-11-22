package ingest

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
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
	link     func(oldname, newname string) error
}

func spoolName(id, hash string) string { return id + "-" + hash + ".json" }

// spoolWrite retains the previous same-ID payload until graph publication.
type spoolWrite struct {
	path   string
	digest string
	backup bool
}

const spoolBackupSuffix = ".prev"

func (w spoolWrite) accept() error {
	if !w.backup {
		return nil
	}
	return os.Remove(w.path + spoolBackupSuffix)
}

func (w spoolWrite) restore() error {
	var err error
	if w.backup {
		err = os.Rename(w.path+spoolBackupSuffix, w.path)
	} else {
		err = os.Remove(w.path)
	}
	if err != nil {
		return err
	}
	return syncSpoolDir(filepath.Dir(w.path))
}

func syncSpoolDir(path string) error {
	dir, err := os.Open(path)
	if err != nil {
		return err
	}
	defer dir.Close()
	return dir.Sync()
}

// stageSpoolFile writes and syncs a 0600 temp file before its caller publishes it.
func stageSpoolFile(dir string, src io.Reader) (_ string, err error) {
	f, err := os.CreateTemp(dir, ".spool-*")
	if err != nil {
		return "", err
	}
	defer func() {
		if err != nil {
			f.Close()
			os.Remove(f.Name())
		}
	}()
	if _, err := io.Copy(f, src); err != nil {
		return "", err
	}
	if err := f.Sync(); err != nil {
		return "", err
	}
	if err := f.Close(); err != nil {
		return "", err
	}
	return f.Name(), nil
}

func copySpoolBackup(path, backup string) (bool, error) {
	src, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	defer src.Close()
	staged, err := stageSpoolFile(filepath.Dir(backup), src)
	if err != nil {
		return false, err
	}
	defer os.Remove(staged)
	if err := os.Rename(staged, backup); err != nil {
		return false, err
	}
	return true, nil
}

// resolveSpoolBackup reconciles a crash or an uncertain graph commit.
// The graph digest identifies the published payload without changing the
// payload format. A legacy document with no digest uses the backup.
func resolveSpoolBackup(path, digest string) error {
	backup := path + spoolBackupSuffix
	if _, err := os.Stat(backup); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	}
	if digest != "" {
		data, err := os.ReadFile(path)
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		if err == nil && sha256Hex(string(data)) == digest {
			return os.Remove(backup)
		}
	}
	return os.Rename(backup, path)
}

// putLocked commits the converted payload before the graph can advertise a
// recoverable document. The caller holds mu until graph publication completes.
func (s *submissionSpool) putLocked(id string, sub submission, priorDigest string) (spoolWrite, error) {
	name := spoolName(id, sub.loaded.Hash)
	if !spoolNameRE.MatchString(name) {
		return spoolWrite{}, errors.New("invalid spool identity")
	}
	data, err := json.Marshal(spoolEntry{Loaded: sub.loaded, Labels: sub.labels})
	if err != nil {
		return spoolWrite{}, err
	}
	if err := os.MkdirAll(s.dir, 0o700); err != nil {
		return spoolWrite{}, err
	}
	path := filepath.Join(s.dir, name)
	if err := resolveSpoolBackup(path, priorDigest); err != nil {
		return spoolWrite{}, err
	}
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		return spoolWrite{}, err
	}
	count := 1
	totalBytes := int64(len(data))
	for _, e := range entries {
		if e.Name() == name || !spoolNameRE.MatchString(strings.TrimSuffix(e.Name(), spoolBackupSuffix)) {
			continue
		}
		info, err := e.Info()
		if err != nil {
			return spoolWrite{}, err
		}
		count++
		totalBytes += info.Size()
	}
	if count > s.maxCount || totalBytes > s.maxBytes {
		return spoolWrite{}, ErrSpoolFull
	}
	staged, err := stageSpoolFile(s.dir, bytes.NewReader(data))
	if err != nil {
		return spoolWrite{}, err
	}
	defer os.Remove(staged)

	write := spoolWrite{path: path, digest: sha256Hex(string(data))}
	backup := path + spoolBackupSuffix
	link := s.link
	if link == nil {
		link = os.Link
	}
	if linkErr := link(path, backup); linkErr == nil {
		write.backup = true
	} else if !errors.Is(linkErr, os.ErrNotExist) {
		write.backup, err = copySpoolBackup(path, backup)
		if err != nil {
			return spoolWrite{}, errors.Join(linkErr, err)
		}
	}
	if write.backup {
		if err := syncSpoolDir(s.dir); err != nil {
			return spoolWrite{}, errors.Join(err, write.accept())
		}
	}
	if err := os.Rename(staged, path); err != nil {
		return spoolWrite{}, errors.Join(err, write.accept())
	}
	// Sync the directory so the new file and its backup survive host crash.
	if err := syncSpoolDir(s.dir); err != nil {
		return spoolWrite{}, errors.Join(err, write.restore())
	}
	return write, nil
}

func (s *submissionSpool) load(doc *graph.Document) (submission, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	name := spoolName(doc.ID, doc.Hash)
	if !spoolNameRE.MatchString(name) {
		return submission{}, errors.New("invalid spool identity")
	}
	path := filepath.Join(s.dir, name)
	if err := resolveSpoolBackup(path, doc.SpoolPayloadDigest); err != nil {
		return submission{}, err
	}
	data, err := os.ReadFile(path)
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

// sweep keeps payloads needed by received and fetched documents, including
// documents that recovery has yet to enqueue. Query under mu so a concurrent
// Submit cannot publish between the snapshot and removal. Workers may advance
// after the snapshot; their own cleanup removes those entries.
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
		name := strings.TrimSuffix(e.Name(), spoolBackupSuffix)
		owned := spoolNameRE.MatchString(name) || (temps && strings.HasPrefix(e.Name(), ".spool-"))
		if !e.IsDir() && owned && !keep(name) {
			if err := os.Remove(filepath.Join(s.dir, e.Name())); err != nil && !errors.Is(err, os.ErrNotExist) {
				return err
			}
		}
	}
	return nil
}
