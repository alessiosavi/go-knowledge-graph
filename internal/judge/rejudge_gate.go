package judge

import (
	"fmt"
	"io"
	"maps"
	"slices"
)

type rejudgeGateCounts struct {
	reconciled, expected int
	ok, bad              [4]int // confirmed, unclear, refuted, missing
}

func (c *rejudgeGateCounts) record(label, verdict string) {
	index := 3
	switch verdict {
	case "confirmed":
		index = 0
	case "unclear":
		index = 1
	case "refuted":
		index = 2
	}
	c.reconciled++
	c.expected++
	if label == "ok" {
		c.ok[index]++
	} else {
		c.bad[index]++
	}
}

func (c rejudgeGateCounts) okTotal() int {
	return c.ok[0] + c.ok[1] + c.ok[2] + c.ok[3]
}

func (c rejudgeGateCounts) passes() bool {
	return c.expected > 0 && 100*c.reconciled >= 95*c.expected &&
		c.ok[3]+c.bad[3] == 0 && c.okTotal() > 0 && 100*c.ok[0] >= 98*c.okTotal() &&
		100*c.ok[0] >= 95*(c.ok[0]+c.bad[0])
}

type rejudgeDistinctKey struct{ head, relation, tail, evidence string }

// Reduce duplicate occurrences conservatively for the distinct-fact diagnostic.
// A bad annotation makes the group bad. For ok groups every occurrence must
// be kept; for bad groups any kept occurrence counts against precision.
func (c rejudgeGateCounts) distinctVerdict() (label, verdict string) {
	totalBad := c.bad[0] + c.bad[1] + c.bad[2] + c.bad[3]
	label = "ok"
	if totalBad > 0 {
		label = "wrong"
	}
	switch {
	case c.ok[3]+c.bad[3] > 0:
		return label, ""
	case totalBad > 0 && c.ok[0]+c.bad[0] > 0, totalBad == 0 && c.ok[0] == c.reconciled:
		return label, "confirmed"
	case totalBad > 0 && c.ok[2]+c.bad[2] == c.reconciled, totalBad == 0 && c.ok[2] > 0:
		return label, "refuted"
	default:
		return label, "unclear"
	}
}

type rejudgeGateIssue struct{ kind, id, reason string }

// RejudgeFingerprints holds the frozen profile fingerprint for each preset.
type RejudgeFingerprints map[string]string

func (f RejudgeFingerprints) Write(w io.Writer) error {
	for _, preset := range slices.Sorted(maps.Keys(f)) {
		if _, err := fmt.Fprintf(w, "preset=%q fingerprint=%s\n", preset, f[preset]); err != nil {
			return err
		}
	}
	return nil
}

type RejudgeGate struct {
	fingerprints   RejudgeFingerprints
	incomplete     bool
	rows, distinct rejudgeGateCounts
	domains        map[string]*rejudgeGateCounts
	issues         []rejudgeGateIssue
}

func buildRejudgeGate(m rejudgeManifest, payloads map[string]v2Payload, done map[string]rejudgeVerdict, fingerprints RejudgeFingerprints) *RejudgeGate {
	g := &RejudgeGate{
		fingerprints: fingerprints,
		incomplete:   len(done) < len(m.rows),
		domains:      make(map[string]*rejudgeGateCounts),
	}
	domainCounts := func(domain string) *rejudgeGateCounts {
		if g.domains[domain] == nil {
			g.domains[domain] = &rejudgeGateCounts{}
		}
		return g.domains[domain]
	}
	for _, mismatch := range m.header.Mismatches {
		g.rows.expected++
		domainCounts(mismatch.Domain).expected++
		g.issues = append(g.issues, rejudgeGateIssue{"mismatched", mismatch.ID, mismatch.Reason})
	}
	distinct := make(map[rejudgeDistinctKey]rejudgeGateCounts)
	for _, row := range m.rows {
		if !scoredRejudgeLabel(row.Label) {
			continue
		}
		v := done[row.ID]
		g.rows.record(row.Label, v.Verdict)
		domainCounts(row.Domain).record(row.Label, v.Verdict)
		payload := payloads[row.ID]
		key := rejudgeDistinctKey{foldRejudgeName(payload.Fact.Head), row.Relation, foldRejudgeName(payload.Fact.Tail), payload.Evidence}
		c := distinct[key]
		c.record(row.Label, v.Verdict)
		distinct[key] = c
		if row.Label == "ok" && v.Verdict != "confirmed" {
			reason := v.Verdict + ": " + v.Reason
			if v.Verdict == "" {
				reason = "missing verdict"
			}
			g.issues = append(g.issues, rejudgeGateIssue{"lost-ok", row.ID, reason})
		} else if row.Label != "ok" && v.Verdict == "confirmed" {
			g.issues = append(g.issues, rejudgeGateIssue{"kept-bad", row.ID, v.Reason})
		}
	}
	if g.rows.expected == 0 {
		return nil
	}
	for _, c := range distinct {
		label, verdict := c.distinctVerdict()
		g.distinct.record(label, verdict)
	}
	return g
}

func (g *RejudgeGate) Passed() bool { return !g.incomplete && g.rows.passes() }

// Write reports the decision for the labelled set. Distinct facts and domain
// splits are diagnostics; the pre-registered thresholds use the row totals.
func (g *RejudgeGate) Write(w io.Writer) error {
	if err := g.fingerprints.Write(w); err != nil {
		return err
	}
	const format = "%-20s %-20s %-18s %-20s %-20s %7s %9s %s\n"
	if _, err := fmt.Fprintf(w, format, "set", "reconciled/expected", "judged/reconciled",
		"ok K/U/R/missing", "bad K/U/R/missing", "ok kept", "precision", "Wilson 95%"); err != nil {
		return err
	}
	write := func(name string, c rejudgeGateCounts) error {
		counts := func(n [4]int) string { return fmt.Sprintf("%d/%d/%d/%d", n[0], n[1], n[2], n[3]) }
		retained, precision, interval := "n/a", "n/a", "n/a"
		if c.okTotal() > 0 {
			retained = fmt.Sprintf("%.3f", float64(c.ok[0])/float64(c.okTotal()))
		}
		if n := c.ok[0] + c.bad[0]; n > 0 {
			p, lo, hi := wilson(c.ok[0], n)
			precision = fmt.Sprintf("%.3f", p)
			interval = fmt.Sprintf("[%.3f, %.3f] n=%d", lo, hi, n)
		}
		judged := c.reconciled - c.ok[3] - c.bad[3]
		_, err := fmt.Fprintf(w, format, name, fmt.Sprintf("%d/%d", c.reconciled, c.expected), fmt.Sprintf("%d/%d", judged, c.reconciled),
			counts(c.ok), counts(c.bad), retained, precision, interval)
		return err
	}
	if err := write("rows", g.rows); err != nil {
		return err
	}
	if err := write("distinct facts", g.distinct); err != nil {
		return err
	}
	for _, domain := range slices.Sorted(maps.Keys(g.domains)) {
		if err := write(fmt.Sprintf("domain %q", domain), *g.domains[domain]); err != nil {
			return err
		}
	}
	for _, issue := range g.issues {
		if _, err := fmt.Fprintf(w, "%s %s reason=%q\n", issue.kind, issue.id, issue.reason); err != nil {
			return err
		}
	}
	if g.Passed() {
		_, err := fmt.Fprintln(w, "GATE PASS")
		return err
	}
	_, err := fmt.Fprintln(w, "GATE FAIL")
	return err
}
