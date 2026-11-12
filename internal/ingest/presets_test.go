package ingest

import (
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestLoadPresetRejectsAmbiguousText(t *testing.T) {
	for _, kind := range []string{"entities", "relations"} {
		for _, tc := range []struct {
			name, labels, first, second string
		}{
			{"same text", "  - name: depends_on\n    text: uses\n  - name: relies_on\n    text: uses\n", "depends_on", "relies_on"},
			{"normalized text", "  - name: depends_on\n    text: ' Uses '\n  - name: relies_on\n    text: USES\n", "depends_on", "relies_on"},
			{"text matches name", "  - name: depends_on\n    text: ' PART_OF '\n  - name: part_of\n", "depends_on", "part_of"},
			{"name matches text", "  - name: part_of\n  - name: depends_on\n    text: part_of\n", "part_of", "depends_on"},
			{"names with text", "  - name: depends_on\n    text: depends on\n  - name: part_of\n    text: depends_on\n", "depends_on", "part_of"},
			{"same name", "  - name: uses\n  - name: uses\n", "uses", "uses"},
		} {
			t.Run(kind+"/"+tc.name, func(t *testing.T) {
				dir := t.TempDir()
				if err := os.WriteFile(filepath.Join(dir, "custom.yaml"), []byte(kind+":\n"+tc.labels), 0o600); err != nil {
					t.Fatal(err)
				}
				_, _, err := LoadPreset(dir, "custom")
				if err == nil || !strings.Contains(err.Error(), tc.first) || !strings.Contains(err.Error(), tc.second) {
					t.Fatalf("LoadPreset error = %v, want both %q and %q", err, tc.first, tc.second)
				}
			})
		}
	}
}

func TestLoadPresetEmptyIsDefault(t *testing.T) {
	entities, relations, err := LoadPreset(filepath.Join(t.TempDir(), "missing"), "")
	if err != nil {
		t.Fatal(err)
	}
	wantEntities, wantRelations := DefaultLabels()
	if !reflect.DeepEqual(entities, wantEntities) || !reflect.DeepEqual(relations, wantRelations) {
		t.Fatalf("defaults = %+v / %+v, want %+v / %+v", entities, relations, wantEntities, wantRelations)
	}
	entities[0].Name = "changed"
	relations[0].Name = "changed"
	actualEntities, actualRelations := DefaultLabels()
	if !reflect.DeepEqual(actualEntities, wantEntities) || !reflect.DeepEqual(actualRelations, wantRelations) {
		t.Error("loaded defaults alias the built-in labels")
	}
}

func TestLoadPresetAllowsMappingKeysAcrossKinds(t *testing.T) {
	dir := t.TempDir()
	preset := "entities:\n  - name: service\n    text: uses\nrelations:\n  - name: uses\n    text: uses\n"
	if err := os.WriteFile(filepath.Join(dir, "custom.yaml"), []byte(preset), 0o600); err != nil {
		t.Fatal(err)
	}
	entities, relations, err := LoadPreset(dir, "custom")
	if err != nil {
		t.Fatal(err)
	}
	if len(entities) != 1 || entities[0].Text != "uses" || len(relations) != 1 || relations[0].Text != "uses" {
		t.Fatalf("labels = %+v / %+v", entities, relations)
	}
}

func TestLoadPresetPreservesValidationErrors(t *testing.T) {
	dir := t.TempDir()
	for _, tc := range []struct{ name, preset, want string }{
		{"../escape", "", `label preset "../escape": invalid name`},
		{"badname", "entities:\n  - name: '!!!'\n", `label preset "badname": label "!!!" has no alphanumeric content`},
		{"badscope", "entities:\n  - name: date\n    scope: global\n", `label preset "badscope": label "date" has unknown scope "global"`},
		{"rel_scope", "relations:\n  - name: due_on\n    scope: document\n", `label preset "rel_scope": relation label "due_on" cannot have a scope (scope applies to entity labels)`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if tc.preset != "" {
				if err := os.WriteFile(filepath.Join(dir, tc.name+".yaml"), []byte(tc.preset), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			_, _, err := LoadPreset(dir, tc.name)
			if err == nil || err.Error() != tc.want {
				t.Fatalf("LoadPreset error = %v, want %s", err, tc.want)
			}
		})
	}
	for _, name := range []string{"missing", "broken"} {
		t.Run(name, func(t *testing.T) {
			if name == "broken" {
				if err := os.WriteFile(filepath.Join(dir, name+".yaml"), []byte("entities: ["), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			_, _, err := LoadPreset(dir, name)
			if err == nil || !strings.HasPrefix(err.Error(), fmt.Sprintf("label preset %q:", name)) {
				t.Fatalf("LoadPreset error = %v, want preset name", err)
			}
		})
	}
}
