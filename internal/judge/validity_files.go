package judge

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// ErrValidityInput denotes an invalid artifact or a failed precondition.
var ErrValidityInput = errors.New("invalid validity repair input")

func validityInputError(err error) error { return fmt.Errorf("%w: %w", ErrValidityInput, err) }

type validityFile struct {
	name string
	data []byte
}

func validityDigest(parts ...[]byte) string {
	h := sha256.New()
	for _, part := range parts {
		_, _ = h.Write(binary.BigEndian.AppendUint64(nil, uint64(len(part))))
		_, _ = h.Write(part)
	}
	return fmt.Sprintf("%x", h.Sum(nil))
}

func validityJSONLines[T any](rows []T) ([]byte, error) {
	var out bytes.Buffer
	enc := json.NewEncoder(&out)
	for _, row := range rows {
		if err := enc.Encode(row); err != nil {
			return nil, err
		}
	}
	return out.Bytes(), nil
}

func readValidityFile(path string) ([]byte, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if err := privateRejudgeFile(info); err != nil {
		return nil, err
	}
	return os.ReadFile(path)
}

// Serialize publication within a directory and check the entire output set
// before staging. Each rename is exclusive, even against other tools.
func writeValidityFiles(ctx context.Context, dir string, files []validityFile) (err error) {
	if err := context.Cause(ctx); err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0700); err != nil {
		return err
	}
	lock, err := lockRejudgeDir(dir)
	if err != nil {
		return err
	}
	defer lock.Close()
	for _, file := range files {
		path := filepath.Join(dir, file.name)
		if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
			if err == nil {
				return fmt.Errorf("%s: %w", path, os.ErrExist)
			}
			return err
		}
	}
	staged := make([]string, 0, len(files))
	published := 0
	defer func() {
		for _, path := range staged {
			_ = os.Remove(path)
		}
		if err != nil {
			for _, file := range files[:published] {
				err = errors.Join(err, os.Remove(filepath.Join(dir, file.name)))
			}
		}
	}()
	for _, file := range files {
		f, err := os.CreateTemp(dir, ".validity-*")
		if err != nil {
			return err
		}
		staged = append(staged, f.Name())
		_, err = f.Write(file.data)
		if err == nil {
			err = f.Sync()
		}
		if err := errors.Join(err, f.Close()); err != nil {
			return err
		}
	}
	if err := context.Cause(ctx); err != nil {
		return err
	}
	for i, file := range files {
		if err := renameRejudgeExclusive(staged[i], filepath.Join(dir, file.name)); err != nil {
			return err
		}
		published++
	}
	return lock.Sync()
}

type sealedValidityRun struct {
	verdicts map[string]rejudgeVerdict
	bytes    []byte
	ledger   []byte
	seal     string
}

func readSealedValidityRun(path, manifestDigest string, rows map[string]rejudgeManifestRow) (sealedValidityRun, error) {
	raw, err := readValidityFile(path)
	if err != nil {
		return sealedValidityRun{}, err
	}
	ledgerPath := filepath.Join(filepath.Dir(path), "attempts.json")
	ledger, ledgerBytes, err := readRejudgeAttempts(ledgerPath)
	if err != nil {
		return sealedValidityRun{}, err
	}
	if ledger == nil || ledger.Verdicts != filepath.Base(path) {
		return sealedValidityRun{}, fmt.Errorf("%s has no matching attempt ledger", path)
	}
	return validateSealedValidityRun(raw, ledgerBytes, ledger, manifestDigest, rows)
}

func validateSealedValidityRun(raw, ledgerBytes []byte, ledger *rejudgeAttempts, manifestDigest string, rows map[string]rejudgeManifestRow) (sealedValidityRun, error) {
	if !ledger.Completed || ledger.Manifest != manifestDigest ||
		(ledger.LastID != "" && rows[ledger.LastID].ID == "") {
		return sealedValidityRun{}, errors.New("attempt ledger is unsealed or belongs to a different manifest")
	}
	if len(raw) == 0 || raw[len(raw)-1] != '\n' {
		return sealedValidityRun{}, errors.New("sealed verdicts must be non-empty newline-terminated JSONL")
	}
	snapshot, err := decodeRejudgeVerdicts(bytes.NewReader(raw), rows)
	if err != nil {
		return sealedValidityRun{}, err
	}
	if snapshot.digest != ledger.VerdictsSHA256 || snapshot.debits > ledger.Attempts || len(snapshot.done) != len(rows) {
		return sealedValidityRun{}, errors.New("verdicts do not match the sealed attempt ledger or manifest population")
	}
	return sealedValidityRun{snapshot.done, raw, ledgerBytes, snapshot.digest}, nil
}
