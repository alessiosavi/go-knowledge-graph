package graph_test

import (
	"errors"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/alessiosavi/go-knowledge-graph/internal/graph"
	"github.com/alessiosavi/go-knowledge-graph/internal/graph/graphtest"
)

func TestEntitySelectorPrecedenceAndFailClosed(t *testing.T) {
	st := graphtest.Store(t)
	ctx := t.Context()
	uri := os.Getenv(graphtest.URIEnv)
	tag := fmt.Sprintf("zelvix%d", time.Now().UnixNano())
	liveID, shadowKey := tag+"-live", tag+"-shadow"
	liveKey := graph.DomainKey("zelvix", tag)
	const scope = "zelvix"
	rawRun(t, uri, fmt.Sprintf(`CREATE (:Entity {id:%q,key:%q,name:'Zelvix',domain:%q}),
(:Entity {id:%q,key:%q,name:'Vorqun',domain:'vorqun'}),
(:Entity {id:%q,key:%q,name:'Nyrvox',domain:%q})`,
		liveID, liveKey, scope, tag+"-other", liveID, tag+"-shadow-id", shadowKey, scope))
	t.Cleanup(func() { rawRun(t, uri, fmt.Sprintf(`MATCH (e:Entity) WHERE e.id STARTS WITH %q DELETE e`, tag)) })
	seed := func(id, key, survivor string) { graphtest.SeedMergedEntity(t, id, key, survivor) }
	seed(tag+"-old", tag+"-old-key", liveID)
	seed(tag+"-shadow-id-ledger", liveID, tag+"-shadow-id")
	seed(shadowKey, tag+"-shadow-ledger", liveID)
	seed(tag+"-dangling", tag+"-dangling-key", liveKey) // A live key is invalid as a survivor ID.
	seed(tag+"-missing-target", tag+"-missing-target-key", tag+"-absent")
	seed(tag+"-loop-a", tag+"-loop-a-key", tag+"-loop-b")
	seed(tag+"-loop-b", tag+"-loop-b-key", tag+"-loop-a")
	seed(tag+"-chain", tag+"-chain-key", tag+"-old")
	seed(tag+"-collision", tag+"-collision-key", liveID)
	seed(tag+"-other-ledger", tag+"-collision", liveID)
	seed(tag+"-same", tag+"-same", liveID)

	for _, tc := range []struct {
		test, selector, domain, want string
		merged                       bool
	}{
		{"id beats live key and retired key", liveID, scope, liveID, false},
		{"wrong-domain id still beats key", liveID, "vorqun", "", false},
		{"exact key", liveKey, scope, liveID, false},
		{"live key beats ledger id", shadowKey, scope, tag + "-shadow-id", false},
		{"old id", tag + "-old", scope, liveID, true},
		{"old key", tag + "-old-key", "", liveID, true},
		{"one row matching both columns", tag + "-same", scope, liveID, true},
		{"conflicting ledger rows", tag + "-collision", scope, "", false},
		{"wrong-domain survivor", tag + "-old", "vorqun", "", false},
		{"missing survivor with matching live key", tag + "-dangling", scope, "", false},
		{"missing survivor", tag + "-missing-target", scope, "", false},
		{"loop", tag + "-loop-a", scope, "", false},
		{"second hop even with live final target", tag + "-chain-key", scope, "", false},
		{"unknown", tag + "-missing", scope, "", false},
	} {
		t.Run(tc.test, func(t *testing.T) {
			opt := graph.ViewOptions{Domain: tc.domain}
			view, viewErr := st.EntityViewOpts(ctx, tc.selector, opt)
			sub, traverseErr := st.TraverseOpts(ctx, tc.selector, "", "out", 1, 10, opt)
			if tc.want == "" {
				if !errors.Is(viewErr, graph.ErrNotFound) || !errors.Is(traverseErr, graph.ErrNotFound) {
					t.Fatalf("want ErrNotFound for both reads, got %v and %v", viewErr, traverseErr)
				}
				return
			}
			if viewErr != nil || view.ID != tc.want || traverseErr != nil || len(sub.Nodes) == 0 || sub.Nodes[0].ID != tc.want {
				t.Fatalf("want %q, got view %+v (%v), traversal %+v (%v)", tc.want, view, viewErr, sub, traverseErr)
			}
			want := graph.Provenance{}
			if tc.merged {
				want = graph.Provenance{Requested: tc.selector, ResolvedVia: "merged"}
			}
			if view.Requested != want.Requested || view.ResolvedVia != want.ResolvedVia ||
				sub.Requested != want.Requested || sub.ResolvedVia != want.ResolvedVia {
				t.Errorf("want provenance %+v, got view %q/%q, traversal %q/%q", want, view.Requested, view.ResolvedVia, sub.Requested, sub.ResolvedVia)
			}
		})
	}
}
