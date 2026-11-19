package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"

	"github.com/alessiosavi/go-knowledge-graph/internal/domain"
	"github.com/alessiosavi/go-knowledge-graph/internal/graph"
	"github.com/alessiosavi/go-knowledge-graph/internal/ingest"
)

const titleRepairBatch = 200

func runRepairTitles(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("repair-titles", flag.ContinueOnError)
	fs.SetOutput(stderr)
	apply := fs.Bool("apply", false, "fill empty document titles in the graph")
	scope := fs.String("domain", "", "scan only documents in this domain")
	out := fs.String("out", "", "write proposed titles to a new private JSONL file")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if fs.NArg() > 0 {
		fmt.Fprintf(stderr, "gkg repair-titles: unexpected argument %q\n", fs.Arg(0))
		return 2
	}
	if *scope != "" && !domain.ValidDomain(*scope) {
		fmt.Fprintln(stderr, "gkg repair-titles: invalid domain slug")
		return 2
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	fail := func(err error) int {
		fmt.Fprintf(stderr, "gkg repair-titles: %v\n", err)
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

	var report *os.File
	if *out != "" {
		report, err = os.OpenFile(*out, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
		if err != nil {
			return fail(err)
		}
	}
	summary, err := scanRepairTitles(ctx, st, *scope, *apply && report == nil, report)
	if report != nil {
		err = errors.Join(err, report.Close())
	}
	if err == nil && *apply && report != nil {
		// Re-read the empty titles page by page after the report is closed.
		summary, err = scanRepairTitles(ctx, st, *scope, true, nil)
	}
	if err != nil {
		return fail(fmt.Errorf("%w (%s)", err, summary))
	}
	fmt.Fprintln(stdout, summary)
	return 0
}

type titleRepairSummary struct {
	total   int
	origins map[string]int
}

func (s titleRepairSummary) String() string {
	frontMatter, h1, slug := s.origins["frontmatter"], s.origins["h1"], s.origins["slug"]
	return fmt.Sprintf("total=%d frontmatter=%d h1=%d slug=%d unchanged=%d",
		s.total, frontMatter, h1, slug, s.total-frontMatter-h1-slug)
}

func (s titleRepairSummary) apply(ctx context.Context, st *graph.Store, scope string, updates []graph.DocumentTitleUpdate) error {
	if len(updates) == 0 {
		return nil
	}
	counts, err := st.SetMissingDocumentTitles(ctx, scope, updates)
	if err != nil {
		return err
	}
	for origin, n := range counts {
		s.origins[origin] += n
	}
	return nil
}

func scanRepairTitles(ctx context.Context, st *graph.Store, scope string, apply bool, report *os.File) (titleRepairSummary, error) {
	var encoder *json.Encoder
	if report != nil {
		encoder = json.NewEncoder(report)
	}
	summary := titleRepairSummary{origins: make(map[string]int)}
	after := ""
	for {
		page, err := st.PageDocumentTitleSeeds(ctx, after, scope, titleRepairBatch)
		if err != nil {
			return summary, err
		}
		var updates []graph.DocumentTitleUpdate
		for _, doc := range page {
			summary.total++
			if doc.HasTitle {
				continue
			}
			title, origin := ingest.DeriveTitle(doc.Text, doc.Source)
			if title == "" {
				continue
			}
			update := graph.DocumentTitleUpdate{ID: doc.ID, Title: title, TitleSource: origin}
			if encoder != nil {
				if err := encoder.Encode(update); err != nil {
					return summary, err
				}
			}
			if apply {
				updates = append(updates, update)
			} else {
				summary.origins[origin]++
			}
		}
		if apply {
			if err := summary.apply(ctx, st, scope, updates); err != nil {
				return summary, err
			}
		}
		if len(page) < titleRepairBatch {
			return summary, nil
		}
		after = page[len(page)-1].ID
	}
}
