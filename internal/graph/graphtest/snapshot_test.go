package graphtest

import (
	"reflect"
	"testing"

	"github.com/neo4j/neo4j-go-driver/v6/neo4j"
)

func TestNormalizeIndexRows(t *testing.T) {
	index := func(kind, label string, property any, count int64) *neo4j.Record {
		return &neo4j.Record{
			Keys:   []string{"index type", "label", "property", "count"},
			Values: []any{kind, label, property, count},
		}
	}
	label := index("label", "Document", nil, 5000)
	property := index("label+property", "Document", []any{"id"}, 5000)
	before := normalizeSchemaRows(t, []*neo4j.Record{label, property}, "count")
	after := normalizeSchemaRows(t, []*neo4j.Record{
		index("label+property", "Document", []any{"id"}, 1),
		index("label", "Document", nil, 1),
	}, "count")
	if !reflect.DeepEqual(before, after) {
		t.Errorf("count or row order changed the snapshot: %v != %v", before, after)
	}

	for _, tc := range []struct {
		name string
		rows []*neo4j.Record
	}{
		{"added", []*neo4j.Record{label, property, index("label", "Chunk", nil, 0)}},
		{"dropped", []*neo4j.Record{label}},
		{"type changed", []*neo4j.Record{label, index("label_text (name: doc)", "Document", []any{"id"}, 5000)}},
		{"label changed", []*neo4j.Record{label, index("label+property", "Chunk", []any{"id"}, 5000)}},
		{"properties changed", []*neo4j.Record{label, index("label+property", "Document", []any{"id", "source"}, 5000)}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := normalizeSchemaRows(t, tc.rows, "count"); reflect.DeepEqual(before, got) {
				t.Errorf("index change was missed: %v", got)
			}
		})
	}
}
