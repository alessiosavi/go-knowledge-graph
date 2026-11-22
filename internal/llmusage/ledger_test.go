package llmusage

import (
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func testRun(t *testing.T, cap float64) Run {
	t.Helper()
	ledgerStop.Store(nil)
	t.Cleanup(func() { ledgerStop.Store(nil) })
	path := filepath.Join(t.TempDir(), "ledger.jsonl")
	line := fmt.Sprintf("{\"ledger\":\"gkg-g1t2\",\"provider\":\"DS\",\"model\":\"test\",\"cap_usd\":%g,\"price_in_per_mtok\":10,\"price_out_per_mtok\":100000,\"output_ceiling_tokens\":10,\"ceiling_source\":\"fixture\",\"created\":\"2026-11-22T00:00:00Z\"}\n", cap)
	if err := os.WriteFile(path, []byte(line), 0600); err != nil {
		t.Fatal(err)
	}
	return Run{Path: path, Genesis: fmt.Sprintf("%x", sha256.Sum256([]byte(line))), Provider: "DS",
		Model: "test", RunID: "run-1", Family: "J", Manifest: "fixture", AllotmentUSD: "2"}
}

func openRun(t *testing.T, run Run) *Ledger {
	t.Helper()
	ledger, err := Open(run)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ledger.Close() })
	return ledger
}

func ledgerText(t *testing.T, path string) string {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Error(err)
	}
	return string(raw)
}

func ledgerRows(t *testing.T, path, kind string) []map[string]any {
	t.Helper()
	var rows []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(ledgerText(t, path)), "\n")[1:] {
		var row map[string]any
		if err := json.Unmarshal([]byte(line), &row); err != nil {
			t.Error(err)
		}
		if row["kind"] == kind {
			rows = append(rows, row)
		}
	}
	return rows
}

func appendLedgerFixtureRow(t *testing.T, path, row string) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	_, writeErr := f.WriteString(row + "\n")
	if err := errors.Join(writeErr, f.Close()); err != nil {
		t.Fatal(err)
	}
}

const earmarkRow = `{"ts":"2026-11-22T00:00:00Z","run_id":"earmarked","kind":"allot","family":"J","manifest":"*","allotment_usd":2}`

func TestLedgerEarmarkAllowsRetryAndOtherRuns(t *testing.T) {
	run := testRun(t, 4)
	appendLedgerFixtureRow(t, run.Path, earmarkRow)
	openRun(t, run)
	retry := run
	retry.RunID, retry.Manifest = "earmarked", strings.Repeat("a", 64)
	before := ledgerText(t, run.Path)
	for _, tc := range []struct {
		name         string
		family       string
		allotmentUSD string
	}{
		{"family", "S", "2"},
		{"allotment", "J", "1"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			wrong := retry
			wrong.Family, wrong.AllotmentUSD = tc.family, tc.allotmentUSD
			if _, err := Open(wrong); !errors.Is(err, ErrLedger) || !strings.Contains(err.Error(), "existing allotment disagrees") {
				t.Fatalf("Open = %v, want allotment mismatch", err)
			}
			if ledgerText(t, run.Path) != before {
				t.Fatal("refused start wrote to ledger")
			}
		})
	}
	ledger := openRun(t, retry)
	if got := ledgerText(t, run.Path); got != before {
		t.Fatalf("retry start changed the ledger: %s", got)
	}
	r, err := ledger.reserve("item", 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := ledger.settle(r, nil); err != nil {
		t.Fatal(err)
	}
	if err := openRun(t, retry).Complete(); err != nil {
		t.Fatal(err)
	}
	for _, kind := range []string{"reserve", "settle", "close"} {
		rows := ledgerRows(t, run.Path, kind)
		if len(rows) != 1 || rows[0]["manifest"] != retry.Manifest || rows[0]["run_id"] != retry.RunID {
			t.Fatalf("%s rows = %v", kind, rows)
		}
	}
	openRun(t, run)
	if rows := ledgerRows(t, run.Path, "allot"); len(rows) != 2 || rows[0]["manifest"] != "*" || rows[1]["run_id"] != run.RunID {
		t.Fatalf("allot rows = %v", rows)
	}
}

func TestLedgerRejectsWildcardOutsideAllot(t *testing.T) {
	t.Run("run settings", func(t *testing.T) {
		run := testRun(t, 4)
		run.RunID, run.Manifest = "earmarked", "*"
		appendLedgerFixtureRow(t, run.Path, earmarkRow)
		if _, err := Open(run); !errors.Is(err, ErrLedger) || !strings.Contains(err.Error(), "invalid run settings") {
			t.Fatalf("Open = %v, want invalid run settings", err)
		}
	})
	for _, tc := range []struct{ kind, fields string }{
		{"reserve", `,"id":"item","attempt":2,"reserved_usd":1,"settled_usd":null`},
		{"settle", `,"id":"item","attempt":1,"reserved_usd":1,"settled_usd":null`},
		{"close", ""},
	} {
		t.Run(tc.kind, func(t *testing.T) {
			run := testRun(t, 4)
			run.RunID = "earmarked"
			appendLedgerFixtureRow(t, run.Path, earmarkRow)
			if _, err := openRun(t, run).reserve("item", 0); err != nil {
				t.Fatal(err)
			}
			appendLedgerFixtureRow(t, run.Path, fmt.Sprintf(`{"ts":"2026-11-22T00:00:00Z","run_id":"earmarked","kind":%q,"family":"J","manifest":"*"%s}`, tc.kind, tc.fields))
			if _, err := Open(run); !errors.Is(err, ErrLedger) || !strings.Contains(err.Error(), "line 4: invalid ledger row") {
				t.Fatalf("Open = %v, want wildcard row rejection", err)
			}
		})
	}
}

func TestLedgerEarmarkCountsAgainstCap(t *testing.T) {
	t.Run("start", func(t *testing.T) {
		run := testRun(t, 3)
		appendLedgerFixtureRow(t, run.Path, earmarkRow)
		before := ledgerText(t, run.Path)
		if _, err := Open(run); !errors.Is(err, ErrLedgerStop) {
			t.Fatalf("start with earmark = %v, want budget stop", err)
		}
		if ledgerText(t, run.Path) != before {
			t.Fatal("budget stop wrote to ledger")
		}
	})
	t.Run("reserve", func(t *testing.T) {
		run := testRun(t, 3)
		ledger := openRun(t, run)
		appendLedgerFixtureRow(t, run.Path, earmarkRow)
		if _, err := ledger.reserve("item", 0); err != nil {
			t.Fatalf("reserve at cap with earmark: %v", err)
		}
		before := ledgerText(t, run.Path)
		if _, err := ledger.reserve("item", 0); !errors.Is(err, ErrLedgerStop) {
			t.Fatalf("reserve above cap with earmark = %v, want budget stop", err)
		}
		if ledgerText(t, run.Path) != before {
			t.Fatal("budget stop wrote to ledger")
		}
	})
}

func TestLedgerRejectsForeignHeaderAndMissingFile(t *testing.T) {
	for _, tc := range []struct {
		name string
		edit func(*Run)
	}{
		{"genesis", func(r *Run) { r.Genesis = strings.Repeat("0", 64) }},
		{"model", func(r *Run) { r.Model = "other" }},
		{"provider", func(r *Run) { r.Provider = "GM" }},
		{"missing", func(r *Run) { r.Path += ".missing" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			run := testRun(t, 2)
			path := run.Path
			original := ledgerText(t, path)
			tc.edit(&run)
			if _, err := Open(run); !errors.Is(err, ErrLedger) || errors.Is(err, ErrLedgerStop) {
				t.Fatalf("Open = %v", err)
			}
			if ledgerText(t, path) != original {
				t.Fatal("refused start wrote to ledger")
			}
			if tc.name == "missing" {
				if _, err := os.Stat(run.Path); !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("missing ledger was created: %v", err)
				}
			}
		})
	}
}

func TestLedgerRefusesUnavailableAllotment(t *testing.T) {
	run := testRun(t, 3)
	ledger := openRun(t, run)
	run.RunID = "run-2"
	if _, err := Open(run); !errors.Is(err, ErrLedgerStop) {
		t.Fatalf("Open = %v", err)
	}
	if err := ledger.Check(); !errors.Is(err, ErrLedgerStop) {
		t.Fatalf("Check = %v", err)
	}
	if n := len(ledgerRows(t, run.Path, "allot")); n != 1 {
		t.Fatalf("allot rows = %d", n)
	}
}

func TestLedgerResumeSumsPriorRowsAndAttempts(t *testing.T) {
	run := testRun(t, 3)
	ledger := openRun(t, run)
	first, err := ledger.reserve("item", 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := ledger.settle(first, &Usage{Prompt: 10, Completion: 2, Reasoning: 3}); err != nil {
		t.Fatal(err)
	}
	second := openRun(t, run)
	next, err := second.reserve("item", 0)
	if err != nil || next.Attempt != 2 {
		t.Fatalf("next = %+v, %v", next, err)
	}
	if _, err := second.reserve("item", 0); !errors.Is(err, ErrLedgerStop) {
		t.Fatalf("resume did not count prior spend: %v", err)
	}
	if n := len(ledgerRows(t, run.Path, "allot")); n != 1 {
		t.Fatalf("allot rows = %d", n)
	}
}

func TestLedgerReplayCountsReservationWithoutSettle(t *testing.T) {
	run := testRun(t, 2)
	run.AllotmentUSD = "1"
	ledger, err := Open(run)
	if err != nil {
		t.Fatal(err)
	}
	_, reserveErr := ledger.reserve("item", 0)
	if err := errors.Join(reserveErr, ledger.Close()); err != nil {
		t.Fatal(err)
	}
	if len(ledgerRows(t, run.Path, "settle")) != 0 {
		t.Fatal("fixture unexpectedly settled the reservation")
	}
	resumed := openRun(t, run)
	before := ledgerText(t, run.Path)
	if _, err := resumed.reserve("item", 0); !errors.Is(err, ErrLedgerStop) {
		t.Fatalf("reserve after replay = %v, want budget stop", err)
	}
	if ledgerText(t, run.Path) != before {
		t.Fatal("budget stop wrote to ledger")
	}
}

func TestLedgerCapAcrossManifestsAndClosedAllotment(t *testing.T) {
	run := testRun(t, 3)
	ledger := openRun(t, run)
	first, err := ledger.reserve("item", 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := ledger.settle(first, nil); err != nil {
		t.Fatal(err)
	}
	if err := ledger.Complete(); err != nil {
		t.Fatal(err)
	}
	run.RunID, run.Manifest = "run-2", strings.Repeat("a", 64)
	second := openRun(t, run)
	if _, err := second.reserve("item", 0); err != nil {
		t.Fatal(err)
	}
	if _, err := second.reserve("item", 0); err != nil {
		t.Fatal(err)
	}
	run.RunID, run.AllotmentUSD = "run-3", "0.00001"
	if _, err := Open(run); !errors.Is(err, ErrLedgerStop) {
		t.Fatalf("start above cumulative cap = %v", err)
	}
}

func TestLedgerReserveRechecksCapAndProtectsSharedAllotment(t *testing.T) {
	run := testRun(t, 4)
	other := run
	other.RunID = "other"
	first := openRun(t, other)
	prior, err := first.reserve("item", 0)
	if err != nil {
		t.Fatal(err)
	}
	second := openRun(t, run)
	// A provider may report more than the pinned output ceiling. Accounting
	// must then protect the other run's allotment on the next reservation.
	if err := first.settle(prior, &Usage{Completion: 25}); err != nil {
		t.Fatal(err)
	}
	if _, err := second.reserve("item", 0); err != nil {
		t.Fatal(err)
	}
	if _, err := second.reserve("item", 0); !errors.Is(err, ErrLedgerStop) {
		t.Fatalf("reserve above cap = %v", err)
	}
}

func TestLedgerConcurrentReservations(t *testing.T) {
	run := testRun(t, 2)
	a, b := openRun(t, run), openRun(t, run)
	var wg sync.WaitGroup
	errs := make(chan error, 4)
	for _, ledger := range []*Ledger{a, a, b, b} {
		wg.Go(func() {
			_, err := ledger.reserve("same", 0)
			errs <- err
		})
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil && !errors.Is(err, ErrLedgerStop) {
			t.Fatal(err)
		}
	}
	rows := ledgerRows(t, run.Path, "reserve")
	if len(rows) != 2 || rows[0]["attempt"] != float64(1) || rows[1]["attempt"] != float64(2) {
		t.Fatalf("reserve rows = %v", rows)
	}
}
