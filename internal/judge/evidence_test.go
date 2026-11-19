package judge_test

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/alessiosavi/go-knowledge-graph/internal/graph"
	"github.com/alessiosavi/go-knowledge-graph/internal/judge"
)

type v2Payload struct {
	Fact struct {
		Head string
		Rel  string
		Tail string
	}
	RelationDefinition string  `json:"relation_definition"`
	HeadMentionType    string  `json:"head_mention_type"`
	TailMentionType    string  `json:"tail_mention_type"`
	HeadTypeFallback   bool    `json:"head_type_fallback"`
	TailTypeFallback   bool    `json:"tail_type_fallback"`
	Evidence           string  `json:"evidence"`
	EvidenceTruncated  bool    `json:"evidence_truncated"`
	TableHeader        *string `json:"table_header"`
	DocTitle           string  `json:"doc_title"`
}

func v2Input(t *testing.T, text, head, tail string) judge.V2Input {
	t.Helper()
	mention := func(surface string) judge.EvidenceMention {
		start := strings.Index(text, surface)
		if start == -1 {
			t.Fatalf("surface %q missing from fixture", surface)
		}
		return judge.EvidenceMention{Start: start, End: start + len(surface)}
	}
	return judge.V2Input{ChunkText: text, Head: mention(head), Tail: mention(tail), Rel: "uses"}
}

func buildV2(t *testing.T, in judge.V2Input) (v2Payload, []byte) {
	t.Helper()
	b, reason := judge.BuildV2Request(in)
	if reason != "" {
		t.Fatalf("request is unverifiable: %s", reason)
	}
	var out v2Payload
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatal(err)
	}
	return out, b
}

func TestV2RequestFrozenBytesAndKeys(t *testing.T) {
	const definition = "the head employs the tail"
	const prefix = `{"fact":{"head":"Nebula","rel":"uses","tail":"Nova"},"relation_definition":"the head employs the tail","head_mention_type":"service","tail_mention_type":"technology","head_type_fallback":false,"tail_type_fallback":true,`
	const suffix = `,"doc_title":"Nebula guide"}`
	const row = "| Nebula | Nova |"
	profile := judge.NewV2Profile("frozen-fixture-model", map[string]map[string]string{"": {"uses": definition}})
	for _, tc := range []struct {
		name, text, previous, bytes, key string
	}{
		{"table without header", row, "",
			prefix + `"evidence":"| Nebula | Nova |","table_header":null` + suffix,
			"51529249d33d731514b89fef31adc3f705f823971e6589794d0c5aeada8e167b"},
		{"table with previous header", row, "| Product | API |\n| --- | --- |\n| Orion | Vega |\n",
			prefix + `"evidence":"| Nebula | Nova |","table_header":"| Product | API |\n| --- | --- |"` + suffix,
			"c0f6453d1b26f776c9ceb5e37aac2bae50a40d6b348024ee76b3a567d8ac9fc7"},
		{"prose", "Intro sentence. Nebula uses Nova. Closing sentence.", "",
			prefix + `"evidence":"Nebula uses Nova.","table_header":null` + suffix,
			"26d8cd6a2e2bda75da7ec17570bc2e7794fa12018080aa09117a8b8fb64fbc5c"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			in := v2Input(t, tc.text, "Nebula", "Nova")
			in.PreviousChunkText = tc.previous
			in.Head.MentionType = "service"
			in.Tail.MentionType, in.Tail.TypeFallback = "technology", true
			in.Definition, in.DocTitle = definition, "Nebula guide"
			_, payload := buildV2(t, in)
			if string(payload) != tc.bytes {
				t.Errorf("request bytes = %s, want %s", payload, tc.bytes)
			}
			if key := judge.RequestKeyV2(payload, profile.Fingerprint); key != tc.key {
				t.Errorf("request key = %s, want %s", key, tc.key)
			}
		})
	}
}

func TestV2RequestUsesSurfacesAndMentionTypes(t *testing.T) {
	in := v2Input(t, "Intro sentence. **Nebula** uses a C++ shim at nebula.dev.", "**Nebula**", "C++")
	in.Head.MentionType = "service"
	in.Tail.MentionType, in.Tail.TypeFallback = "technology", true
	in.Definition, in.DocTitle = "the head itself employs the tail", "Nebula guide"
	got, b := buildV2(t, in)
	if got.Fact.Head != "Nebula" || got.Fact.Tail != "C++" || got.Fact.Rel != "uses" {
		t.Errorf("fact = %+v", got.Fact)
	}
	if scoped := graph.ScopedEntityKey("doc-id", "Nebula"); strings.Contains(string(b), scoped) {
		t.Errorf("scoped key leaked into request: %s", b)
	}
	if got.HeadMentionType != "service" || got.TailMentionType != "technology" || got.HeadTypeFallback || !got.TailTypeFallback {
		t.Errorf("mention metadata = %+v", got)
	}
	if got.RelationDefinition != in.Definition || got.DocTitle != in.DocTitle {
		t.Errorf("definition or title changed: %+v", got)
	}
	in.Head.MentionType = "project" // Same entity, another occurrence's type.
	got, _ = buildV2(t, in)
	if got.Fact.Head != "Nebula" || got.HeadMentionType != "project" {
		t.Errorf("request did not preserve the other occurrence's type: %+v", got)
	}
}

func TestV2RequestOverridesHeadSurface(t *testing.T) {
	const header = "| Material | Notes | API |\n| --- | --- | --- |"
	for _, tc := range []struct {
		name, text, previous string
	}{
		{"prose", "Intro sentence. Nebula uses Nova. Closing sentence.", ""},
		{"table with previous header", "| Nebula | " + strings.Repeat("界", 300) + " | Nova |", header + "\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			in := v2Input(t, tc.text, "Nebula", "Nova")
			in.PreviousChunkText = tc.previous
			in.Head.MentionType = "material"
			in.Tail.MentionType = "technology"
			want, _ := buildV2(t, in)
			surface := "**Quill Pro**"
			in.HeadSurface = &surface
			in.Head.MentionType = "product"
			want.Fact.Head, want.HeadMentionType, want.HeadTypeFallback = "**Quill Pro**", "product", true
			got, _ := buildV2(t, in)
			if !reflect.DeepEqual(got, want) {
				t.Errorf("payload = %+v, want %+v", got, want)
			}
		})
	}
}

func TestV2RequestRejectsInvalidHeadSurface(t *testing.T) {
	for _, tc := range []struct{ name, surface string }{
		{"empty", ""},
		{"whitespace only", " \t\u00a0"},
		{"untrimmed", " Quill Pro "},
	} {
		t.Run(tc.name, func(t *testing.T) {
			in := v2Input(t, "Nebula uses Nova.", "Nebula", "Nova")
			in.HeadSurface = &tc.surface
			if b, reason := judge.BuildV2Request(in); b != nil || reason != "invalid head_surface" {
				t.Errorf("request = %s, reason = %q, want nil and invalid head_surface", b, reason)
			}
		})
	}
}

func TestV2ProseEvidence(t *testing.T) {
	for _, tc := range []struct {
		name, text, head, tail, want string
	}{
		{"same sentence", "Other evidence. Nebula runs Go 1.27 on nebula.dev. Ignore this.", "Nebula", "Go", "Nebula runs Go 1.27 on nebula.dev."},
		{"different sentences", "Other evidence. Nebula reaches a gateway. Ignore this. The gateway calls Nova.", "Nebula", "Nova", "Nebula reaches a gateway. … The gateway calls Nova."},
		{"document order", "Nova hosts a registry. Nebula reads the registry.", "Nebula", "Nova", "Nova hosts a registry. … Nebula reads the registry."},
		{"decimal and domain between mentions", "Other evidence. Nebula 3.14 on nebula.dev calls Nova. Ignore this.", "Nebula", "Nova", "Nebula 3.14 on nebula.dev calls Nova."},
		{"wrapped sentence", "Earlier evidence. Nebula calls\nNova. Later evidence.", "Nebula", "Nova", "Nebula calls\nNova."},
		{"operator across lines", "Quill uses the a | b operator.\nNova reads the docs.", "Quill", "Nova", "Quill uses the a | b operator. … Nova reads the docs."},
		{"operator on same line", "Introduction. Quill uses the a | b operator and Nova.", "Quill", "Nova", "Quill uses the a | b operator and Nova."},
		{"shell pipeline", "Run this pipeline: go run ./cmd | tee out. Quill uses Nova.", "Quill", "Nova", "Quill uses Nova."},
		{"escaped and unescaped pipe", "Earlier text. Quill uses \\| and a | b with Nova.", "Quill", "Nova", "Quill uses \\| and a | b with Nova."},
		{"two pipes with only one non-empty cell", "Earlier text. Quill uses Nova. ||", "Quill", "Nova", "Quill uses Nova."},
		{"mentions on different rows", "| Quill | toolkit |\n| Other | Nova |", "Quill", "Nova", "| Quill | toolkit |\n| Other | Nova |"},
		{"one unescaped pipe without leading pipe", "Product | API\n--- | ---\nQuill | Nova", "Quill", "Nova", "Product | API\n--- | ---\nQuill | Nova"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, _ := buildV2(t, v2Input(t, tc.text, tc.head, tc.tail))
			if got.Evidence != tc.want || got.TableHeader != nil || got.EvidenceTruncated {
				t.Errorf("evidence = %q, header = %v, truncated = %t, want %q, null, false", got.Evidence, got.TableHeader, got.EvidenceTruncated, tc.want)
			}
		})
	}
}

func TestV2EvidenceBudget(t *testing.T) {
	padding := strings.Repeat("界", 300)
	for _, tc := range []struct {
		text, head, tail string
		different        bool
	}{
		{padding + " Nebula uses Nova " + padding + ".", "Nebula", "Nova", false},
		{padding + " Nebula " + padding + ". " + padding + " Nova " + padding + ".", "Nebula", "Nova", true},
		{"Nebula uses " + strings.Repeat("界", 140) + ". Nova " + padding + ".", strings.Repeat("界", 140), "Nova", true},
	} {
		got, _ := buildV2(t, v2Input(t, tc.text, tc.head, tc.tail))
		if len(got.Evidence) > 800 || !utf8.ValidString(got.Evidence) || strings.ContainsRune(got.Evidence, utf8.RuneError) ||
			!strings.Contains(got.Evidence, tc.head) || !strings.Contains(got.Evidence, tc.tail) ||
			strings.Contains(got.Evidence, " … ") != tc.different {
			t.Errorf("unexpected evidence (%d bytes): %q", len(got.Evidence), got.Evidence)
		}
	}
}

func TestV2TableEvidence(t *testing.T) {
	const header = "| Product | API |"
	const separator = "| :--- | ---: |"
	const row = "| Nebula v1.2 | Nova \\| toolkit |"
	const earlier = "| Person | Company |\n| --- | --- |\n| Ada | Acme |"
	for _, tc := range []struct {
		name, previous, beforeRow, dataRow, wantHeader string
	}{
		{"same chunk", "", header + "\n" + separator + "\n| Orion | Vega |\n", row, header + "\n" + separator},
		{"escaped header pipe", "", "| Product \\| Edition | API |\n" + separator + "\n", row, "| Product \\| Edition | API |\n" + separator},
		{"no header", "", "", row, ""},
		{"two tables", "", earlier + "\n\n" + header + "\n" + separator + "\n", row, header + "\n" + separator},
		{"through preceding chunk", header + "\n" + separator + "\n| Orion | Vega |\n", "| Aster | Atlas |\n", row, header + "\n" + separator},
		{"split header and separator", header, separator + "\n", row, header + "\n" + separator},
		{"noncontiguous current header", "", header + "\n" + separator + "\n\n", row, ""},
		{"noncontiguous previous header", header + "\n" + separator + "\n\n", "", row, ""},
		{"mismatched separator", "", header + "\n| --- | --- | --- |\n", row, ""},
		{"optional outer pipes", "", "Product | API | Service\n--- | --- | ---\n", "Nebula v1.2 | Nova | Atlas", "Product | API | Service\n--- | --- | ---"},
		{"indented leading pipe", "", "Product | API\n--- | ---\n", " \t| Nebula v1.2 | Nova", "Product | API\n--- | ---"},
		{"shell pipeline breaks table block", "", header + "\n" + separator + "\ngo run ./cmd | tee out\n", row, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			in := v2Input(t, tc.beforeRow+tc.dataRow+"\nUnrelated prose.", "Nebula", "Nova")
			in.PreviousChunkText = tc.previous
			got, b := buildV2(t, in)
			if got.Evidence != tc.dataRow || got.EvidenceTruncated {
				t.Errorf("evidence = %q, truncated = %t, want complete row %q", got.Evidence, got.EvidenceTruncated, tc.dataRow)
			}
			if tc.wantHeader == "" {
				if got.TableHeader != nil || !strings.Contains(string(b), `"table_header":null`) {
					t.Errorf("expected null table header: %s", b)
				}
			} else if got.TableHeader == nil || *got.TableHeader != tc.wantHeader {
				t.Errorf("header should be %q: %s", tc.wantHeader, b)
			}
		})
	}
}

func TestV2TruncatedTableEvidence(t *testing.T) {
	const header = "| Product | Notes | API |\n| --- | --- | --- |"
	for _, tc := range []struct {
		name, row string
		gap       bool
	}{
		{"nearby mentions", "| Quill | Nova | " + strings.Repeat("界", 300) + " |", false},
		{"review probe", "| Quill | " + strings.Repeat("y", 2000) + " | Nova |", true},
		{"distant multibyte mentions", "| Quill | " + strings.Repeat("界", 300) + " | Nova |", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, _ := buildV2(t, v2Input(t, header+"\n"+tc.row, "Quill", "Nova"))
			if len(got.Evidence) > 800 || !utf8.ValidString(got.Evidence) || strings.ContainsRune(got.Evidence, utf8.RuneError) ||
				!strings.Contains(got.Evidence, "Quill") || !strings.Contains(got.Evidence, "Nova") ||
				strings.Contains(got.Evidence, " … ") != tc.gap || !got.EvidenceTruncated {
				t.Errorf("unexpected evidence (%d bytes, truncated = %t): %q", len(got.Evidence), got.EvidenceTruncated, got.Evidence)
			}
			if got.TableHeader == nil || *got.TableHeader != header {
				t.Errorf("trimmed row lost its table header: %+v", got)
			}
		})
	}
}

func TestV2UnverifiableEvidence(t *testing.T) {
	in := v2Input(t, "étoile uses Nova.", "étoile", "Nova")
	for _, tc := range []struct {
		name string
		edit func(*judge.V2Input)
	}{
		{"missing text", func(in *judge.V2Input) { in.ChunkText = "" }},
		{"zero span", func(in *judge.V2Input) { in.Head = judge.EvidenceMention{} }},
		{"negative span", func(in *judge.V2Input) { in.Head.Start = -1 }},
		{"backward span", func(in *judge.V2Input) { in.Head.End = in.Head.Start - 1 }},
		{"past end", func(in *judge.V2Input) { in.Tail.End += 2 }},
		{"unaligned start", func(in *judge.V2Input) { in.Head.Start = 1 }},
		{"unaligned end", func(in *judge.V2Input) { in.Head.End = 1 }},
		{"invalid utf8", func(in *judge.V2Input) { in.ChunkText = "\xff" + in.ChunkText[1:] }},
		{"whitespace span", func(in *judge.V2Input) { in.Head.Start, in.Head.End = len("étoile"), len("étoile ") }},
		{"same sentence endpoints exceed budget", func(in *judge.V2Input) {
			*in = v2Input(t, "Nebula "+strings.Repeat("界", 300)+" Nova.", "Nebula", "Nova")
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			input := in
			tc.edit(&input)
			if b, reason := judge.BuildV2Request(input); len(b) != 0 || reason == "" {
				t.Errorf("expected unverifiable, got %s, reason %q", b, reason)
			}
		})
	}
}
