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
	"strconv"
	"strings"
	"syscall"

	"github.com/alessiosavi/go-knowledge-graph/internal/agent"
	"github.com/alessiosavi/go-knowledge-graph/internal/graph"
	"github.com/alessiosavi/go-knowledge-graph/internal/reteval"
	"github.com/alessiosavi/go-knowledge-graph/internal/search"
)

func runEvalRetrieval(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("eval-retrieval", flag.ContinueOnError)
	fs.SetOutput(stderr)
	questions := fs.String("questions", "", "schema-v1 question JSON file (required)")
	agentLevel := fs.Bool("agent", false, "run the configured agent (spends provider money)")
	replay := fs.String("replay", "", "re-score agent answers from a stored JSON report")
	kList := fs.String("k", "5,10,20", "comma-separated positive search cutoffs")
	out := fs.String("out", "", "write JSON report to this file")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if fs.NArg() != 0 || *questions == "" {
		fmt.Fprintln(stderr, "gkg eval-retrieval: --questions is required; positional arguments are not supported")
		return 2
	}
	if *agentLevel && *replay != "" {
		fmt.Fprintln(stderr, "gkg eval-retrieval: --agent and --replay cannot be used together")
		return 2
	}
	var ks []int
	for _, part := range strings.Split(*kList, ",") {
		k, err := strconv.Atoi(strings.TrimSpace(part))
		if err != nil || k <= 0 {
			fmt.Fprintln(stderr, "gkg eval-retrieval: --k must contain positive integers")
			return 2
		}
		ks = append(ks, k)
	}
	cfg, err := loadConfig()
	if err != nil {
		fmt.Fprintf(stderr, "gkg eval-retrieval: %v\n", err)
		return 1
	}
	if *agentLevel && !cfg.Agent.Enabled {
		fmt.Fprintln(stderr, "gkg eval-retrieval: agent is not enabled (agent.enabled=false)")
		return 2
	}
	if *agentLevel {
		fmt.Fprintln(stderr, "warning: --agent calls the configured agent provider and spends money")
	}
	set, err := reteval.Load(*questions)
	if err != nil {
		fmt.Fprintf(stderr, "gkg eval-retrieval: %v\n", err)
		return 2
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	st, err := graph.Open(ctx, cfg)
	if err != nil {
		fmt.Fprintf(stderr, "gkg eval-retrieval: %v\n", err)
		return 1
	}
	defer st.Close(ctx)
	var emb search.QueryEmbedder
	if cfg.Embedder.Enabled {
		emb, err = embedderFromConfig(cfg)
		if err != nil {
			fmt.Fprintf(stderr, "gkg eval-retrieval: %v\n", err)
			return 1
		}
	}
	d := reteval.Deps{Store: st, Embedder: emb}
	if *agentLevel {
		d.Agent = agent.NewServer(cfg, st, emb)
	}
	r, err := reteval.Run(ctx, d, set, reteval.Options{Agent: *agentLevel, ReplayPath: *replay, Ks: ks})
	runErr := err
	if err != nil {
		fmt.Fprintf(stderr, "gkg eval-retrieval: %v\n", err)
		if r.Schema == 0 {
			return 1
		}
	}
	if len(r.Failed) > 0 {
		fmt.Fprintf(stderr, "gkg eval-retrieval: %d failed question(s)\n", len(r.Failed))
	}
	if *out != "" {
		b, err := json.MarshalIndent(r, "", "  ")
		if err == nil {
			err = os.WriteFile(*out, append(b, '\n'), 0600)
		}
		if err != nil {
			fmt.Fprintf(stderr, "gkg eval-retrieval: write report: %v\n", err)
			return 1
		}
	}
	_, err = io.WriteString(stdout, reteval.Markdown(r))
	if err != nil {
		fmt.Fprintf(stderr, "gkg eval-retrieval: summary: %v\n", err)
		return 1
	}
	if runErr != nil || len(r.Failed) > 0 {
		return 1
	}
	return 0
}
