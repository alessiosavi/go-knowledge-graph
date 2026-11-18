package judge

import (
	"bufio"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

// writeRejudgeAtomic syncs the replacement before publishing it, then syncs
// the directory so a debit survives a host crash.
func writeRejudgeAtomic(path string, replace bool, write func(io.Writer) error) error {
	dir := filepath.Dir(path)
	f, err := os.CreateTemp(dir, ".rejudge-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	defer f.Close()
	if err := write(f); err != nil {
		return err
	}
	if err := f.Sync(); err != nil {
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if replace {
		err = os.Rename(f.Name(), path)
	} else {
		err = renameRejudgeExclusive(f.Name(), path)
	}
	if err != nil {
		return err
	}
	return syncRejudgeDir(dir)
}

func syncRejudgeDir(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	return f.Sync()
}

func scanRejudgeLines(r io.Reader, visit func(int, []byte) error) error {
	scan := bufio.NewScanner(r)
	scan.Buffer(make([]byte, 16<<10), 16<<20)
	for line := 1; scan.Scan(); line++ {
		if err := visit(line, scan.Bytes()); err != nil {
			return fmt.Errorf("line %d: %w", line, err)
		}
	}
	return scan.Err()
}

func readRejudgeManifest(path string) (rejudgeManifest, string, error) {
	var m rejudgeManifest
	fi, err := os.Lstat(path)
	if err != nil {
		return m, "", err
	}
	if err := privateRejudgeFile(fi); err != nil {
		return m, "", fmt.Errorf("read manifest %s: %w", path, err)
	}
	f, err := os.Open(path)
	if err != nil {
		return m, "", err
	}
	defer f.Close()
	hash := sha256.New()
	err = scanRejudgeLines(io.TeeReader(f, hash), func(line int, raw []byte) error {
		if line == 1 {
			return json.Unmarshal(raw, &m.header)
		}
		var row rejudgeManifestRow
		if err := json.Unmarshal(raw, &row); err != nil {
			return err
		}
		m.rows = append(m.rows, row)
		return nil
	})
	if err == nil && m.header.Version != rejudgeManifestVersion {
		err = fmt.Errorf("invalid manifest version %d", m.header.Version)
	}
	if err == nil && len(m.rows) == 0 {
		err = errors.New("manifest has no rows")
	}
	if err != nil {
		return m, "", fmt.Errorf("read manifest %s: %w", path, err)
	}
	return m, fmt.Sprintf("%x", hash.Sum(nil)), nil
}

type rejudgeAttempts struct {
	Manifest       string `json:"manifest_sha256"`
	Verdicts       string `json:"verdicts"`
	Cap            int    `json:"cap"`
	Attempts       int    `json:"attempts"`
	LastID         string `json:"last_id,omitempty"`
	LastAttempts   int    `json:"last_attempts,omitempty"`
	VerdictsSHA256 string `json:"verdicts_sha256,omitempty"`
	Completed      bool   `json:"completed,omitempty"`
}

func (a rejudgeAttempts) save(path string, replace bool) error {
	return writeRejudgeAtomic(path, replace, func(w io.Writer) error {
		return json.NewEncoder(w).Encode(a)
	})
}

func readRejudgeAttempts(path string) (*rejudgeAttempts, []byte, error) {
	fi, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil, nil
	}
	if err != nil {
		return nil, nil, err
	}
	if err := privateRejudgeFile(fi); err != nil {
		return nil, nil, err
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, nil, err
	}
	var a rejudgeAttempts
	if err := json.Unmarshal(raw, &a); err != nil {
		return nil, nil, err
	}
	if a.Cap <= 0 || a.Attempts < 0 || a.Attempts > a.Cap || a.LastAttempts < 0 || a.LastAttempts > a.Attempts ||
		(a.LastID == "") != (a.LastAttempts == 0) || (a.Attempts == 0) != (a.LastAttempts == 0) {
		return nil, nil, fmt.Errorf("invalid attempts or cap in %s", path)
	}
	if a.Completed != (a.VerdictsSHA256 != "") {
		return nil, nil, fmt.Errorf("invalid verdicts seal in %s", path)
	}
	return &a, raw, nil
}

// rejudgeVerdictsDigest hashes the exact bytes of the open verdict log and
// rewinds it for validation on resume.
func rejudgeVerdictsDigest(f *os.File) (string, error) {
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return "", err
	}
	hash := sha256.New()
	if _, err := io.Copy(hash, f); err != nil {
		return "", err
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return "", err
	}
	return fmt.Sprintf("%x", hash.Sum(nil)), nil
}

func privateRejudgeFile(fi os.FileInfo) error {
	if !fi.Mode().IsRegular() || fi.Mode().Perm() != 0600 {
		return fmt.Errorf("%s must be a regular 0600 file", fi.Name())
	}
	return nil
}

func openRejudgeVerdicts(path string) (*os.File, error) {
	fi, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if err := privateRejudgeFile(fi); err != nil {
		return nil, err
	}
	return os.OpenFile(path, os.O_RDWR|os.O_APPEND, 0)
}

func createRejudgeVerdicts(path string) (*os.File, error) {
	f, err := os.OpenFile(path, os.O_RDWR|os.O_APPEND|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return nil, err
	}
	if err := syncRejudgeDir(filepath.Dir(path)); err != nil {
		f.Close()
		return nil, err
	}
	return f, nil
}

// The directory has a stable inode while attempts.json is atomically replaced.
func lockRejudgeDir(path string) (*os.File, error) {
	dir, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	if err := flockRejudgeDir(dir); err != nil {
		dir.Close()
		return nil, fmt.Errorf("lock rejudge output directory %s: %w", path, err)
	}
	return dir, nil
}
