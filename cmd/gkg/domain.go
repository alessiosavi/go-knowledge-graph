package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/alessiosavi/go-knowledge-graph/internal/domain"
	"github.com/alessiosavi/go-knowledge-graph/internal/graph"
)

func runDomain(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprintln(stderr, "usage: gkg domain <list|show|evaluate|rename|move|merge|undo> [args] [--server URL] [--op-id ID]")
		return 2
	}
	action := args[0]
	if action == "help" || action == "--help" || action == "-h" {
		fmt.Fprintln(stdout, "usage: gkg domain <list|show|evaluate|rename|move|merge|undo> [args] [--server URL] [--op-id ID]")
		return 0
	}
	nargs, ok := map[string]int{"list": 0, "show": 1, "evaluate": 0, "rename": 2, "move": 2, "merge": 2, "undo": 1}[action]
	if !ok {
		fmt.Fprintf(stderr, "gkg domain: unknown command %q\n", action)
		return 2
	}
	fs := flag.NewFlagSet("domain "+action, flag.ContinueOnError)
	fs.SetOutput(stderr)
	server := fs.String("server", "", "serve URL (default: loopback and configured server port)")
	opID := fs.String("op-id", "", "resume a move/merge or identify an undo operation")
	// Go's flag parser stops at the first positional argument; accept the usual
	// CLI spelling with flags either before or after the operation's arguments.
	flags, pos := []string{}, []string{}
	for i := 1; i < len(args); i++ {
		a := args[i]
		if a == "--" {
			pos = append(pos, args[i+1:]...)
			break
		}
		if strings.HasPrefix(a, "-") {
			flags = append(flags, a)
			if (a == "--server" || a == "-server" || a == "--op-id" || a == "-op-id") && i+1 < len(args) {
				i++
				flags = append(flags, args[i])
			}
		} else {
			pos = append(pos, a)
		}
	}
	if err := fs.Parse(flags); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if *opID != "" && action != "move" && action != "merge" && action != "undo" {
		fmt.Fprintf(stderr, "gkg domain: --op-id is not valid for %s\n", action)
		return 2
	}
	if action == "undo" && *opID != "" {
		if len(pos) != 0 {
			fmt.Fprintln(stderr, "gkg domain: undo accepts either --op-id or a positional operation ID")
			return 2
		}
		pos = append(pos, *opID)
	}
	if len(pos) != nargs {
		fmt.Fprintf(stderr, "gkg domain: %s needs %d arguments\n", action, nargs)
		return 2
	}
	cfg, err := loadConfig()
	if err != nil {
		fmt.Fprintf(stderr, "gkg domain: %v\n", err)
		return 1
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	fail := func(err error) int { fmt.Fprintf(stderr, "gkg domain: %v\n", err); return 1 }
	switch action {
	case "rename", "move", "merge", "undo":
		if *server == "" {
			_, port, err := net.SplitHostPort(cfg.Server.Addr)
			if err != nil {
				return fail(err)
			}
			*server = "http://127.0.0.1:" + port
		}
		base, err := url.Parse(*server)
		if err != nil || base.Host == "" || (base.Scheme != "http" && base.Scheme != "https") {
			return fail(fmt.Errorf("invalid server URL %q", *server))
		}
		body := map[string]any{}
		switch action {
		case "rename":
			body["domain"], body["name"] = pos[0], pos[1]
		case "move":
			body["document_ids"], body["domain"] = []string{pos[0]}, pos[1]
		case "merge":
			body["from"], body["into"] = pos[0], pos[1]
		case "undo":
			body["op_id"] = pos[0]
		}
		if *opID != "" {
			body["op_id"] = *opID
		}
		data, err := json.Marshal(body)
		if err != nil {
			return fail(err)
		}
		endpoint := strings.TrimRight(*server, "/") + "/v1/admin/domains/" + action
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(data))
		if err != nil {
			return fail(err)
		}
		req.Header.Set("Content-Type", "application/json")
		if cfg.Server.APIKey != "" {
			req.Header.Set("Authorization", "Bearer "+cfg.Server.APIKey)
		}
		client := &http.Client{Timeout: 2 * time.Minute, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
		resp, err := client.Do(req)
		if err != nil {
			return fail(fmt.Errorf("serve unreachable at %s: %w", *server, err))
		}
		defer resp.Body.Close()
		data, err = io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		if err != nil {
			return fail(err)
		}
		if resp.StatusCode != http.StatusAccepted {
			return fail(fmt.Errorf("serve returned %s: %s", resp.Status, strings.TrimSpace(string(data))))
		}
		if _, err := fmt.Fprintln(stdout, strings.TrimSpace(string(data))); err != nil {
			return fail(err)
		}
		return 0
	}
	st, err := graph.Open(ctx, cfg)
	if err != nil {
		return fail(err)
	}
	defer st.Close(context.WithoutCancel(ctx))
	// Read-only commands never bootstrap or migrate the graph.
	profiles, err := st.DomainProfiles(ctx, "")
	if err != nil {
		return fail(err)
	}
	if action == "evaluate" {
		result := domain.Evaluate(profiles, domain.ParamsFromConfig(cfg.Domain.Tau, cfg.Domain.Delta, cfg.Domain.TauOverride))
		_, err = fmt.Fprintf(stdout, "docs=%d top_choice_accuracy=%.6f auto_attach_share=%.6f unseen_false_attach_share=%.6f\n", result.Docs, result.TopChoiceAccuracy, result.AutoAttachShare, result.UnseenFalseAttachShare)
		if err != nil {
			return fail(err)
		}
		return 0
	}
	domains, err := st.ListDomains(ctx)
	if err != nil {
		return fail(err)
	}
	if action == "list" {
		if _, err = fmt.Fprintln(stdout, "DOMAIN\tNAME\tDOCUMENTS\tNEAREST\tSCORE"); err != nil {
			return fail(err)
		}
		for _, d := range domains {
			nearest, score := nearestDomain(d.ID, profiles)
			if _, err = fmt.Fprintf(stdout, "%s\t%s\t%d\t%s\t%.6f\n", d.ID, d.Name, d.Documents, nearest, score); err != nil {
				return fail(err)
			}
		}
		return 0
	}
	found := false
	for _, d := range domains {
		if d.ID == pos[0] {
			found = true
			if err := json.NewEncoder(stdout).Encode(d); err != nil {
				return fail(err)
			}
		}
	}
	if !found {
		return fail(graph.ErrNotFound)
	}
	records, err := st.DomainDecisions(ctx, "")
	if err != nil {
		return fail(err)
	}
	for _, record := range records {
		if record.Domain == pos[0] || record.PreviousDomain == pos[0] {
			if err := json.NewEncoder(stdout).Encode(record); err != nil {
				return fail(err)
			}
		}
	}
	return 0
}

// Average the existing top-three document scores over the source partition.
func nearestDomain(slug string, profiles []domain.DocProfile) (string, float64) {
	var source, others []domain.DocProfile
	for _, p := range profiles {
		if p.Domain == slug {
			source = append(source, p)
		} else {
			others = append(others, p)
		}
	}
	scores := map[string]float64{}
	for _, p := range source {
		for _, score := range domain.Score(p.Profile, others) {
			scores[score.Domain] += score.Score
		}
	}
	nearest := ""
	var best float64
	for id, sum := range scores {
		score := sum / float64(len(source))
		if nearest == "" || score > best || (score == best && id < nearest) {
			nearest, best = id, score
		}
	}
	return nearest, best
}
