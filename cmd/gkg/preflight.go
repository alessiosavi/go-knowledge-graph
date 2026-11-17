package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/alessiosavi/go-knowledge-graph/internal/config"
	"github.com/alessiosavi/go-knowledge-graph/internal/domain"
	"github.com/alessiosavi/go-knowledge-graph/internal/graph"
)

func migrationOptions(cfg config.Config) (graph.MigrationOptions, error) {
	opt := graph.MigrationOptions{Params: domain.Params{Tau: cfg.Domain.Tau, Delta: cfg.Domain.Delta, TauOverride: cfg.Domain.TauOverride}}
	switch cfg.Domain.SeedFile {
	case "":
	case "detect-all":
		opt.DetectAll = true
	default:
		f, err := os.Open(cfg.Domain.SeedFile)
		if err != nil {
			return opt, fmt.Errorf("%w: %v", graph.ErrSeedRequired, err)
		}
		defer f.Close()
		seed, err := domain.LoadSeed(f)
		if err != nil {
			return opt, fmt.Errorf("%w: %v", graph.ErrSeedRequired, err)
		}
		opt.Seed = &seed
	}
	return opt, nil
}

func migrateSchema(ctx context.Context, st *graph.Store, cfg config.Config) (graph.MigrationResult, error) {
	current, err := st.ExtractionSchemaCurrent(ctx)
	if err != nil {
		return graph.MigrationResult{}, err
	}
	if current {
		return st.MigrateExtractionSchema(ctx)
	}
	opt, err := migrationOptions(cfg)
	if err != nil {
		return graph.MigrationResult{}, err
	}
	return st.MigrateExtractionSchemaOpts(ctx, opt)
}

// runPreflight never bootstraps, builds a pipeline, or opens the spool. Deploy
// consumes its exit status: current/empty=0, seed error=3, migration ready=10.
func runPreflight(args []string, _ io.Writer, stderr io.Writer) int {
	fs := flag.NewFlagSet("preflight", flag.ContinueOnError)
	fs.SetOutput(stderr)
	if err := fs.Parse(args); err != nil {
		return 1
	}
	if fs.NArg() > 0 {
		fmt.Fprintf(stderr, "gkg preflight: unexpected argument %q\n", fs.Arg(0))
		return 1
	}
	ctx := context.Background()
	cfg, err := loadConfig()
	if err != nil {
		fmt.Fprintf(stderr, "gkg preflight: %v\n", err)
		return 1
	}
	st, err := graph.Open(ctx, cfg)
	if err != nil {
		fmt.Fprintf(stderr, "gkg preflight: %v\n", err)
		return 1
	}
	defer st.Close(ctx)
	current, err := st.ExtractionSchemaCurrent(ctx)
	if err != nil {
		fmt.Fprintf(stderr, "gkg preflight: %v\n", err)
		return 1
	}
	if current {
		return 0
	}
	documents, err := st.RunReadQuery(ctx, `MATCH (d:Document)
WHERE d.status IS NULL OR d.status <> 'superseded'
RETURN d.id AS id,d.source AS source ORDER BY d.id`, nil, 0)
	if err != nil {
		fmt.Fprintf(stderr, "gkg preflight: %v\n", err)
		return 1
	}
	hasDocuments := len(documents.Rows) > 0
	if !hasDocuments {
		// Superseded documents still make this a document-bearing graph. Orphan
		// derived nodes also require stopping the old writer before clearing them.
		documents, err := st.RunReadQuery(ctx, `MATCH (d:Document) RETURN d.id LIMIT 1`, nil, 0)
		if err != nil {
			fmt.Fprintf(stderr, "gkg preflight: %v\n", err)
			return 1
		}
		hasDocuments = len(documents.Rows) > 0
		if !hasDocuments {
			data, err := st.RunReadQuery(ctx, `MATCH (n) WHERE n:Entity OR n:RejectedFact OR n:FactCandidate RETURN n LIMIT 1`, nil, 0)
			if err != nil {
				fmt.Fprintf(stderr, "gkg preflight: %v\n", err)
				return 1
			}
			if len(data.Rows) == 0 {
				return 0
			}
		}
	}
	opt, err := migrationOptions(cfg)
	if err != nil {
		fmt.Fprintf(stderr, "gkg preflight: %v\n", err)
		return 3
	}
	if opt.DetectAll || !hasDocuments {
		return 10
	}
	if opt.Seed == nil && len(documents.Rows) == 0 {
		fmt.Fprintf(stderr, "gkg preflight: %v: no live documents but legacy rows remain\n", graph.ErrSeedRequired)
		return 3
	}
	var unmatched []string
	for _, doc := range documents.Rows {
		source, _ := doc["source"].(string)
		if opt.Seed != nil {
			if _, ok := opt.Seed.DomainFor(source); ok {
				continue
			}
		}
		unmatched = append(unmatched, source)
	}
	if len(unmatched) > 0 {
		fmt.Fprintf(stderr, "gkg preflight: %v: %d unmatched sources (up to 20): %s\n", graph.ErrSeedRequired, len(unmatched), strings.Join(unmatched[:min(20, len(unmatched))], ", "))
		return 3
	}
	return 10
}
