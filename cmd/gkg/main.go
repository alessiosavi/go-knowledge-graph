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
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/alessiosavi/GoGPUtils/minhash"

	"github.com/alessiosavi/go-knowledge-graph/internal/agent"
	"github.com/alessiosavi/go-knowledge-graph/internal/api"
	"github.com/alessiosavi/go-knowledge-graph/internal/config"
	"github.com/alessiosavi/go-knowledge-graph/internal/convert"
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
	labels := fs.String("labels", "", "label preset from config labels.presets_dir (default: built-in labels)")
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

	cfg, err := loadConfig()
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

	report, err := eval.Run(ctx, cfg, prov, factJudge, *golden, *labels, *sweep, *out)
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

// runMigrate upgrades the schema and synchronously finishes in-flight work.
func runMigrate(stdout, stderr io.Writer) int {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	cfg, err := loadConfig()
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
	if _, err := st.MigrateExtractionSchema(ctx); err != nil {
		fmt.Fprintf(stderr, "gkg migrate: %v\n", err)
		return 1
	}
	fmt.Fprintln(stdout, "schema ok")
	pipe, _, err := buildPipeline(ctx, cfg, st)
	if err != nil {
		fmt.Fprintf(stderr, "gkg migrate: %v; start \"gkg serve\" with a usable extractor to finish the re-extraction of queued documents\n", err)
		return 1
	}
	docs, err := st.InFlight(ctx)
	if err != nil {
		fmt.Fprintf(stderr, "gkg migrate: %v\n", err)
		return 1
	}
	processed, failed := 0, 0
	for _, doc := range docs {
		if err := pipe.Process(ctx, doc.ID); err != nil {
			failed++
			fmt.Fprintf(stderr, "gkg migrate: document %s: %v\n", doc.ID, err)
			// Process records failures as terminal. Keep this recovery work
			// in flight so serve (or a later migrate) can finish after the
			// extractor/configuration is repaired, using the stored chunks.
			resume := doc.Status
			if resume == graph.StatusReceived || resume == graph.StatusFetched {
				chunks, err := st.Chunks(ctx, doc.ID)
				if err != nil {
					fmt.Fprintf(stderr, "gkg migrate: could not inspect recovery chunks for %s: %v\n", doc.ID, err)
				} else if len(chunks) > 0 {
					// Chunking may have completed and removed the spool before
					// extraction failed; resume from those durable chunks.
					resume = graph.StatusChunked
				}
			}
			if err := st.SetStatus(ctx, doc.ID, resume, ""); err != nil {
				fmt.Fprintf(stderr, "gkg migrate: could not requeue document %s: %v\n", doc.ID, err)
			}
			continue
		}
		processed++
	}
	fmt.Fprintf(stdout, "re-extraction: processed=%d failed=%d\n", processed, failed)
	if failed > 0 {
		fmt.Fprintln(stderr, "gkg migrate: start \"gkg serve\" with a usable extractor to finish the re-extraction of queued documents")
		return 1
	}
	return 0
}

// loadConfig resolves the configuration: GKG_CONFIG names a YAML file, a
// config.yaml in the working directory is picked up when present, and
// environment overrides always win (secrets stay environment-only).
func loadConfig() (config.Config, error) {
	path := os.Getenv("GKG_CONFIG")
	if path == "" {
		if _, err := os.Stat("config.yaml"); err == nil {
			path = "config.yaml"
		}
	}
	return config.Load(path)
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

// allowedFurlsModels is the embeddings service's model allowlist: anything
// else 400s every call, so a non-allowlisted model fails at startup.
var allowedFurlsModels = map[string]bool{"gemini-embedding-2": true}

// validateFurl rejects a missing or non-http(s) Function URL, naming the
// environment variable to fix (the raw value is never echoed: the URL is a
// capability).
func validateFurl(name, raw string) error {
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return fmt.Errorf("embedder furls: %s must be a valid http(s) URL", name)
	}
	return nil
}

// embedderFromConfig builds the embedding provider the configuration
// selects: the OpenAI-compatible default, or the asymmetric furls client
// after validating its env-provided URLs, shared secret and model. Every
// validation error names the environment variable to fix.
func embedderFromConfig(cfg config.Config) (embed.Provider, error) {
	switch cfg.Embedder.Provider {
	case "", "openai":
		return embed.NewOpenAI(cfg), nil
	case "furls":
		if err := validateFurl("EMBED_PRODUCT_FURL", cfg.Embedder.ProductFURL); err != nil {
			return nil, err
		}
		if err := validateFurl("EMBED_QUERY_FURL", cfg.Embedder.QueryFURL); err != nil {
			return nil, err
		}
		if cfg.Embedder.APIKey == "" {
			return nil, errors.New("embedder furls: EMBED_API_KEY must be set (keep it in .env; never commit it)")
		}
		if m := cfg.Embedder.Model; m != "" && !allowedFurlsModels[m] {
			return nil, fmt.Errorf("embedder furls: GKG_EMBEDDER_MODEL %q is not allowed by the embeddings service (allowed: gemini-embedding-2)", m)
		}
		return embed.NewFurls(cfg), nil
	default:
		return nil, fmt.Errorf("embedder: unknown provider %q (want openai or furls)", cfg.Embedder.Provider)
	}
}

// converterFromConfig builds the optional high-quality document converter:
// nil when off, a validated docling client when configured. Every validation
// error names the environment variable to fix.
func converterFromConfig(cfg config.Config) (convert.Converter, error) {
	switch cfg.Converter.Mode {
	case "", "off":
		return nil, nil
	case "docling":
		u, err := url.Parse(cfg.Converter.URL)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			return nil, errors.New("converter docling: GKG_CONVERTER_URL must be a valid http(s) URL")
		}
		return convert.ImagePolicy(convert.New(cfg), cfg.Converter.Images, int64(cfg.Converter.MaxImageMP*1e6)), nil
	default:
		return nil, fmt.Errorf("converter: unknown mode %q (want off or docling)", cfg.Converter.Mode)
	}
}

// buildPipeline wires the extractor, the re-seeded dedup index, the
// optional judge and embedder and the text loader on an open store; the
// returned pipeline is ready for submissions. serve, mcp and ingest share
// it so a document is treated identically whichever entry point submits
// it. The returned embedder (nil interface when disabled) is also for the
// search endpoints' vector query leg, and records the embedding-space
// marker (warn-only) when it is enabled.
func buildPipeline(ctx context.Context, cfg config.Config, st *graph.Store) (*ingest.Pipeline, embed.Provider, error) {
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
	conv, err := converterFromConfig(cfg)
	if err != nil {
		return nil, nil, err
	}
	pipe := ingest.NewPipeline(cfg, st, prov, ix, ingest.DefaultLoader(ingestFetcher(cfg), conv))
	if mode := cfg.Judge.Mode; mode != "" && mode != "off" {
		pipe.SetJudge(judge.NewClient(cfg))
	}
	var emb embed.Provider
	if cfg.Embedder.Enabled {
		emb, err = embedderFromConfig(cfg)
		if err != nil {
			return nil, nil, err
		}
		// Warn-only space tripwire: two providers can share a dimension,
		// so the store cannot detect a source switch — a mismatch means
		// the stored vectors must be cleared and re-ingested first.
		if prev, serr := st.SetEmbeddingSpace(ctx, emb.Space()); serr != nil {
			slog.Warn("embedder: could not record the embedding-space marker", "err", serr)
		} else if prev != "" && prev != emb.Space() {
			slog.Warn("embedder: embedding space changed; stored vectors belong to a different space — clear Entity.embedding and re-ingest (see docs/known-limitations.md)",
				"previous", prev, "current", emb.Space())
		}
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

	cfg, err := loadConfig()
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
	if _, err := st.MigrateExtractionSchema(ctx); err != nil {
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

	rest := newAPIServer(cfg, st, pipe, emb, metrics)
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

// newAPIServer is the HTTP wiring used by serve and the OpenAPI drift guard.
// Keeping every extra mount here lets the guard inspect the real route table
// without opening Memgraph or starting the ingestion workers.
func newAPIServer(cfg config.Config, st *graph.Store, pipe api.Submitter, emb search.QueryEmbedder, metrics *obs.Metrics) *api.Server {
	rest := api.NewServer(cfg, st, pipe)
	mcpSrv := mcpserver.NewServer(cfg, st, pipe)
	agentSrv := agent.NewServer(cfg, st, emb)
	rest.SetEmbedder(emb)
	mcpSrv.SetEmbedder(emb)
	rest.SetMetrics(metrics)
	rest.SetReady(func(ctx context.Context) error { return st.Ping(ctx) })
	rest.Mount("/mcp", mcpSrv.HTTPHandler())
	rest.Mount("POST /v1/agent/ask", agentSrv.AskHandler())
	rest.Mount("POST /v1/agent/ask/stream", agentSrv.StreamHandler())
	rest.Mount("GET /v1/agent/status", agentSrv.StatusHandler())
	return rest
}

// runIngest ingests one file, URL or stdin payload synchronously and
// prints the resulting document id and status.
func runIngest(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("ingest", flag.ContinueOnError)
	fs.SetOutput(stderr)
	source := fs.String("source", "", "identity override (default: file basename / URL / content hash)")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() != 1 {
		fmt.Fprintln(stderr, "gkg ingest: exactly one <file|url|-> argument is required")
		return 2
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	cfg, err := loadConfig()
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
	current, err := st.ExtractionSchemaCurrent(ctx)
	if err != nil {
		fmt.Fprintf(stderr, "gkg ingest: %v\n", err)
		return 1
	}
	if !current {
		fmt.Fprintln(stderr, `gkg ingest: graph extraction schema is outdated; run "gkg migrate" (re-extracts all documents) or start "gkg serve" first`)
		return 1
	}
	pipe, _, err := buildPipeline(ctx, cfg, st)
	if err != nil {
		fmt.Fprintf(stderr, "gkg ingest: %v\n", err)
		return 1
	}
	in, err := ingestArg(fs.Arg(0), *source)
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

	cfg, err := loadConfig()
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
	if _, err := st.MigrateExtractionSchema(ctx); err != nil {
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
// is a file path. An explicit source overrides the derived identity for
// every kind — the knob for "a renamed file must supersede its old
// version".
func ingestArg(arg, source string) (ingest.Input, error) {
	switch {
	case arg == "-":
		data, err := io.ReadAll(os.Stdin)
		if err != nil {
			return ingest.Input{}, fmt.Errorf("read stdin: %w", err)
		}
		return ingest.Input{Kind: "text", Text: string(data), Source: source}, nil
	case strings.HasPrefix(arg, "http://") || strings.HasPrefix(arg, "https://"):
		return ingest.Input{Kind: "url", URL: arg, Source: source}, nil
	default:
		data, err := os.ReadFile(arg)
		if err != nil {
			return ingest.Input{}, fmt.Errorf("read %s: %w", arg, err)
		}
		if source == "" {
			source = filepath.Base(arg)
		}
		return ingest.Input{Kind: "file", Source: source, Bytes: data}, nil
	}
}

func usage(w io.Writer) {
	fmt.Fprint(w, `usage: gkg <command> [flags]

commands:
  serve     run the REST API, MCP endpoint and ingestion workers
  ingest    ingest a file, URL or stdin into the knowledge graph (-source <s> overrides the identity)
  eval      run the golden-set evaluation harness
  migrate   upgrade the Memgraph schema and re-extract in-flight documents
  version   print the version
  mcp       run the MCP server over stdio
`)
}
