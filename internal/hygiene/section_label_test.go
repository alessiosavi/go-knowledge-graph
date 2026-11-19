package hygiene

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/alessiosavi/go-knowledge-graph/internal/extract"
)

func TestSectionLabel(t *testing.T) {
	preset := Preset{Allow: map[string]bool{"Allowlisted": true}}
	for _, tt := range []struct {
		chunk, name, rule string
	}{
		{"## Part 3 — FE + API routes on Vercel", "Part 3", "heading_lead"},
		{"## Phase 2: Schema, API", "Phase 2", "heading_lead"},
		{"### Deployment architecture (live on AWS)", "Deployment architecture", "heading_lead"},
		{"- **Roster.** Giuseppe Gualtieri left...", "Roster.", "lead_in"},
		{"- **Roster.** Giuseppe Gualtieri left...", "Roster", "lead_in"},
		{"*Scope note (June 2026)*: this criterion gates...", "Scope note (June 2026)", "lead_in"},
		{"- **Submission**: Upload to Merchant Center", "Submission", "lead_in"},
		{"### 2.1 Deployment architecture (live)", "Deployment architecture", "heading_lead"},
		{"**Etiopia: Trekking nel Parco**", "Etiopia", "heading_lead"},
		{"### Déjà — Déploiement (live)", "Déjà", "heading_lead"},
		{"### Déjà — Déploiement (live)", "Déploiement", ""},
		{"- **Déjà — Déploiement**: details", "Déploiement", ""},
		{"   ### ***3. Déploiement***–détails", "Déploiement", "heading_lead"},
		{"Perché è così?\n• __Consegna__ — dettagli", "Consegna", "lead_in"},
		{"2) _Consegna:_ dettagli", "Consegna", "lead_in"},
		{"## Deployment. Next steps", "Deployment", "heading_lead"},
		{"- **FAB** chat/support nero bottom-right (Intercom).", "FAB", ""},
		{"- **Google Business Agent (new, announced around this date):** a business agent", "Google Business Agent", ""},
		{"**schema.org** does provide...", "schema.org", ""},
		{"## Vercel deployment notes", "Vercel", ""},
		{"## Part 3 — FE + API routes on Vercel", "Vercel", ""},
		{"This prose mentions Vercel: a platform", "Vercel", ""},
		{"| **Roster**: | People |", "Roster", ""},
		{"## Allowlisted: suite", "Allowlisted", ""},
		{"- **Allowlisted**: suite", "Allowlisted", ""},
		{"## Deployment architecture", "Deployment architecture", ""},
		{"    ## Part 3 — indented", "Part 3", ""},
		{"**Etiopia**: trekking **altrove**", "Etiopia", "lead_in"},
		{"**Etiopia: Trekking** and **Hiking**", "Etiopia", ""},
	} {
		t.Run(tt.chunk+"/"+tt.name, func(t *testing.T) {
			start := strings.Index(tt.chunk, tt.name)
			if got := SectionLabel(tt.chunk, start, start+len(tt.name), "topic", preset); got != tt.rule {
				t.Errorf("SectionLabel = %q, want %q", got, tt.rule)
			}
		})
	}
}

func TestSectionLabelFenceStateWithinOneChunk(t *testing.T) {
	const longHeading = "# Part 3 — deploy the API after verifying the rollback procedure"
	for _, tt := range []struct {
		caseName, chunk, mention, rule string
	}{
		{"long heading inside fence", "```sh\n" + longHeading + "\n```", "Part 3", ""},
		{"shorter marker leaves fence open", "````md\n```\n- **Roster.** example\n````", "Roster", ""},
		{"long heading after closing fence", "```sh\nexample\n```\n" + longHeading, "Part 3", "heading_lead"},
		{"opener in previous chunk", "# Part 3 — deploy the API", "Part 3", "heading_lead"},
	} {
		t.Run(tt.caseName, func(t *testing.T) {
			start := strings.Index(tt.chunk, tt.mention)
			if got := SectionLabel(tt.chunk, start, start+len(tt.mention), "topic", Preset{}); got != tt.rule {
				t.Errorf("SectionLabel = %q, want %q", got, tt.rule)
			}
		})
	}
}

func TestApplySectionLabelDropsAnchoredFact(t *testing.T) {
	const chunk = "## Part 3 — FE + API routes on Vercel\n## Summary"
	mention := func(name, label string) extract.Mention {
		start := strings.Index(chunk, name)
		return extract.Mention{Text: name, Label: label, Start: start, End: start + len(name)}
	}
	result := Apply(extract.ChunkResult{
		Mentions:   []extract.Mention{mention("Part 3", "date"), mention("Vercel", "organization"), mention("Summary", "topic")},
		Candidates: []extract.Candidate{{Head: 0, Tail: 1}, {Head: 1, Tail: 1}},
	}, chunk, Preset{Values: map[string]bool{"date": true}})
	if result.Dropped["section_label"] != 1 || result.Dropped["heading_line"] != 1 ||
		len(result.Mentions) != 1 || result.Mentions[0].Name != "Vercel" ||
		len(result.Candidates) != 1 || result.Candidates[0].Head != 0 || result.Candidates[0].Tail != 0 {
		t.Fatalf("Apply = %+v", result)
	}
}

func TestApplySectionLabelKeepTypes(t *testing.T) {
	dir := t.TempDir()
	for name, data := range map[string]string{
		"implicit": "allowlist: []\n",
		"empty":    "section_label_keep_types: []\n",
		"custom":   "section_label_keep_types: [cOnCePt]\n",
	} {
		if err := os.WriteFile(filepath.Join(dir, name+".yaml"), []byte(data), 0600); err != nil {
			t.Fatal(err)
		}
	}
	for _, tt := range []struct {
		chunk, name, label, preset string
		drop                       Rule
	}{
		{"**Zorvania – Trekking**", "Zorvania", "location", "", ""},
		{"## Kelmora (14)", "Kelmora", "location", "", ""},
		{"# Vandrel: arrivo", "Vandrel", "Location", "implicit", ""},
		{"## Part 3 — FE", "Part 3", "work", "", "section_label"},
		{"## Phase 2: Schema", "Phase 2", "concept", "", "section_label"},
		{"- **Kelmora**: details", "Kelmora", "location", "", "section_label"},
		{"## Kelmora", "Kelmora", "location", "", "heading_line"},
		{"## Kelmora (14)", "Kelmora", "location", "empty", "section_label"},
		{"## Kelmora (14)", "Kelmora", "location", "custom", "section_label"},
		{"## Phase 2: Schema", "Phase 2", "concept", "custom", ""},
	} {
		t.Run(tt.preset+"/"+tt.chunk, func(t *testing.T) {
			preset, err := LoadPreset(dir, tt.preset, "")
			if err != nil {
				t.Fatal(err)
			}
			start := strings.Index(tt.chunk, tt.name)
			mention := extract.Mention{Text: tt.name, Label: tt.label, Start: start, End: start + len(tt.name)}
			result, trace := ApplyWithTrace(extract.ChunkResult{Mentions: []extract.Mention{mention}}, tt.chunk, preset)
			if got := trace.MentionDrops[0]; got != tt.drop {
				t.Fatalf("drop rule = %q, want %q", got, tt.drop)
			}
			if tt.drop == "" && (len(result.Mentions) != 1 || result.Mentions[0].Mention != mention) {
				t.Errorf("kept mentions = %+v, want %+v", result.Mentions, mention)
			}
		})
	}
}
