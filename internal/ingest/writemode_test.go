package ingest

import (
	"testing"

	"github.com/alessiosavi/go-knowledge-graph/internal/graph"
)

func TestWriteModeOptions(t *testing.T) {
	pipe := &Pipeline{}
	doc := &graph.Document{ID: "zorbo", Domain: "a"}
	if got := pipe.WriteMode(); got != (WriteMode{}) {
		t.Fatalf("default WriteMode = %+v; want zero value", got)
	}
	for _, mode := range []WriteMode{
		{Hygiene: true, Domains: true, RequireConfirmation: true},
		{Hygiene: true, Domains: true},
		{},
	} {
		pipe.SetWriteMode(mode)
		if got := pipe.WriteMode(); got != mode {
			t.Errorf("WriteMode = %+v; want %+v", got, mode)
		}
		want := graph.WriteOptions{FactsAsCandidates: mode.RequireConfirmation}
		if mode.Domains {
			want.Domain = "a"
		}
		if got := pipe.writeOptions(doc); got != want {
			t.Errorf("writeOptions = %+v; want %+v", got, want)
		}
	}
}
