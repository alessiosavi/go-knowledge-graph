package ingest_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/alessiosavi/go-knowledge-graph/internal/ingest"
)

type titleFixtureConverter struct{}

func (titleFixtureConverter) Convert(context.Context, []byte, string, string) (string, error) {
	return "# **Converted Fixture**\n\nFixture paragraph.", nil
}

func TestPipelinePersistsTitleAndSource(t *testing.T) {
	env := newPipelineEnv(t)
	ctx := context.Background()
	p := ingest.NewPipeline(env.cfg, env.st, env.mock, env.ix, ingest.DefaultLoader(nil, titleFixtureConverter{}))
	for _, tt := range []struct {
		name          string
		in            ingest.Input
		title, origin string
	}{
		{"frontmatter", ingest.Input{Kind: "text", Text: "---\ntitle: Fixture Metadata\n---\n# Other Fixture"}, "Fixture Metadata", "frontmatter"},
		{"file-h1", ingest.Input{Kind: "file", Bytes: []byte("# **Fixture** À-head\n\nFixture paragraph.")}, "Fixture À-head", "h1"},
		{"caller-before-h1", ingest.Input{Kind: "text", Title: "  Caller\rFixture – Révision  ", Text: "# Fixture Heading"}, "Caller Fixture – Révision", "caller"},
		{"caller-before-loader", ingest.Input{Kind: "file", Title: "Caller HTML Fixture", Bytes: []byte("<html><head><title>HTML Fixture</title></head><body>Fixture paragraph</body></html>")}, "Caller HTML Fixture", "caller"},
		{"html", ingest.Input{Kind: "file", Bytes: []byte("<html><head><title>HTML Fixture</title></head><body>Fixture paragraph</body></html>")}, "HTML Fixture", "html"},
		{"docling-markdown", ingest.Input{Kind: "file", Bytes: []byte("%PDF fixture routed to the stub converter")}, "Converted Fixture", "h1"},
		{"unset", ingest.Input{Kind: "text", Text: "**Fixture paragraph.**"}, "", ""},
	} {
		t.Run(tt.name, func(t *testing.T) {
			tt.in.Source = fmt.Sprintf("fixture/%d/index.md#%s", time.Now().UnixNano(), tt.name)
			result, err := p.Submit(ctx, tt.in)
			if err != nil || result.Noop {
				t.Fatalf("Submit = %+v, %v", result, err)
			}
			t.Cleanup(func() {
				if err := env.st.DeleteDocument(ctx, result.DocumentID); err != nil {
					t.Error(err)
				}
			})
			doc, err := env.st.Document(ctx, result.DocumentID)
			if err != nil {
				t.Fatal(err)
			}
			if doc.Title != tt.title || doc.TitleSource != tt.origin {
				t.Fatalf("stored title = (%q, %q), want (%q, %q)", doc.Title, doc.TitleSource, tt.title, tt.origin)
			}
		})
	}
}

func TestPipelineTitleWithSynthesizedSource(t *testing.T) {
	env := newPipelineEnv(t)
	ctx := context.Background()
	for _, tt := range []struct {
		name, kind, text, title, origin string
	}{
		{"explicit text", "text", "Fixture without a heading.", "", ""},
		{"inferred text", "", "Another fixture without a heading.", "", ""},
		{"heading", "text", "# Fixture Heading\n\nFixture paragraph.", "Fixture Heading", "h1"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			result, err := env.p.Submit(ctx, ingest.Input{Kind: tt.kind, Text: tt.text})
			if err != nil || result.Noop {
				t.Fatalf("Submit = %+v, %v", result, err)
			}
			t.Cleanup(func() {
				if err := env.st.DeleteDocument(ctx, result.DocumentID); err != nil {
					t.Error(err)
				}
			})
			doc, err := env.st.Document(ctx, result.DocumentID)
			if err != nil {
				t.Fatal(err)
			}
			if len(doc.Hash) < 8 || doc.Source != doc.Hash[:8] {
				t.Fatalf("source = %q, want prefix of hash %q", doc.Source, doc.Hash)
			}
			if doc.Title != tt.title || doc.TitleSource != tt.origin {
				t.Fatalf("stored title = (%q, %q), want (%q, %q)", doc.Title, doc.TitleSource, tt.title, tt.origin)
			}
		})
	}
}
