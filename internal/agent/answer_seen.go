package agent

import (
	"maps"
	"slices"

	"github.com/alessiosavi/go-knowledge-graph/internal/chunkread"
	"github.com/alessiosavi/go-knowledge-graph/internal/graph"
	"github.com/alessiosavi/go-knowledge-graph/internal/search"
)

// Excerpt is the text of a passage returned by a tool.
type Excerpt struct {
	Tool      string
	Text      string
	Truncated bool
}

type answerCardText struct {
	Tool string `json:"tool"`
	graph.ProductText
	ExcerptCut bool `json:"excerpt_cut,omitempty"`
}

// Link is the association between a returned product and a passage.
type Link struct {
	ProductID           string
	Via                 []string
	OutsideReturnedText bool
	Kind                string
}

type seenIndex struct {
	products map[string]string
	texts    map[string][]answerCardText
	excerpts map[string][]Excerpt
	links    map[string][]Link
}

func newSeenIndex() *seenIndex {
	return &seenIndex{
		products: make(map[string]string),
		texts:    make(map[string][]answerCardText),
		excerpts: make(map[string][]Excerpt),
		links:    make(map[string][]Link),
	}
}

func (s *seenIndex) Product(token string) (productID string, ok bool) {
	productID, ok = s.products[token]
	return
}

func (s *seenIndex) Texts(productID string) []answerCardText { return s.texts[productID] }

func (s *seenIndex) Chunk(token string) bool { return len(s.excerpts[token]) != 0 }

func (s *seenIndex) Excerpts(chunkID string) []Excerpt { return s.excerpts[chunkID] }

func (s *seenIndex) Links(chunkID string) []Link { return s.links[chunkID] }

func (s *seenIndex) KnownIDs() []string {
	ids := make([]string, 0, len(s.products)+len(s.excerpts))
	ids = slices.AppendSeq(ids, maps.Keys(s.products))
	ids = slices.AppendSeq(ids, maps.Keys(s.excerpts))
	slices.Sort(ids)
	return slices.Compact(ids)
}

func (s *seenIndex) addProduct(id, productID string) {
	if productID == "" {
		return
	}
	s.products[productID] = productID
	if id != "" {
		s.products[id] = productID
	}
}

func (s *seenIndex) addProductText(productID string, entry answerCardText) {
	if productID == "" {
		return
	}
	for _, prior := range s.texts[productID] {
		if prior.Tool == entry.Tool && prior.Key == entry.Key && prior.Text == entry.Text {
			return
		}
	}
	s.texts[productID] = append(s.texts[productID], entry)
}

func (s *seenIndex) addExcerpt(id string, excerpt Excerpt) {
	if id == "" {
		return
	}
	for _, prior := range s.excerpts[id] {
		if prior.Text == excerpt.Text {
			return
		}
	}
	s.excerpts[id] = append(s.excerpts[id], excerpt)
}

func (s *seenIndex) addLink(chunkID string, link Link) {
	if chunkID == "" || link.ProductID == "" {
		return
	}
	for _, prior := range s.links[chunkID] {
		if prior.ProductID == link.ProductID && prior.Kind == link.Kind &&
			prior.OutsideReturnedText == link.OutsideReturnedText && slices.Equal(prior.Via, link.Via) {
			return
		}
	}
	s.links[chunkID] = append(s.links[chunkID], link)
}

// record indexes only typed entries in a result sent to the model after fitting.
func (s *seenIndex) record(tool string, result any) {
	switch out := result.(type) {
	case searchOut:
		for _, hit := range out.Hits {
			switch hit.Kind {
			case "product":
				s.addProduct(hit.ID, hit.ProductID)
				if hit.Key != nil && hit.Excerpt != nil {
					s.addProductText(hit.ProductID, answerCardText{
						Tool: tool,
						ProductText: graph.ProductText{
							Key:           *hit.Key,
							Text:          *hit.Excerpt,
							TextTruncated: hit.TextTruncated != nil && *hit.TextTruncated,
							Checked:       hit.Checked != nil && *hit.Checked,
						},
						ExcerptCut: hit.ExcerptCut != nil && *hit.ExcerptCut,
					})
				}
			case "chunk":
				if hit.Snippet != nil {
					s.addExcerpt(hit.ID, Excerpt{Tool: tool, Text: *hit.Snippet})
				}
			}
		}
	case graph.ProductSearchResult:
		for _, hit := range out.Hits {
			s.addProduct(hit.Product.ID, hit.Product.ProductID)
		}
	case search.ProductQueryResult:
		for _, hit := range out.Hits {
			s.addProduct(hit.ID, hit.ProductID)
			for _, match := range hit.Matched {
				s.addProductText(hit.ProductID, answerCardText{
					Tool: tool, ProductText: match.ProductText, ExcerptCut: match.ExcerptCut,
				})
			}
		}
	case productDetailOut:
		s.addProduct(out.ID, out.ProductID)
		if out.ProductTextFields != nil {
			for _, entry := range out.Texts {
				s.addProductText(out.ProductID, answerCardText{Tool: tool, ProductText: entry})
			}
		}
		for _, page := range out.Pages {
			for _, id := range page.ChunkIDs {
				s.addLink(id, Link{ProductID: out.ProductID, Kind: "page"})
			}
		}
		if out.ProductMentionFields != nil {
			for _, mention := range out.Mentions {
				s.addExcerpt(mention.ChunkID, Excerpt{Tool: tool, Text: mention.Excerpt})
				s.addLink(mention.ChunkID, Link{ProductID: out.ProductID, Kind: "mention"})
			}
		}
	case chunkread.Result:
		for _, chunk := range out.Chunks {
			s.addExcerpt(chunk.ID, Excerpt{Tool: tool, Text: chunk.Text, Truncated: chunk.Truncated})
			if chunk.Products == nil {
				continue
			}
			for _, product := range *chunk.Products {
				s.addProduct(product.ID, product.ProductID)
				s.addLink(chunk.ID, Link{ProductID: product.ProductID, Via: product.Via, OutsideReturnedText: product.OutsideReturnedText})
			}
		}
	}
}
