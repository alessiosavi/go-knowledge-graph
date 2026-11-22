package agent

import (
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/alessiosavi/go-knowledge-graph/internal/argerr"
	"github.com/alessiosavi/go-knowledge-graph/internal/config"
	"github.com/alessiosavi/go-knowledge-graph/internal/graph/graphtest"
)

func TestDateWindowAgentTools(t *testing.T) {
	f := graphtest.DatedEvidence(t)
	s := NewServer(config.Config{}, f.Store, nil)
	ctx := withDomain(t.Context(), f.Domain)
	for _, tool := range []struct {
		name, field, idKey string
		args               map[string]string
	}{
		{"search", "hits", "document_id", map[string]string{"query": f.Domain, "kind": "chunk"}},
		{"get_entity", "facts", "doc_id", map[string]string{"id": f.EntityID}},
		{"traverse", "edges", "doc_id", map[string]string{"entity": f.EntityID}},
		{"list_documents", "documents", "id", nil},
	} {
		t.Run(tool.name, func(t *testing.T) {
			for _, tc := range []struct {
				name, since, until, wantID, wantError string
			}{
				{name: "since", since: "2026-03-01", wantID: f.NewID},
				{name: "until", until: "2024-03-01", wantID: f.OldID},
				{name: "both", since: "2024-03-01T18:00:00+01:00", until: "2024-03-01", wantID: f.OldID},
				{name: "bad_since", since: "yesterday", wantError: "since must be YYYY-MM-DD"},
				{name: "bad_until", until: "yesterday", wantError: "until must be YYYY-MM-DD"},
				{name: "inverted", since: "2026-03-01", until: "2024-03-01", wantError: "since must be at or before until"},
			} {
				t.Run(tc.name, func(t *testing.T) {
					args := map[string]string{"since": tc.since, "until": tc.until}
					for k, v := range tool.args {
						args[k] = v
					}
					raw, err := json.Marshal(args)
					if err != nil {
						t.Fatal(err)
					}
					result, err := s.execTool(ctx, tool.name, raw)
					if tc.wantError != "" {
						var badArg argerr.Error
						if !errors.As(err, &badArg) || !strings.Contains(err.Error(), tc.wantError) {
							t.Fatalf("error = %v, want argerr containing %q", err, tc.wantError)
						}
						return
					}
					if err != nil {
						t.Fatal(err)
					}
					out := unmarshalResult[map[string]json.RawMessage](t, result)
					var rows []map[string]any
					if err := json.Unmarshal(out[tool.field], &rows); err != nil {
						t.Fatal(err)
					}
					if len(rows) != 1 || rows[0][tool.idKey] != tc.wantID {
						t.Fatalf("%s = %v, want one row from %s", tool.field, rows, tc.wantID)
					}
				})
			}
		})
	}
}

func TestDateWindowAgentToolSchemas(t *testing.T) {
	defs := toolDefs()
	for _, name := range []string{"search", "get_entity", "traverse", "list_documents"} {
		t.Run(name, func(t *testing.T) {
			i := slices.IndexFunc(defs, func(d toolDef) bool { return d.Function.Name == name })
			if i < 0 {
				t.Fatal("missing tool")
			}
			params := defs[i].Function.Parameters
			properties := params["properties"].(map[string]any)
			required, _ := params["required"].([]string)
			for _, bound := range []string{"since", "until"} {
				property, _ := properties[bound].(map[string]any)
				if property["type"] != "string" || slices.Contains(required, bound) {
					t.Errorf("%s = %v, required = %v, want an optional string", bound, property, required)
				}
			}
		})
	}
}
