package judge

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"time"

	"github.com/alessiosavi/go-knowledge-graph/internal/graph"
	"github.com/alessiosavi/go-knowledge-graph/internal/ingest"
)

const rejudgeLeaseName = graph.RejudgeLeaseName

var errRejudgeBudget = errors.New("judge: rejudge attempt budget reached")

type RejudgeOptions struct {
	Manifest, Out  string
	BudgetAttempts int
	RaiseCap       bool
}

type RejudgeResult struct {
	Planned, EffectiveBudget                  int
	Judged, Resumed, CacheHits, Attempts, Cap int
	Fingerprints                              RejudgeFingerprints
	StoppedForBudget                          bool
	Sealed                                    bool // Returned from an already sealed ledger.
	Gate                                      *RejudgeGate
}

type rejudgeVerdict struct {
	ID          string  `json:"id"`
	Key         string  `json:"key"`
	Fingerprint string  `json:"fingerprint"`
	Verdict     string  `json:"verdict"`
	Confidence  float64 `json:"confidence"`
	Reason      string  `json:"reason"`
	Attempts    int     `json:"attempts"`
	Label       string  `json:"label,omitempty"`
}

type strictRejudgeStore struct{ *graph.Store }

type rejudgeLeases interface {
	AcquireRejudgeLease(context.Context, string, time.Duration) (bool, error)
	RenewLease(context.Context, string, string, time.Duration) (bool, error)
	ReleaseLease(context.Context, string, string) error
}

func (s strictRejudgeStore) PutJudgeVerdict(ctx context.Context, v graph.JudgeVerdict) error {
	return s.PutJudgeVerdictStrict(ctx, v)
}

// JudgeRejudge verifies frozen requests through the strict cache. It reuses
// completed local verdicts before consulting the graph and persists every
// paid attempt debit before the provider can be called.
func JudgeRejudge(ctx context.Context, st *graph.Store, client *Client, presetsDir string, opt RejudgeOptions) (RejudgeResult, error) {
	return judgeRejudge(ctx, st, strictRejudgeStore{st}, client, presetsDir, opt)
}

func judgeRejudge(ctx context.Context, leases rejudgeLeases, store VerdictStore, client *Client, presetsDir string, opt RejudgeOptions) (result RejudgeResult, err error) {
	if client.tokens != nil {
		return result, ErrV3GCPAuth
	}
	if opt.BudgetAttempts <= 0 {
		return result, fmt.Errorf("judge: budget-attempts must be positive")
	}
	result.EffectiveBudget = opt.BudgetAttempts
	manifest, digest, err := readRejudgeManifest(opt.Manifest)
	if err != nil {
		return result, err
	}
	rows, payloads, definitions, err := validateRejudgeManifest(manifest, client.Model(), presetsDir)
	if err != nil {
		return result, err
	}
	result.Planned = len(manifest.rows)
	result.Fingerprints = make(RejudgeFingerprints, len(definitions))
	for _, row := range manifest.rows {
		result.Fingerprints[row.Preset] = row.Request.Profile.Fingerprint
	}
	gate := buildRejudgeGate(manifest, payloads, nil, result.Fingerprints)
	if gate != nil && 100*gate.rows.reconciled < 95*gate.rows.expected {
		result.Gate = gate
		return result, nil
	}

	dir := filepath.Dir(opt.Out)
	outName := filepath.Base(opt.Out)
	if outName == "attempts.json" {
		return result, fmt.Errorf("judge: verdict output cannot be named attempts.json")
	}
	lock, err := lockRejudgeDir(dir)
	if err != nil {
		return result, err
	}
	defer lock.Close()

	attemptsPath := filepath.Join(dir, "attempts.json")
	ledger, attemptsSnapshot, err := readRejudgeAttempts(attemptsPath)
	if err != nil {
		return result, fmt.Errorf("read attempt ledger: %w", err)
	}
	if ledger != nil && (ledger.Manifest != digest || ledger.Verdicts != outName ||
		ledger.LastID != "" && rows[ledger.LastID].ID == "") {
		return result, fmt.Errorf("judge: attempts.json belongs to a different manifest or verdict file")
	}
	out, err := openRejudgeVerdicts(opt.Out)
	if err != nil && !(ledger == nil && errors.Is(err, os.ErrNotExist)) {
		return result, fmt.Errorf("open verdict ledger: %w", err)
	}
	var sealDigest string
	defer func(runCtx context.Context) {
		// The lease cleanup runs first. Publish only after the file closes
		// and both the run and its cleanup have succeeded.
		if out != nil {
			err = errors.Join(err, out.Close())
		}
		if err != nil || sealDigest == "" {
			return
		}
		if err = context.Cause(runCtx); err != nil {
			return
		}
		ledger.Completed, ledger.VerdictsSHA256 = true, sealDigest
		if err = ledger.save(attemptsPath, true); err != nil {
			err = fmt.Errorf("seal attempt ledger: %w", err)
		}
	}(ctx)
	if ledger != nil && ledger.Completed {
		verdictsDigest, err := rejudgeVerdictsDigest(out)
		if err != nil {
			return result, fmt.Errorf("read verdicts digest: %w", err)
		}
		if verdictsDigest != ledger.VerdictsSHA256 {
			return result, fmt.Errorf("judge: verdicts digest does not match the sealed attempt ledger")
		}
	}
	verdicts, err := readRejudgeVerdicts(out, rows)
	if err != nil {
		return result, fmt.Errorf("read verdict ledger: %w", err)
	}
	done, known := verdicts.done, verdicts.known
	result.Resumed = len(done)
	if (ledger == nil && len(done) > 0) || (ledger != nil && verdicts.debits > ledger.Attempts) {
		return result, fmt.Errorf("judge: verdict ledger has no matching attempt debits")
	}
	newLedger := ledger == nil
	if newLedger {
		ledger = &rejudgeAttempts{Manifest: digest, Verdicts: outName, Cap: opt.BudgetAttempts}
	} else {
		result.Attempts, result.Cap = ledger.Attempts, ledger.Cap
		if ledger.Completed {
			if len(done) != result.Planned {
				return result, fmt.Errorf("judge: sealed verdict ledger is incomplete")
			}
			result.Gate = buildRejudgeGate(manifest, payloads, done, result.Fingerprints)
			result.Sealed = true
			return result, nil
		}
	}
	raise := opt.BudgetAttempts > ledger.Cap
	if raise && !opt.RaiseCap {
		return result, fmt.Errorf("judge: stored cap is %d; use -raise-cap to raise it to %d", ledger.Cap, opt.BudgetAttempts)
	}
	owner := rand.Text()
	ok, err := leases.AcquireRejudgeLease(ctx, owner, backfillLeaseTTL)
	if err != nil {
		return result, err
	}
	if !ok {
		return result, fmt.Errorf("judge: rejudge lease is held by another owner")
	}
	ctx, cancel := context.WithCancelCause(ctx)
	renew := func() error {
		ok, err := leases.RenewLease(ctx, rejudgeLeaseName, owner, backfillLeaseTTL)
		if err == nil && !ok {
			err = fmt.Errorf("judge: rejudge lease lost")
		}
		return err
	}
	stopped := make(chan struct{})
	go func() {
		defer close(stopped)
		ticker := time.NewTicker(backfillRenewEvery)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				if err := renew(); err != nil {
					cancel(err)
					return
				}
			}
		}
	}()
	defer func() {
		cancel(nil)
		<-stopped
		cleanup, stop := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer stop()
		err = errors.Join(err, leases.ReleaseLease(cleanup, rejudgeLeaseName, owner))
	}()
	// Another host may finish writing between our snapshot and lease acquisition.
	_, currentAttempts, err := readRejudgeAttempts(attemptsPath)
	if err != nil {
		return result, fmt.Errorf("recheck attempts.json after acquiring the rejudge lease: %w", err)
	}
	if !bytes.Equal(attemptsSnapshot, currentAttempts) {
		return result, errors.New("attempts.json changed before acquiring the rejudge lease")
	}
	if err := verdicts.recheck(opt.Out, out); err != nil {
		return result, fmt.Errorf("%s changed before acquiring the rejudge lease: %w", outName, err)
	}
	if err := context.Cause(ctx); err != nil {
		return result, err
	}
	if out == nil {
		out, err = createRejudgeVerdicts(opt.Out)
		if err != nil {
			return result, fmt.Errorf("create verdict ledger: %w", err)
		}
	}
	if raise {
		ledger.Cap = opt.BudgetAttempts
	}
	if newLedger || raise {
		if err := ledger.save(attemptsPath, !newLedger); err != nil {
			return result, fmt.Errorf("write attempt ledger: %w", err)
		}
	}
	result.Attempts, result.Cap = ledger.Attempts, ledger.Cap
	cached := NewCached(client, store, client.Model())
	oldGate := client.attemptGate
	var activeID string
	client.SetAttemptGate(func() error {
		if err := context.Cause(ctx); err != nil {
			return err
		}
		if ledger.Attempts >= result.EffectiveBudget {
			return errRejudgeBudget
		}
		if oldGate != nil {
			if err := oldGate(); err != nil {
				return err
			}
		}
		if err := renew(); err != nil {
			return err
		}
		// Only the most recently attempted row can have unrecorded attempts:
		// the loop stops on every provider/cache error or budget stop.
		if ledger.LastID != activeID {
			ledger.LastID, ledger.LastAttempts = activeID, 0
		}
		ledger.LastAttempts++
		ledger.Attempts++
		if err := ledger.save(attemptsPath, true); err != nil {
			return fmt.Errorf("debit attempt ledger: %w", err)
		}
		result.Attempts = ledger.Attempts
		return nil
	})
	defer client.SetAttemptGate(oldGate)

	enc := json.NewEncoder(out)
	for _, row := range manifest.rows {
		if _, ok := done[row.ID]; ok {
			continue
		}
		if err := context.Cause(ctx); err != nil {
			return result, err
		}
		key := RequestKeyV2(row.Request.RequestBytes, row.Request.Profile.Fingerprint)
		previous, reused := known[key]
		activeID = row.ID
		before := ledger.Attempts
		verdict := ingest.Verdict{Status: previous.Verdict, Confidence: previous.Confidence, Reason: previous.Reason}
		var verifyErr error
		if !reused {
			verdict, verifyErr = cached.VerifyRequest(ctx, row.Request, definitions.forPreset(row.Preset))
		}
		if errors.Is(verifyErr, errRejudgeBudget) {
			result.StoppedForBudget = true
			break
		}
		if validRejudgeVerdict(verdict.Status, verdict.Confidence) {
			attempts := 0
			if ledger.LastID == row.ID {
				attempts = ledger.LastAttempts
			}
			record := rejudgeVerdict{
				ID: row.ID, Key: key, Fingerprint: row.Request.Profile.Fingerprint, Verdict: verdict.Status,
				Confidence: verdict.Confidence, Reason: verdict.Reason, Attempts: attempts, Label: row.Label,
			}
			if err := context.Cause(ctx); err != nil {
				return result, err
			}
			if err := enc.Encode(record); err != nil {
				return result, fmt.Errorf("append verdict for %s: %w", row.ID, errors.Join(err, verifyErr))
			}
			if err := out.Sync(); err != nil {
				return result, fmt.Errorf("sync verdict for %s: %w", row.ID, errors.Join(err, verifyErr))
			}
			done[row.ID], known[key] = record, record
			result.Judged++
			if ledger.Attempts == before {
				result.CacheHits++
			}
		} else if verifyErr == nil {
			verifyErr = fmt.Errorf("invalid cached verdict status or confidence")
		}
		if verifyErr != nil {
			return result, fmt.Errorf("judge %s: %w", row.ID, verifyErr)
		}
	}
	result.Gate = buildRejudgeGate(manifest, payloads, done, result.Fingerprints)
	if len(done) == result.Planned && context.Cause(ctx) == nil {
		sealDigest, err = rejudgeVerdictsDigest(out)
		if err != nil {
			return result, fmt.Errorf("hash verdicts for seal: %w", err)
		}
	}
	return result, context.Cause(ctx)
}

func validateRejudgeManifest(m rejudgeManifest, model, presetDir string) (map[string]rejudgeManifestRow, map[string]v2Payload, rejudgeDefinitions, error) {
	rows := make(map[string]rejudgeManifestRow, len(m.rows))
	payloads := make(map[string]v2Payload, len(m.rows))
	definitions := make(rejudgeDefinitions)
	for _, row := range m.rows {
		if row.ID == "" || row.ID != graph.FactAuditID(row.HeadKey, row.Relation, row.TailKey, row.ChunkID) ||
			row.Request.DocID == "" || !validRejudgeLabel(row.Label) {
			return nil, nil, nil, fmt.Errorf("invalid manifest row %q", row.ID)
		}
		if _, duplicate := rows[row.ID]; duplicate {
			return nil, nil, nil, fmt.Errorf("duplicate manifest id %s", row.ID)
		}
		rows[row.ID] = row
		if _, ok := definitions[row.Preset]; !ok {
			defs, err := loadRejudgeDefinitions(presetDir, row.Preset)
			if err != nil {
				return nil, nil, nil, err
			}
			definitions[row.Preset] = defs
		}
		if _, err := row.Request.validate(model, definitions.forPreset(row.Preset)); err != nil {
			return nil, nil, nil, fmt.Errorf("manifest id %s: %w", row.ID, err)
		}
		var payload v2Payload
		if err := json.Unmarshal(row.Request.RequestBytes, &payload); err != nil {
			return nil, nil, nil, fmt.Errorf("manifest id %s: %w", row.ID, err)
		}
		if payload.Fact.Head == "" || payload.Fact.Tail == "" || payload.Evidence == "" || payload.Fact.Rel != row.Relation ||
			payload.RelationDefinition != definitions[row.Preset][row.Relation] {
			return nil, nil, nil, fmt.Errorf("manifest id %s: invalid frozen request", row.ID)
		}
		payloads[row.ID] = payload
	}
	for _, mismatch := range m.header.Mismatches {
		if _, duplicate := rows[mismatch.ID]; duplicate || mismatch.ID == "" || mismatch.Reason == "" {
			return nil, nil, nil, fmt.Errorf("invalid manifest mismatch id %q", mismatch.ID)
		}
		rows[mismatch.ID] = rejudgeManifestRow{}
	}
	return rows, payloads, definitions, nil
}

func validRejudgeLabel(label string) bool {
	return label == "" || label == "skip" || scoredRejudgeLabel(label)
}

func validRejudgeVerdict(verdict string, confidence float64) bool {
	return (verdict == "confirmed" || verdict == "refuted" || verdict == "unclear") &&
		!math.IsNaN(confidence) && confidence >= 0 && confidence <= 1
}

type rejudgeVerdictSnapshot struct {
	done, known map[string]rejudgeVerdict
	debits      int
	info        os.FileInfo
	digest      string
}

func (s rejudgeVerdictSnapshot) recheck(path string, f *os.File) error {
	info, err := os.Lstat(path)
	if s.info == nil && errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if err := privateRejudgeFile(info); err != nil {
		return err
	}
	if s.info == nil || !os.SameFile(s.info, info) {
		return errors.New("verdict file appeared or was replaced")
	}
	if s.info.Size() != info.Size() {
		return errors.New("verdict file size changed")
	}
	digest, err := rejudgeVerdictsDigest(f)
	if err != nil {
		return err
	}
	if digest != s.digest {
		return errors.New("verdict file digest changed")
	}
	return nil
}

func readRejudgeVerdicts(f *os.File, rows map[string]rejudgeManifestRow) (rejudgeVerdictSnapshot, error) {
	snapshot := rejudgeVerdictSnapshot{done: make(map[string]rejudgeVerdict), known: make(map[string]rejudgeVerdict)}
	if f == nil {
		return snapshot, nil
	}
	info, err := f.Stat()
	if err != nil {
		return snapshot, err
	}
	// Fail closed on a torn append, even when the final JSON itself parses.
	if info.Size() > 0 {
		var end [1]byte
		if _, err := f.ReadAt(end[:], info.Size()-1); err != nil {
			return snapshot, err
		}
		if end[0] != '\n' {
			return snapshot, io.ErrUnexpectedEOF
		}
	}
	snapshot, err = decodeRejudgeVerdicts(f, rows)
	snapshot.info = info
	return snapshot, err
}

func decodeRejudgeVerdicts(r io.Reader, rows map[string]rejudgeManifestRow) (rejudgeVerdictSnapshot, error) {
	snapshot := rejudgeVerdictSnapshot{done: make(map[string]rejudgeVerdict), known: make(map[string]rejudgeVerdict)}
	hash := sha256.New()
	err := scanRejudgeLines(io.TeeReader(r, hash), func(_ int, raw []byte) error {
		var v rejudgeVerdict
		if err := json.Unmarshal(raw, &v); err != nil {
			return err
		}
		row := rows[v.ID]
		if row.ID == "" || v.Fingerprint != row.Request.Profile.Fingerprint || v.Label != row.Label {
			return fmt.Errorf("verdict for %q does not match the manifest", v.ID)
		}
		if _, duplicate := snapshot.done[v.ID]; duplicate {
			return fmt.Errorf("duplicate verdict for %s", v.ID)
		}
		if !validRejudgeVerdict(v.Verdict, v.Confidence) || v.Attempts < 0 || v.Attempts > math.MaxInt-snapshot.debits {
			return fmt.Errorf("invalid verdict for %s", v.ID)
		}
		key := RequestKeyV2(row.Request.RequestBytes, row.Request.Profile.Fingerprint)
		if v.Key != key {
			return fmt.Errorf("verdict for %q has a missing or mismatched request key", v.ID)
		}
		if prev, ok := snapshot.known[key]; ok &&
			(prev.Verdict != v.Verdict || prev.Confidence != v.Confidence || prev.Reason != v.Reason) {
			return fmt.Errorf("conflicting verdicts for identical requests (%s and %s)", prev.ID, v.ID)
		}
		snapshot.debits += v.Attempts
		snapshot.done[v.ID], snapshot.known[key] = v, v
		return nil
	})
	snapshot.digest = fmt.Sprintf("%x", hash.Sum(nil))
	return snapshot, err
}
