package hygiene

import (
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
		{"```md\n## Part 3 — example\n```", "Part 3", ""},
		{"````md\n```\n- **Roster.** example\n````", "Roster", ""},
		{"```md\nexample\n```\n## Part 3 — real heading", "Part 3", "heading_lead"},
		{"## Allowlisted: suite", "Allowlisted", ""},
		{"- **Allowlisted**: suite", "Allowlisted", ""},
		{"## Deployment architecture", "Deployment architecture", ""},
		{"    ## Part 3 — indented", "Part 3", ""},
		{"**Etiopia**: trekking **altrove**", "Etiopia", "lead_in"},
		{"**Etiopia: Trekking** and **Hiking**", "Etiopia", ""},
	} {
		t.Run(tt.chunk+"/"+tt.name, func(t *testing.T) {
			start := strings.Index(tt.chunk, tt.name)
			if got := SectionLabel(tt.chunk, start, start+len(tt.name), preset); got != tt.rule {
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
