package agent

import (
	"maps"
	"slices"
	"strings"
	"time"

	"github.com/alessiosavi/go-knowledge-graph/internal/graph"
)

const (
	maxAnswerFeatures = 40
	maxComparisonRows = 12
)

// AnswerResponse is the domain-scoped gkg-answer/1 response.
type AnswerResponse struct {
	Contract     string            `json:"contract"`
	Domain       string            `json:"domain"`
	Summary      string            `json:"summary"`
	Products     []answerCardData  `json:"products"`
	Alternatives []answerCardData  `json:"alternatives"`
	Comparison   *answerComparison `json:"comparison"`
	Passages     []answerPassage   `json:"passages"`
	Complete     bool              `json:"complete"`
	Warnings     []string          `json:"warnings"`
	Dropped      answerDropped     `json:"dropped"`
	AsOf         answerAsOf        `json:"as_of"`
	Trace        answerTrace       `json:"trace"`
}

type answerCardData struct {
	graph.Product
	ProductType     *string                 `json:"product_type"`
	Features        []graph.ProductEvidence `json:"features"`
	FeaturesOmitted int                     `json:"features_omitted"`
	PassageIDs      []string                `json:"passage_ids"`
	Commerce        *struct{}               `json:"commerce"`
}

type answerComparison struct {
	ProductIDs []string              `json:"product_ids"`
	Rows       []answerComparisonRow `json:"rows"`
}

type answerComparisonRow struct {
	FeatureID string                             `json:"feature_id"`
	Cells     map[string][]graph.ProductEvidence `json:"cells"`
}

type answerPassage struct {
	ChunkID            string          `json:"chunk_id"`
	DocumentID         string          `json:"document_id"`
	DocumentTitle      string          `json:"document_title"`
	Source             string          `json:"source"`
	DocumentDate       *time.Time      `json:"document_date"`
	DocumentDateSource *string         `json:"document_date_source"`
	Excerpts           []answerExcerpt `json:"excerpts"`
	Links              []answerLink    `json:"links"`
}

type answerExcerpt struct {
	Tool      string `json:"tool"`
	Text      string `json:"text"`
	Truncated bool   `json:"truncated,omitempty"`
}

type answerLink struct {
	ProductID           string   `json:"product_id"`
	Via                 []string `json:"via,omitempty"`
	OutsideReturnedText bool     `json:"outside_returned_text,omitempty"`
	Kind                string   `json:"kind,omitempty"`
}

type answerDropped struct {
	NotReturned int `json:"not_returned"`
	NotFound    int `json:"not_found"`
}

type answerAsOf struct {
	ReleaseID     *string                `json:"release_id"`
	ActivationSeq *int64                 `json:"activation_seq"`
	Stores        []answerStoreWatermark `json:"stores"`
}

type answerStoreWatermark struct {
	Store                     string `json:"store"`
	MaterializedActivationSeq *int64 `json:"materialized_activation_seq"`
}

type answerTrace struct {
	Steps int      `json:"steps"`
	Tools []string `json:"tools"`
	MS    int64    `json:"ms"`
}

func buildAnswer(run answerRun, cards graph.ProductCards, docs map[string]graph.ChunkDocument, domain string) AnswerResponse {
	return buildMarkedAnswer(run, resolveMarks(run.Text, run.Seen), cards, docs, domain)
}

func buildMarkedAnswer(run answerRun, marks markResult, cards graph.ProductCards, docs map[string]graph.ChunkDocument, domain string) AnswerResponse {
	out := AnswerResponse{
		Contract: "gkg-answer/1",
		Domain:   domain,
		Summary:  cleanSummary(run.Text, marks.Spans),
		Passages: make([]answerPassage, 0, len(marks.Chunks)),
		Warnings: []string{},
		Dropped:  answerDropped{NotReturned: marks.NotReturned},
		AsOf: answerAsOf{
			ReleaseID:     cards.AsOf.ReleaseID,
			ActivationSeq: cards.AsOf.ActivationSeq,
			Stores:        make([]answerStoreWatermark, 0, len(cards.AsOf.Stores)),
		},
		Trace: answerTrace{Steps: run.Steps, Tools: append([]string{}, run.Tools...)},
	}
	for _, store := range cards.AsOf.Stores {
		out.AsOf.Stores = append(out.AsOf.Stores, answerStoreWatermark(store))
	}

	returned := make(map[string]*answerCardData, len(marks.Products)+len(marks.Alternatives))
	buildCards := func(ids []string) []answerCardData {
		// Reserve capacity so pointers to cards stay valid during appends.
		hydrated := make([]answerCardData, 0, len(ids))
		for _, id := range ids {
			card, ok := cards.Cards[id]
			if !ok {
				out.Dropped.NotFound++
				continue
			}
			n := min(len(card.Features), maxAnswerFeatures)
			hydrated = append(hydrated, answerCardData{
				Product:         card.Product,
				ProductType:     card.ProductType,
				Features:        append([]graph.ProductEvidence{}, card.Features[:n]...),
				FeaturesOmitted: card.FeaturesOmitted + len(card.Features) - n,
				PassageIDs:      []string{},
			})
			returned[id] = &hydrated[len(hydrated)-1]
		}
		return hydrated
	}
	out.Products = buildCards(marks.Products)
	out.Alternatives = buildCards(marks.Alternatives)
	for _, id := range marks.Chunks {
		doc, ok := docs[id]
		if !ok {
			out.Dropped.NotFound++
			continue
		}
		passage := answerPassage{
			ChunkID: id, DocumentID: doc.DocumentID, DocumentTitle: doc.Title, Source: doc.Source,
			DocumentDate: doc.DocumentDate, DocumentDateSource: doc.DocumentDateSource,
			Excerpts: make([]answerExcerpt, 0, len(run.Seen.Excerpts(id))),
			Links:    []answerLink{},
		}
		for _, excerpt := range run.Seen.Excerpts(id) {
			passage.Excerpts = append(passage.Excerpts, answerExcerpt(excerpt))
		}
		for _, link := range run.Seen.Links(id) {
			card, ok := returned[link.ProductID]
			if !ok {
				continue
			}
			passage.Links = append(passage.Links, answerLink(link))
			if len(card.PassageIDs) == 0 || card.PassageIDs[len(card.PassageIDs)-1] != id {
				card.PassageIDs = append(card.PassageIDs, id)
			}
		}
		out.Passages = append(out.Passages, passage)
	}

	out.Comparison = buildAnswerComparison(out.Products)
	empty := strings.TrimSpace(out.Summary) == "" && len(returned) == 0 && len(out.Passages) == 0
	switch {
	case run.Outcome == outcomeFinalTurnFailed:
		out.Summary = "No answer could be produced."
		out.Warnings = append(out.Warnings, "final_turn_failed")
	case run.Outcome == outcomeNoAnswer || empty:
		out.Summary = "No answer could be produced."
		out.Warnings = append(out.Warnings, "no_answer")
	case run.Outcome == outcomeTruncated:
		out.Warnings = append(out.Warnings, "truncated")
	default:
		out.Complete = true
	}
	if run.BudgetFinal {
		out.Warnings = append(out.Warnings, "budget_final")
	}
	return out
}

func buildAnswerComparison(cards []answerCardData) *answerComparison {
	if len(cards) < 2 {
		return nil
	}
	out := &answerComparison{ProductIDs: make([]string, 0, len(cards)), Rows: []answerComparisonRow{}}
	byFeature := make(map[string]map[string][]graph.ProductEvidence)
	for _, card := range cards {
		out.ProductIDs = append(out.ProductIDs, card.ProductID)
		for _, evidence := range card.Features {
			id := evidence.Feature.ID
			cells := byFeature[id]
			if cells == nil {
				cells = make(map[string][]graph.ProductEvidence)
				byFeature[id] = cells
			}
			cells[card.ProductID] = append(cells[card.ProductID], evidence)
		}
	}
	// Registry feature definitions are ordered by feature ID.
	for _, id := range slices.Sorted(maps.Keys(byFeature)) {
		cells := byFeature[id]
		if len(cells) < 2 {
			continue
		}
		for _, productID := range out.ProductIDs {
			cells[productID] = cells[productID]
		}
		out.Rows = append(out.Rows, answerComparisonRow{FeatureID: id, Cells: cells})
		if len(out.Rows) == maxComparisonRows {
			break
		}
	}
	return out
}
