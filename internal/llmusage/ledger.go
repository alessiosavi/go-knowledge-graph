// Package llmusage accounts for provider calls made by the G1-T2 eval commands.
package llmusage

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"os"
	"sync"
	"sync/atomic"
	"time"
)

var (
	// ErrLedgerStop indicates that the available budget forbids another send.
	ErrLedgerStop = errors.New("eval ledger budget exhausted")
	// ErrLedger indicates a refused ledger or an accounting I/O failure.
	ErrLedger  = errors.New("eval ledger")
	ledgerStop atomic.Pointer[error]
)

// Run identifies a single allotment in a provider's shared ledger.
// AllotmentUSD is a decimal JSON number to preserve the exact budget.
type Run struct {
	Path, Genesis, Provider, Model string
	RunID, Family, Manifest        string
	AllotmentUSD                   string
}

// Ledger holds an open file and the validated settings for a run.
type Ledger struct {
	file   *os.File
	mu     sync.Mutex // flock serializes separate opens, this serializes calls on this file descriptor
	run    Run
	header ledgerHeader
	allot  *decimal
}

type ledgerHeader struct {
	Ledger        string    `json:"ledger"`
	Provider      string    `json:"provider"`
	Model         string    `json:"model"`
	Cap           *decimal  `json:"cap_usd"`
	PriceIn       *decimal  `json:"price_in_per_mtok"`
	PriceOut      *decimal  `json:"price_out_per_mtok"`
	OutputCeiling int64     `json:"output_ceiling_tokens"`
	CeilingSource string    `json:"ceiling_source"`
	Created       time.Time `json:"created"`
}

type ledgerRow struct {
	Timestamp time.Time       `json:"ts"`
	RunID     string          `json:"run_id"`
	Kind      string          `json:"kind"`
	Family    string          `json:"family"`
	Manifest  string          `json:"manifest"`
	Allotment *decimal        `json:"allotment_usd,omitempty"`
	ID        string          `json:"id,omitempty"`
	Attempt   int             `json:"attempt,omitempty"`
	Reserved  *decimal        `json:"reserved_usd,omitempty"`
	Settled   json.RawMessage `json:"settled_usd,omitempty"`
}

type reservation struct {
	ID      string
	Attempt int
	Cost    *decimal
}

type runState struct {
	allotRow     ledgerRow
	closed       bool
	spent        big.Rat
	reservations map[string][]attemptState
}

type attemptState struct {
	reserved *decimal
	settled  bool
}

type ledgerState map[string]*runState

// decimal preserves exact comparisons and arithmetic at a budget boundary.
type decimal struct{ *big.Rat }

func (d *decimal) UnmarshalJSON(raw []byte) error {
	if len(raw) == 0 || raw[0] < '0' || raw[0] > '9' || !json.Valid(raw) {
		return errors.New("expected a non-negative decimal amount")
	}
	v, ok := new(big.Rat).SetString(string(raw))
	if !ok {
		return errors.New("invalid decimal amount")
	}
	d.Rat = v
	return nil
}

func (d decimal) MarshalJSON() ([]byte, error) {
	return []byte(d.text()), nil
}

func (d decimal) text() string {
	places, _ := d.FloatPrec()
	return d.FloatString(places)
}

// Open checks the existing ledger and starts or resumes the requested run.
// The caller must Close the returned handle.
func Open(run Run) (*Ledger, error) {
	allot := new(decimal)
	if run.Path == "" || !isDigest(run.Genesis) || run.RunID == "" || run.Model == "" ||
		(run.Provider != "DS" && run.Provider != "GM") || !validFamily(run.Family) || !validManifest(run.Manifest) ||
		allot.UnmarshalJSON([]byte(run.AllotmentUSD)) != nil {
		return nil, fmt.Errorf("%w: invalid run settings", ErrLedger)
	}
	f, err := os.OpenFile(run.Path, os.O_APPEND|os.O_RDWR, 0)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrLedger, err)
	}
	l := &Ledger{file: f, run: run, allot: allot}
	err = l.withLock(func() error {
		state, header, err := l.replay()
		if err != nil {
			return err
		}
		l.header = header
		if existing := state[run.RunID]; existing != nil {
			if !l.matches(existing) {
				return errors.New("existing allotment disagrees with the run settings")
			}
		}
		if err := l.Check(); err != nil {
			return err
		}
		// The current run's spent amount cancels out of the start check.
		needed := state.committedElsewhere(run.RunID)
		needed.Add(needed, allot.Rat)
		if needed.Cmp(header.Cap.Rat) > 0 {
			return stopLedger(ErrLedgerStop)
		}
		if state[run.RunID] != nil {
			return nil
		}
		row := l.newRow("allot")
		row.Allotment = allot
		return l.append(row)
	})
	if err != nil {
		_ = f.Close()
		return nil, err
	}
	return l, nil
}

// Check can be installed as an attempt gate. Once a run exhausts its budget,
// all metered sends in the process are disabled.
func (l *Ledger) Check() error {
	if stopped := ledgerStop.Load(); stopped != nil {
		return *stopped
	}
	return nil
}

func stopLedger(err error) error {
	ledgerStop.CompareAndSwap(nil, &err)
	return err
}

// Close releases the file handle.
func (l *Ledger) Close() error { return l.file.Close() }

// Complete appends the close row, releasing the run's unused allotment.
func (l *Ledger) Complete() error {
	return l.withLock(func() error {
		state, _, err := l.replay()
		if err != nil {
			return err
		}
		run := state[l.run.RunID]
		if !l.matches(run) {
			return errors.New("allotment changed during the run")
		}
		if run.closed {
			return nil
		}
		return l.append(l.newRow("close"))
	})
}

func (l *Ledger) reserve(id string, bodyBytes int64) (reservation, error) {
	if err := l.Check(); err != nil {
		return reservation{}, err
	}
	r := reservation{ID: id}
	err := l.withLock(func() error {
		if err := l.Check(); err != nil {
			return err
		}
		state, header, err := l.replay()
		if err != nil {
			return err
		}
		run := state[l.run.RunID]
		if !l.matches(run) {
			return errors.New("allotment changed during the run")
		}
		if run.closed {
			return stopLedger(ErrLedgerStop)
		}
		r.Cost = header.cost(bodyBytes, header.OutputCeiling)
		next := new(big.Rat).Add(&run.spent, r.Cost.Rat)
		total := new(big.Rat).Add(state.committedElsewhere(l.run.RunID), next)
		if next.Cmp(l.allot.Rat) > 0 || total.Cmp(header.Cap.Rat) > 0 {
			return stopLedger(ErrLedgerStop)
		}
		if err := l.Check(); err != nil {
			return err
		}
		r.Attempt = len(run.reservations[id]) + 1
		row := l.newRow("reserve")
		row.ID, row.Attempt, row.Reserved, row.Settled = r.ID, r.Attempt, r.Cost, json.RawMessage("null")
		return l.append(row)
	})
	if err != nil {
		return reservation{}, stopLedger(err)
	}
	return r, nil
}

func (l *Ledger) settle(r reservation, usage *Usage) error {
	row := l.newRow("settle")
	row.ID, row.Attempt, row.Reserved = r.ID, r.Attempt, r.Cost
	row.Settled = json.RawMessage("null")
	if usage != nil {
		out := usage.Total - usage.Prompt
		if usage.Total == 0 {
			out = usage.Completion + usage.Reasoning
		}
		row.Settled = json.RawMessage(l.header.cost(usage.Prompt, out).text())
	}
	if err := l.withLock(func() error { return l.append(row) }); err != nil {
		return stopLedger(err)
	}
	return nil
}

func (h ledgerHeader) cost(in, out int64) *decimal {
	prompt := new(big.Rat).Mul(h.PriceIn.Rat, big.NewRat(in, 1_000_000))
	completion := new(big.Rat).Mul(h.PriceOut.Rat, big.NewRat(out, 1_000_000))
	return &decimal{prompt.Add(prompt, completion)}
}

func (l *Ledger) newRow(kind string) ledgerRow {
	return ledgerRow{Timestamp: time.Now().UTC(), RunID: l.run.RunID, Kind: kind, Family: l.run.Family, Manifest: l.run.Manifest}
}

func (l *Ledger) append(row ledgerRow) error {
	raw, err := json.Marshal(row)
	if err != nil {
		return err
	}
	if _, err := l.file.Write(append(raw, '\n')); err != nil {
		return err
	}
	return l.file.Sync()
}

func (l *Ledger) withLock(fn func() error) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if err := lockFile(l.file); err != nil {
		return fmt.Errorf("%w: lock: %w", ErrLedger, err)
	}
	err := fn()
	if unlockErr := unlockFile(l.file); unlockErr != nil {
		err = errors.Join(err, unlockErr)
	}
	if err != nil && !errors.Is(err, ErrLedgerStop) && !errors.Is(err, ErrLedger) {
		err = fmt.Errorf("%w: %w", ErrLedger, err)
	}
	return err
}

func (l *Ledger) replay() (ledgerState, ledgerHeader, error) {
	var header ledgerHeader
	if _, err := l.file.Seek(0, io.SeekStart); err != nil {
		return nil, header, err
	}
	reader := bufio.NewReader(l.file)
	line, err := reader.ReadBytes('\n')
	if err != nil {
		return nil, header, fmt.Errorf("read header: %w", err)
	}
	sum := sha256.Sum256(line)
	if hex.EncodeToString(sum[:]) != l.run.Genesis {
		return nil, header, errors.New("header genesis mismatch")
	}
	if err := json.Unmarshal(line, &header); err != nil ||
		header.Ledger != "gkg-g1t2" || header.Cap == nil || header.PriceIn == nil || header.PriceOut == nil ||
		header.OutputCeiling <= 0 || header.CeilingSource == "" || !validTime(header.Created) {
		return nil, header, errors.New("invalid ledger header")
	}
	if header.Provider != l.run.Provider || header.Model != l.run.Model {
		return nil, header, errors.New("ledger provider or model mismatch")
	}
	state := make(ledgerState)
	for n := 2; ; n++ {
		line, err := reader.ReadBytes('\n')
		if err == io.EOF && len(line) == 0 {
			return state, header, nil
		}
		if err != nil {
			return nil, header, fmt.Errorf("line %d: read incomplete ledger row: %w", n, err)
		}
		var row ledgerRow
		if json.Unmarshal(line, &row) != nil || !state.record(row) {
			return nil, header, fmt.Errorf("line %d: invalid ledger row", n)
		}
	}
}

func (s ledgerState) record(row ledgerRow) bool {
	earmark := row.Kind == "allot" && row.Manifest == "*"
	if !validTime(row.Timestamp) || row.RunID == "" || !validFamily(row.Family) || (!earmark && !validManifest(row.Manifest)) {
		return false
	}
	run := s[row.RunID]
	if row.Kind == "allot" {
		if run != nil || row.Allotment == nil {
			return false
		}
		s[row.RunID] = &runState{allotRow: row, reservations: make(map[string][]attemptState)}
		return true
	}
	if !run.matchesIdentity(row.Family, row.Manifest) {
		return false
	}
	if row.Kind == "close" {
		if run.closed {
			return false
		}
		run.closed = true
		return true
	}
	if row.ID == "" || row.Attempt < 1 || row.Reserved == nil || len(row.Settled) == 0 {
		return false
	}
	attempts := run.reservations[row.ID]
	isNull := bytes.Equal(row.Settled, []byte("null"))
	switch row.Kind {
	case "reserve":
		if run.closed || row.Attempt != len(attempts)+1 || !isNull {
			return false
		}
		run.reservations[row.ID] = append(attempts, attemptState{reserved: row.Reserved})
		run.spent.Add(&run.spent, row.Reserved.Rat)
	case "settle":
		if row.Attempt > len(attempts) {
			return false
		}
		attempt := &attempts[row.Attempt-1]
		if attempt.settled || row.Reserved.Cmp(attempt.reserved.Rat) != 0 {
			return false
		}
		if !isNull {
			var actual decimal
			if json.Unmarshal(row.Settled, &actual) != nil {
				return false
			}
			run.spent.Sub(&run.spent, attempt.reserved.Rat)
			run.spent.Add(&run.spent, actual.Rat)
		}
		attempt.settled = true
	default:
		return false
	}
	return true
}

func (s ledgerState) committedElsewhere(runID string) *big.Rat {
	total := new(big.Rat)
	for id, run := range s {
		if id == runID {
			continue
		}
		cost := &run.spent
		if !run.closed && cost.Cmp(run.allotRow.Allotment.Rat) < 0 {
			cost = run.allotRow.Allotment.Rat
		}
		total.Add(total, cost)
	}
	return total
}

func (l *Ledger) matches(run *runState) bool {
	return run.matchesIdentity(l.run.Family, l.run.Manifest) && run.allotRow.Allotment.Cmp(l.allot.Rat) == 0
}

func (run *runState) matchesIdentity(family, manifest string) bool {
	return run != nil && run.allotRow.Family == family && (run.allotRow.Manifest == "*" || run.allotRow.Manifest == manifest)
}

func validFamily(s string) bool { return s == "J" || s == "A" || s == "S" }

func validManifest(s string) bool { return s == "fixture" || s == "smoke" || isDigest(s) }

func isDigest(s string) bool {
	if len(s) != sha256.Size*2 {
		return false
	}
	for _, c := range s {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

func validTime(t time.Time) bool {
	_, offset := t.Zone()
	return !t.IsZero() && offset == 0
}
