package ingest_test

import (
	"context"
	"crypto/sha256"
	"fmt"
	"maps"
	"strings"
	"testing"

	"github.com/alessiosavi/go-knowledge-graph/internal/ingest"
	"github.com/alessiosavi/go-knowledge-graph/internal/obs"
)

type markdownFixtureConverter string

func (md markdownFixtureConverter) Convert(context.Context, []byte, string, string) (string, error) {
	return string(md), nil
}

func TestDoclingTextCleanBeforeHashAndChunks(t *testing.T) {
	env := newPipelineEnv(t)
	env.cfg.Ingest.ChunkWords = 200
	env.cfg.Ingest.ChunkDedup = "off"
	ctx := t.Context()
	metrics := obs.New()
	source, _ := uniqueText(t)
	in := ingest.Input{Kind: "file", Source: source + ".pdf", Bytes: []byte("%PDF synthetic fixture")}
	raw := "- denti sui invisibile totalmente e trasparente è lacca La ظ\n\n" +
		"- \u202b ظ\u202cUna sola sequenza operativa, solo 3 strumenti\n\n" +
		"- polimerizzazione di modalità 3 ظ\n\n" +
		"Ent\x00ry " + strings.Repeat("�", 95) + " 26\n\nDetail \xff\xfe token."
	want := "- La lacca è trasparente e totalmente invisibile sui denti\n\n" +
		"- Una sola sequenza operativa, solo 3 strumenti\n\n" +
		"- 3 modalità di polimerizzazione\n\nEntry … 26\n\nDetail � token."

	submit := func(p *ingest.Pipeline, in ingest.Input) ingest.Result {
		t.Helper()
		res, err := p.Submit(ctx, in)
		if err != nil {
			t.Fatal(err)
		}
		return res
	}
	load := func(md string) *ingest.Pipeline {
		p := ingest.NewPipeline(env.cfg, env.st, env.mock, env.ix, ingest.DefaultLoader(nil, markdownFixtureConverter(md)))
		p.SetMetrics(metrics)
		return p
	}
	p := load(raw)
	res := submit(p, in)
	t.Cleanup(func() {
		if err := env.st.DeleteDocument(context.Background(), res.DocumentID); err != nil {
			t.Error(err)
		}
	})

	assertDocument := func(stats map[string]int64) {
		t.Helper()
		row, err := env.st.RunReadQuery(ctx, `
MATCH (d:Document {id: $id})
RETURN d.hash AS hash, d.text_clean_version AS version, d.text_invalid_utf8 AS invalid_utf8,
       d.text_controls AS controls, d.text_bidi_controls AS bidi_controls, d.text_glyph_lines AS glyph_lines,
       d.text_leader_runs AS leader_runs,
       d.text_fffd_removed AS fffd_removed, d.text_fffd_remaining AS fffd_remaining`, map[string]any{"id": res.DocumentID}, 0)
		if err != nil || len(row.Rows) != 1 {
			t.Fatalf("query document: %+v, %v", row, err)
		}
		wantProps := map[string]any{"hash": fmt.Sprintf("%x", sha256.Sum256([]byte(want))), "version": int64(1)}
		for step, count := range stats {
			wantProps[step] = count
		}
		if !maps.Equal(row.Rows[0], wantProps) {
			t.Errorf("Document properties = %v, want %v", row.Rows[0], wantProps)
		}
	}
	assertDocument(map[string]int64{
		"invalid_utf8": 1, "controls": 1, "bidi_controls": 2, "glyph_lines": 3,
		"leader_runs": 1, "fffd_removed": 95, "fffd_remaining": 1,
	})
	if duplicate := submit(p, in); !duplicate.Noop || duplicate.DocumentID != res.DocumentID {
		t.Errorf("duplicate = %+v, want no-op for %s", duplicate, res.DocumentID)
	}
	// A new pipeline instance has an empty memory cache and reads the cleaned
	// markdown from the same spool. The fixture converter would return
	// different content if called again.
	if err := load("unexpected conversion").Process(ctx, res.DocumentID); err != nil {
		t.Fatal(err)
	}
	chunks, err := env.st.Chunks(ctx, res.DocumentID)
	if err != nil {
		t.Fatal(err)
	}
	if len(chunks) != 1 || chunks[0].Text != want {
		t.Errorf("stored chunks = %+v, want one chunk with %q", chunks, want)
	}

	in.Force = true
	if forced := submit(load(want), in); forced.Noop || forced.DocumentID != res.DocumentID {
		t.Errorf("forced submit = %+v, want new admission for %s", forced, res.DocumentID)
	}
	assertDocument(map[string]int64{
		"invalid_utf8": 0, "controls": 0, "bidi_controls": 0, "glyph_lines": 0,
		"leader_runs": 0, "fffd_removed": 0, "fffd_remaining": 1,
	})
	scrape := gather(t, metrics)
	for _, metric := range []string{
		`{step="invalid_utf8"} 1`, `{step="controls"} 1`, `{step="bidi_controls"} 2`,
		`{step="glyph_lines"} 3`, `{step="leader_runs"} 1`,
		`{step="fffd_removed"} 95`, `{step="fffd_remaining"} 2`,
	} {
		if !strings.Contains(scrape, "gkg_text_clean_total"+metric+"\n") {
			t.Errorf("metrics missing gkg_text_clean_total%s", metric)
		}
	}
}
