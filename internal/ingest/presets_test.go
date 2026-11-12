package ingest

import (
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"testing"

	"github.com/alessiosavi/go-knowledge-graph/internal/extract"
)

func TestShippedPresetsLoad(t *testing.T) {
	files, err := filepath.Glob("../../labels/*.yaml")
	if err != nil {
		t.Fatal(err)
	}
	want := map[string][2]string{
		"general":       {"person organization location product technology work event service project document", "works_at located_in part_of uses wrote depends_on owned_by member_of about"},
		"documentation": {"person team organization project service component technology data_store api environment config_setting document decision requirement", "part_of depends_on calls exposes reads_from writes_to deployed_to configured_by uses owned_by replaces documented_in decided requires"},
		"code":          {"repository service module function api data_store library config_setting test issue person", "part_of depends_on calls implements defines exposes reads_from writes_to tested_by configured_by fixes authored_by"},
		"mail":          {"person organization product service_offering invoice order contract amount date event campaign request project location", "works_for customer_of supplier_of partner_of issued_by billed_to amount_of due_on requests assigned_to attends held_at held_on promotes about"},
		"meeting":       {"person team organization project topic decision action_item risk date product", "attends decided assigned_to due_on about raised blocks depends_on follows_up owned_by"},
		"demo":          {"product feature organization persona scenario feedback issue metric competitor person", "demonstrates part_of requested_by feedback_on reported_by blocks competes_with measured_by uses"},
		"news":          {"person organization location event product", "works_at member_of located_in acquired announced"},
	}
	if len(files) != len(want) {
		t.Errorf("shipped presets = %d, want %d", len(files), len(want))
	}
	lowerSnake := regexp.MustCompile(`^[a-z][a-z0-9]*(?:_[a-z0-9]+)*$`)
	for _, file := range files {
		name := strings.TrimSuffix(filepath.Base(file), ".yaml")
		t.Run(name, func(t *testing.T) {
			entities, relations, err := LoadPreset("../../labels", name)
			if err != nil {
				t.Fatal(err)
			}
			seen := map[string]bool{}
			for kind, labels := range [][]extract.Label{entities, relations} {
				var names []string
				for _, label := range labels {
					names = append(names, label.Name)
					if seen[label.Name] || !lowerSnake.MatchString(label.Name) {
						t.Errorf("duplicate or non-snake label %q", label.Name)
					}
					seen[label.Name] = true
					wantScope := ""
					if kind == 0 && (label.Name == "amount" || label.Name == "date") {
						wantScope = "document"
					}
					if label.Scope != wantScope {
						t.Errorf("%s scope = %q, want %q", label.Name, label.Scope, wantScope)
					}
					wantText := ""
					if strings.Contains(label.Name, "_") {
						wantText = strings.ReplaceAll(label.Name, "_", " ")
					}
					if label.Text != wantText {
						t.Errorf("%s text = %q, want %q", label.Name, label.Text, wantText)
					}
				}
				if got := strings.Join(names, " "); got != want[name][kind] {
					t.Errorf("kind %d vocabulary = %q, want %q", kind, got, want[name][kind])
				}
			}
		})
	}
}

func TestGeneralPresetMatchesDefaults(t *testing.T) {
	entities, relations, err := LoadPreset("../../labels", "general")
	if err != nil {
		t.Fatal(err)
	}
	wantEntities, wantRelations := DefaultLabels()
	if !reflect.DeepEqual(entities, wantEntities) || !reflect.DeepEqual(relations, wantRelations) {
		t.Fatalf("general preset differs from defaults: %+v / %+v, want %+v / %+v", entities, relations, wantEntities, wantRelations)
	}
}

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
