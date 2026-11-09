// Command gkg is the single binary for the go-knowledge-graph service:
// REST + MCP server, the ingestion pipeline and the evaluation harness.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/alessiosavi/GoGPUtils/minhash"

	"github.com/alessiosavi/go-knowledge-graph/internal/api"
	"github.com/alessiosavi/go-knowledge-graph/internal/config"
	"github.com/alessiosavi/go-knowledge-graph/internal/dedup"
	"github.com/alessiosavi/go-knowledge-graph/internal/embed"
	"github.com/alessiosavi/go-knowledge-graph/internal/eval"
	"github.com/alessiosavi/go-knowledge-graph/internal/extract"
	"github.com/alessiosavi/go-knowledge-graph/internal/graph"
	"github.com/alessiosavi/go-knowledge-graph/internal/ingest"
	"github.com/alessiosavi/go-knowledge-graph/internal/judge"
	mcpserver "github.com/alessiosavi/go-knowledge-graph/internal/mcp"
	"github.com/alessiosavi/go-knowledge-graph/internal/obs"
	"github.com/alessiosavi/go-knowledge-graph/internal/search"
)

const version = "v0.0.0-dev"

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

func run(args []string, stdout, stderr io.Writer) int {
	// Structured JSON logs for the whole process (stderr).
	slog.SetDefault(obs.Log())
	if len(args) == 0 {
		usage(stderr)
		return 2
	}
	switch args[0] {
	case "version":
		fmt.Fprintf(stdout, "gkg %s\n", version)
		return 0
	case "help", "-h", "--help":
		usage(stdout)
		return 0
	case "migrate":
		return runMigrate(stdout, stderr)
	case "eval":
		return runEval(args[1:], stdout, stderr)
	case "serve":
		return runServe(args[1:], stdout, stderr)
	case "ingest":
		return runIngest(args[1:], stdout, stderr)
	case "pdf-extract":
		// Hidden subcommand: the isolated PDF parser (parent kills on
		// timeout; see ingest.PDFToText).
		return ingest.PDFExtract(os.Stdin, stdout, stderr)
	case "mcp":
		return runMCP(args[1:], stdout, stderr)
	default:
		fmt.Fprintf(stderr, "gkg: unknown command %q\n\n", args[0])
		usage(stderr)
		return 2
	}
}

// runEval runs the golden-set evaluation harness.
func runEval(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("eval", flag.ContinueOnError)
	fs.SetOutput(stderr)
	golden := fs.String("golden", "eval/golden/sample.jsonl", "golden JSONL path")
	providerName := fs.String("provider", "", "extractor provider: mock|openai|sidecar (default: config extractor mode)")
	out := fs.String("out", "", "write the report to <out>.md and <out>.json")
	sweep := fs.String("sweep", "", "threshold sweep, e.g. entity=0.2:0.8:0.1 relation=0.1:0.6:0.1")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *sweep != "" {
		if _, _, err := eval.ParseSweep(*sweep); err != nil {
			fmt.Fprintf(stderr, "gkg eval: %v\n", err)
			return 2 // usage error
		}
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	cfg, err := config.Load("")
	if err != nil {
		fmt.Fprintf(stderr, "gkg eval: %v\n", err)
		return 1
	}
	name := *providerName
	if name == "" {
		name = cfg.Extractor.Mode
	}
	prov, err := providerFromConfig(cfg, name)
	if err != nil {
		fmt.Fprintf(stderr, "gkg eval: %v\n", err)
		return 2
	}
	var factJudge ingest.FactJudge
	if mode := cfg.Judge.Mode; mode != "" && mode != "off" {
		factJudge = judge.NewClient(cfg)
	}

	report, err := eval.Run(ctx, cfg, prov, factJudge, *golden, *sweep, *out)
	if err != nil {
		fmt.Fprintf(stderr, "gkg eval: %v\n", err)
		return 1
	}
	fmt.Fprintf(stdout, "entities F1 (lenient): %.3f · relations F1 (lenient): %.3f · judge precision: %.3f · reference coverage: %.3f\n",
		report.AggregateEntities.F1, report.AggregateRelations.F1, report.JudgePrecision, report.ReferenceCoverage)
	if *out != "" {
		fmt.Fprintf(stdout, "report: %s.md %s.json\n", *out, *out)
	}
	return 0
}

// runMigrate opens Memgraph and bootstraps the schema, idempotently.
func runMigrate(stdout, stderr io.Writer) int {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	cfg, err := config.Load("")
	if err != nil {
		fmt.Fprintf(stderr, "gkg migrate: %v\n", err)
		return 1
	}
	st, err := graph.Open(ctx, cfg)
	if err != nil {
		fmt.Fprintf(stderr, "gkg migrate: %v\n", err)
		return 1
	}
	defer func() { _ = st.Close(ctx) }()
	if err := st.Bootstrap(ctx); err != nil {
		fmt.Fprintf(stderr, "gkg migrate: %v\n", err)
		return 1
	}
	fmt.Fprintln(stdout, "schema ok")
	return 0
}

// serveWorkers is the ingestion worker pool size.
const serveWorkers = 4

// providerFromConfig builds the extractor provider by name; an empty name
// selects the configured default.
func providerFromConfig(cfg config.Config, name string) (extract.Provider, error) {
	if name == "" {
		name = cfg.Extractor.Mode
	}
	switch name {
	case "mock":
		return &extract.Mock{}, nil
	case "openai":
		return extract.NewOpenAI(cfg), nil
	case "sidecar":
		return extract.NewSidecar(cfg), nil
	default:
		return nil, fmt.Errorf("unknown provider %q", name)
	}
}

// ingestFetcher builds the SSRF-guarded fetcher from the ingest config.
func ingestFetcher(cfg config.Config) *ingest.Fetcher {
	return &ingest.Fetcher{
		MaxBytes:     int64(cfg.Ingest.MaxURLMB) << 20,
		MaxRedirects: cfg.Ingest.MaxRedirects,
		TimeoutS:     cfg.Ingest.FetchTimeoutS,
	}
}

// seedDedupIndex loads persisted chunk fingerprints into the in-memory
// candidate index, skipping stale signature formats.
func seedDedupIndex(ix *dedup.Index, fps []graph.ChunkFP) {
	for _, fp := range fps {
		if fp.FP.Version != minhash.SignatureVersion || len(fp.FP.Sig) != dedup.SignatureLength {
			continue
		}
		_ = ix.Add(fp.ChunkID, fp.FP)
	}
}

// buildPipeline wires the extractor, the re-seeded dedup index, the
// optional judge and embedder and the text loader on an open store; the
// returned pipeline is ready for submissions. serve, mcp and ingest share
// it so a document is treated identically whichever entry point submits
// it. The returned embedder (nil when disabled) is also for the search
// endpoints' vector leg.
func buildPipeline(ctx context.Context, cfg config.Config, st *graph.Store) (*ingest.Pipeline, search.Embedder, error) {
	prov, err := providerFromConfig(cfg, "")
	if err != nil {
		return nil, nil, err
	}
	ix := dedup.New()
	fps, err := st.AllFingerprints(ctx)
	if err != nil {
		return nil, nil, err
	}
	seedDedupIndex(ix, fps)
	pipe := ingest.NewPipeline(cfg, st, prov, ix, ingest.DefaultLoader(ingestFetcher(cfg)))
	if mode := cfg.Judge.Mode; mode != "" && mode != "off" {
		pipe.SetJudge(judge.NewClient(cfg))
	}
	var emb search.Embedder
	if cfg.Embedder.Enabled {
		emb = embed.NewOpenAI(cfg)
		pipe.SetEmbedder(emb)
	}
	return pipe, emb, nil
}

// runServe runs the REST API and the ingestion worker pool until the
// process is signalled.
func runServe(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	fs.SetOutput(stderr)
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() > 0 {
		fmt.Fprintf(stderr, "gkg serve: unexpected argument %q\n", fs.Arg(0))
		return 2
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	cfg, err := config.Load("")
	if err != nil {
		fmt.Fprintf(stderr, "gkg serve: %v\n", err)
		return 1
	}
	st, err := graph.Open(ctx, cfg)
	if err != nil {
		fmt.Fprintf(stderr, "gkg serve: %v\n", err)
		return 1
	}
	defer func() { _ = st.Close(context.Background()) }()
	if err := st.Bootstrap(ctx); err != nil {
		fmt.Fprintf(stderr, "gkg serve: %v\n", err)
		return 1
	}
	pipe, emb, err := buildPipeline(ctx, cfg, st)
	if err != nil {
		fmt.Fprintf(stderr, "gkg serve: %v\n", err)
		return 1
	}
	metrics := obs.New()
	pipe.SetMetrics(metrics)
	workers := &ingest.Worker{Workers: serveWorkers, Pipeline: pipe}
	workers.Start(ctx)
	if err := pipe.Recover(ctx); err != nil {
		fmt.Fprintf(stderr, "gkg serve: %v\n", err)
		return 1
	}

	rest := api.NewServer(cfg, st, pipe)
	mcpSrv := mcpserver.NewServer(cfg, st, pipe)
	rest.SetEmbedder(emb)
	mcpSrv.SetEmbedder(emb)
	rest.SetMetrics(metrics)
	rest.SetReady(func(ctx context.Context) error { return st.Ping(ctx) })
	rest.Mount("/mcp", mcpSrv.HTTPHandler())
	srv := &http.Server{
		Addr:              cfg.Server.Addr,
		Handler:           rest.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
	}
	errCh := make(chan error, 1)
	go func() { errCh <- srv.ListenAndServe() }()
	fmt.Fprintf(stdout, "gkg: listening on %s\n", cfg.Server.Addr)

	select {
	case <-ctx.Done():
	case err := <-errCh:
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			fmt.Fprintf(stderr, "gkg serve: %v\n", err)
			return 1
		}
	}
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_ = srv.Shutdown(shutdownCtx)
	return 0
}

// runIngest ingests one file, URL or stdin payload synchronously and
// prints the resulting document id and status.
func runIngest(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("ingest", flag.ContinueOnError)
	fs.SetOutput(stderr)
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() != 1 {
		fmt.Fprintln(stderr, "gkg ingest: exactly one <file|url|-> argument is required")
		return 2
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	cfg, err := config.Load("")
	if err != nil {
		fmt.Fprintf(stderr, "gkg ingest: %v\n", err)
		return 1
	}
	st, err := graph.Open(ctx, cfg)
	if err != nil {
		fmt.Fprintf(stderr, "gkg ingest: %v\n", err)
		return 1
	}
	defer func() { _ = st.Close(context.Background()) }()
	if err := st.Bootstrap(ctx); err != nil {
		fmt.Fprintf(stderr, "gkg ingest: %v\n", err)
		return 1
	}
	pipe, _, err := buildPipeline(ctx, cfg, st)
	if err != nil {
		fmt.Fprintf(stderr, "gkg ingest: %v\n", err)
		return 1
	}
	in, err := ingestArg(fs.Arg(0))
	if err != nil {
		fmt.Fprintf(stderr, "gkg ingest: %v\n", err)
		return 1
	}
	res, err := pipe.Submit(ctx, in)
	if err != nil {
		fmt.Fprintf(stderr, "gkg ingest: %v\n", err)
		return 1
	}
	if !res.Noop {
		if err := pipe.Process(ctx, res.DocumentID); err != nil {
			fmt.Fprintf(stderr, "gkg ingest: %v\n", err)
			return 1
		}
	}
	doc, err := st.Document(ctx, res.DocumentID)
	if err != nil {
		fmt.Fprintf(stderr, "gkg ingest: %v\n", err)
		return 1
	}
	fmt.Fprintf(stdout, "document_id=%s status=%s noop=%v\n", doc.ID, doc.Status, res.Noop)
	if doc.Status == graph.StatusFailed {
		fmt.Fprintf(stderr, "gkg ingest: %s\n", doc.Error)
		return 1
	}
	return 0
}

// runMCP runs the MCP server over stdio with the ingestion pipeline
// attached, until the client disconnects or the process is signalled.
func runMCP(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("mcp", flag.ContinueOnError)
	fs.SetOutput(stderr)
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() > 0 {
		fmt.Fprintf(stderr, "gkg mcp: unexpected argument %q\n", fs.Arg(0))
		return 2
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	cfg, err := config.Load("")
	if err != nil {
		fmt.Fprintf(stderr, "gkg mcp: %v\n", err)
		return 1
	}
	st, err := graph.Open(ctx, cfg)
	if err != nil {
		fmt.Fprintf(stderr, "gkg mcp: %v\n", err)
		return 1
	}
	defer func() { _ = st.Close(context.Background()) }()
	if err := st.Bootstrap(ctx); err != nil {
		fmt.Fprintf(stderr, "gkg mcp: %v\n", err)
		return 1
	}
	pipe, emb, err := buildPipeline(ctx, cfg, st)
	if err != nil {
		fmt.Fprintf(stderr, "gkg mcp: %v\n", err)
		return 1
	}
	workers := &ingest.Worker{Workers: serveWorkers, Pipeline: pipe}
	workers.Start(ctx)
	if err := pipe.Recover(ctx); err != nil {
		fmt.Fprintf(stderr, "gkg mcp: %v\n", err)
		return 1
	}
	srv := mcpserver.NewServer(cfg, st, pipe)
	srv.SetEmbedder(emb)
	if err := srv.RunStdio(ctx); err != nil && !errors.Is(err, context.Canceled) {
		fmt.Fprintf(stderr, "gkg mcp: %v\n", err)
		return 1
	}
	return 0
}

// ingestArg maps the command-line argument to an ingest input: "-" reads
// stdin as text, an http(s) URL is fetched by the pipeline, anything else
// is a file path.
func ingestArg(arg string) (ingest.Input, error) {
	switch {
	case arg == "-":
		data, err := io.ReadAll(os.Stdin)
		if err != nil {
			return ingest.Input{}, fmt.Errorf("read stdin: %w", err)
		}
		return ingest.Input{Kind: "text", Text: string(data)}, nil
	case strings.HasPrefix(arg, "http://") || strings.HasPrefix(arg, "https://"):
		return ingest.Input{Kind: "url", URL: arg}, nil
	default:
		data, err := os.ReadFile(arg)
		if err != nil {
			return ingest.Input{}, fmt.Errorf("read %s: %w", arg, err)
		}
		return ingest.Input{Kind: "file", Source: filepath.Base(arg), Bytes: data}, nil
	}
}

func usage(w io.Writer) {
	fmt.Fprint(w, `usage: gkg <command> [flags]

commands:
  serve     run the REST API, MCP endpoint and ingestion workers
  ingest    ingest a file, URL or stdin into the knowledge graph
  eval      run the golden-set evaluation harness
  migrate   bootstrap the Memgraph schema
  version   print the version
  mcp       run the MCP server over stdio
`)
}
