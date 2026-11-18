package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"maps"
	"os"
	"os/signal"
	"slices"
	"strings"
	"syscall"
	"unicode/utf8"

	"github.com/alessiosavi/go-knowledge-graph/internal/config"
	"github.com/alessiosavi/go-knowledge-graph/internal/domain"
	"github.com/alessiosavi/go-knowledge-graph/internal/graph"
	"github.com/alessiosavi/go-knowledge-graph/internal/hygiene"
)

func runRepairLabels(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("repair-labels", flag.ContinueOnError)
	fs.SetOutput(stderr)
	dryRun := fs.Bool("dry-run", false, "preview the repair without writing to the graph")
	scope := fs.String("domain", "", "scan chunks belonging to documents in this domain")
	reportPath := fs.String("report", "", "write matched spans to a new private JSONL file")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if fs.NArg() > 0 {
		fmt.Fprintf(stderr, "gkg repair-labels: unexpected argument %q\n", fs.Arg(0))
		return 2
	}
	if *scope != "" && !domain.ValidDomain(*scope) {
		fmt.Fprintln(stderr, "gkg repair-labels: invalid domain slug")
		return 2
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	fail := func(err error) int {
		fmt.Fprintf(stderr, "gkg repair-labels: %v\n", err)
		return 1
	}
	cfg, err := loadConfig()
	if err != nil {
		return fail(err)
	}
	st, err := graph.Open(ctx, cfg)
	if err != nil {
		return fail(err)
	}
	defer st.Close(context.Background())
	if _, err := st.ExtractionSchemaCurrent(ctx); err != nil {
		return fail(err)
	}

	var report *os.File
	if *reportPath != "" {
		report, err = openLabelRepairReport(*reportPath)
		if err != nil {
			return fail(err)
		}
	}
	summary, pending, err := scanRepairLabels(ctx, st, cfg, *scope, *dryRun, report, stderr)
	if report != nil {
		err = errors.Join(err, report.Close())
	}
	if err != nil {
		return fail(fmt.Errorf("%w (%s)", err, summary))
	}

	// Complete and close the report before applying its planned deletions.
	for _, chunk := range pending {
		changes, err := st.DeleteChunkMentionSpans(ctx, chunk.id, chunk.text, slices.Collect(maps.Keys(chunk.rules)))
		if err != nil {
			return fail(fmt.Errorf("%w (%s)", err, summary))
		}
		summary.addChanges(changes, chunk.rules)
	}
	fmt.Fprintln(stdout, summary)
	return 0
}

func openLabelRepairReport(path string) (*os.File, error) {
	return os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
}

type labelRepairSummary struct {
	scanned, headingLead, leadIn    int
	confirmed, other, candidates    int
	rejected, entities, skippedDocs int
}

func (s labelRepairSummary) String() string {
	return fmt.Sprintf("scanned_chunks=%d spans=%d heading_lead=%d lead_in=%d facts_confirmed=%d facts_other=%d candidates=%d rejected=%d entities_removed=%d skipped_docs=%d",
		s.scanned, s.headingLead+s.leadIn, s.headingLead, s.leadIn, s.confirmed, s.other, s.candidates, s.rejected, s.entities, s.skippedDocs)
}

func (s *labelRepairSummary) addChanges(changes graph.ChunkSpanChanges, rules map[graph.ChunkMentionSpan]string) {
	for span, n := range changes.Mentions {
		if rules[span] == hygiene.HeadingLead {
			s.headingLead += n
		} else {
			s.leadIn += n
		}
	}
	s.confirmed += changes.FactsConfirmed
	s.other += changes.FactsOther
	s.candidates += changes.Candidates
	s.rejected += changes.Rejected
	s.entities += changes.EntitiesRemoved
}

type labelRepairRow struct {
	Domain        string `json:"domain"`
	DocID         string `json:"doc_id"`
	ChunkID       string `json:"chunk_id"`
	EntityKey     string `json:"entity_key"`
	Name          string `json:"name"`
	Type          string `json:"type"`
	Rule          string `json:"rule"`
	EntityRemoved bool   `json:"entity_removed"`
	Line          string `json:"line"`
}

type labelChunkRepair struct {
	id, text string
	rules    map[graph.ChunkMentionSpan]string
}

func scanRepairLabels(ctx context.Context, st *graph.Store, cfg config.Config, scope string, dryRun bool, report *os.File, stderr io.Writer) (labelRepairSummary, []labelChunkRepair, error) {
	const pageSize = 200
	applyAfterReport := !dryRun && report != nil
	var summary labelRepairSummary
	var pending []labelChunkRepair
	var rows []labelRepairRow
	dropped := make(map[string]int)
	presets := make(map[string]hygiene.Preset)
	skipped := make(map[string]bool)
	after := ""
	for {
		page, err := st.PageChunkMentions(ctx, after, scope, pageSize)
		if err != nil {
			return summary, nil, err
		}
		for _, chunk := range page {
			summary.scanned++
			if skipped[chunk.DocID] {
				continue
			}
			if chunk.Status != graph.StatusDone {
				skipped[chunk.DocID] = true
				summary.skippedDocs++
				continue
			}
			if len(chunk.Mentions) == 0 {
				continue
			}
			preset, ok := presets[chunk.Labels]
			if !ok {
				preset, err = hygiene.LoadPreset(cfg.Labels.PresetsDir, chunk.Labels, cfg.Ingest.StoplistFile)
				if err != nil {
					fmt.Fprintf(stderr, "gkg repair-labels: warning: skipping document %s: %v\n", chunk.DocID, err)
					skipped[chunk.DocID] = true
					summary.skippedDocs++
					continue
				}
				presets[chunk.Labels] = preset
			}
			rules := make(map[graph.ChunkMentionSpan]string)
			matched := make(map[graph.ChunkMentionSpan]int)
			for _, m := range chunk.Mentions {
				rule := hygiene.SectionLabel(chunk.Text, m.Start, m.End, preset)
				if rule == "" {
					continue
				}
				span := m.ChunkMentionSpan
				if report != nil && matched[span] == 0 {
					rows = append(rows, labelRepairRow{
						Domain: chunk.Domain, DocID: chunk.DocID, ChunkID: chunk.ID, EntityKey: m.Key,
						Name: m.Name, Type: m.Type, Rule: rule, Line: strings.Clone(labelReportLine(chunk.Text, m.Start)),
					})
				}
				matched[span]++
				rules[span] = rule
			}
			if len(matched) == 0 {
				continue
			}
			if applyAfterReport {
				pending = append(pending, labelChunkRepair{id: chunk.ID, text: chunk.Text, rules: rules})
				for span, n := range matched {
					dropped[span.Key] += n
				}
				continue
			}
			spans := slices.Collect(maps.Keys(rules))
			var changes graph.ChunkSpanChanges
			if dryRun {
				changes, err = st.PreviewChunkMentionSpans(ctx, chunk.ID, chunk.Text, spans)
			} else {
				changes, err = st.DeleteChunkMentionSpans(ctx, chunk.ID, chunk.Text, spans)
			}
			if err != nil {
				return summary, nil, err
			}
			summary.addChanges(changes, rules)
			if dryRun {
				for span, n := range changes.Mentions {
					dropped[span.Key] += n
				}
			}
		}
		if len(page) < pageSize {
			break
		}
		after = page[len(page)-1].ID
	}
	if !dryRun && report == nil {
		return summary, nil, nil
	}

	removed := make(map[string]bool)
	for keys := range slices.Chunk(slices.Collect(maps.Keys(dropped)), pageSize) {
		totals, err := st.CountMentionEdges(ctx, keys)
		if err != nil {
			return summary, nil, err
		}
		for key, n := range totals {
			if n == dropped[key] {
				removed[key] = true
			}
		}
	}
	if dryRun {
		summary.entities = len(removed)
	}
	if report != nil {
		enc := json.NewEncoder(report)
		for _, row := range rows {
			row.EntityRemoved = removed[row.EntityKey]
			if err := enc.Encode(row); err != nil {
				return summary, nil, fmt.Errorf("write report: %w", err)
			}
		}
		if err := enc.Encode(map[string]int{"skipped_docs": summary.skippedDocs}); err != nil {
			return summary, nil, fmt.Errorf("write report: %w", err)
		}
	}
	return summary, pending, nil
}

func labelReportLine(text string, start int) string {
	lineStart := strings.LastIndexByte(text[:start], '\n') + 1
	line, _, _ := strings.Cut(text[lineStart:], "\n")
	line = strings.TrimSuffix(line, "\r")
	if len(line) <= 200 {
		return line
	}
	end := 200
	for !utf8.RuneStart(line[end]) {
		end--
	}
	return line[:end]
}
