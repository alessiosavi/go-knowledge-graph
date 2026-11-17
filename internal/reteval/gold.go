package reteval

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"github.com/alessiosavi/go-knowledge-graph/internal/graph"
)

type resolvedGold struct {
	anchors, items [][]string
	crossDomain    []string
	invalid        string
}

func resolveGold(ctx context.Context, st *graph.Store, q Question) (resolvedGold, error) {
	var g resolvedGold
	resolve := func(a Anchor) ([]string, error) {
		// Read all domains so out-of-scope matches can be reported as well.
		hits, err := st.ChunksContaining(ctx, a.Quote, "")
		if err != nil {
			return nil, err
		}
		ids := []string{}
		for _, h := range hits {
			if !strings.HasSuffix(h.Source, a.Source) {
				continue
			}
			if q.Domain != "" && h.Domain != q.Domain {
				g.crossDomain = append(g.crossDomain, h.ChunkID)
				continue
			}
			ids = append(ids, h.ChunkID)
		}
		if len(ids) == 0 {
			g.invalid = fmt.Sprintf("anchor resolves nowhere: source=%q quote=%q", a.Source, a.Quote)
		}
		return ids, nil
	}
	for _, a := range q.Gold {
		ids, err := resolve(a)
		if err != nil {
			return g, err
		}
		g.anchors = append(g.anchors, ids)
	}
	for _, item := range q.AnswerItems {
		var itemIDs []string
		for _, a := range item.Evidence {
			ids, err := resolve(a)
			if err != nil {
				return g, err
			}
			g.anchors = append(g.anchors, ids)
			itemIDs = append(itemIDs, ids...)
		}
		g.items = append(g.items, itemIDs)
	}
	slices.Sort(g.crossDomain)
	g.crossDomain = slices.Compact(g.crossDomain)
	return g, nil
}
