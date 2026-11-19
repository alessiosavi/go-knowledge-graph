package graph_test

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/alessiosavi/go-knowledge-graph/internal/graph/graphtest"
)

func TestPageDocumentTitleSeedsReconstruction(t *testing.T) {
	st := graphtest.Store(t)
	uri := os.Getenv(graphtest.URIEnv)
	const scope = "title-seeds-fixture"
	const lead = "Fixture prose."
	const heading = "# Fixture Heading"
	runePrefix := strings.Repeat("è", 4095) + "x" // 8191 bytes, with one byte left before the cap.
	t.Cleanup(func() {
		rawRun(t, uri, `MATCH (d:Document {domain:'title-seeds-fixture'})-[:HAS_CHUNK]->(c:Chunk) DETACH DELETE c, d`)
	})
	rawRun(t, uri, fmt.Sprintf(`
CREATE (g:Document {id:'title-seed-a-gap', domain:%q}),
       (g)-[:HAS_CHUNK]->(:Chunk {id:'title-seed-gap-1', seq:1, start:%d, text:%q}),
       (g)-[:HAS_CHUNK]->(:Chunk {id:'title-seed-gap-0', seq:0, start:0, text:%q}),
       (r:Document {id:'title-seed-b-rune', domain:%q}),
       (r)-[:HAS_CHUNK]->(:Chunk {id:'title-seed-rune-0', seq:0, start:0, text:%q})`,
		scope, len(lead)+6, heading, lead, scope, runePrefix+"界"))
	page, err := st.PageDocumentTitleSeeds(context.Background(), "", scope, 2)
	if err != nil || len(page) != 2 {
		t.Fatalf("PageDocumentTitleSeeds returned %d seeds, %v, want 2 seeds", len(page), err)
	}
	t.Run("gap capped at two newlines", func(t *testing.T) {
		if got, want := page[0].Text, lead+"\n\n"+heading; got != want {
			t.Fatalf("prefix = %q, want %q", got, want)
		}
	})
	t.Run("trim at rune boundary", func(t *testing.T) {
		if got := page[1].Text; got != runePrefix {
			t.Fatalf("prefix length = %d, valid UTF-8 = %t, want %d bytes of complete runes", len(got), utf8.ValidString(got), len(runePrefix))
		}
	})
}
