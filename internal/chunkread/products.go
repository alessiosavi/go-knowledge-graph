package chunkread

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"maps"
	"slices"
	"time"

	"github.com/alessiosavi/go-knowledge-graph/internal/argerr"
	"github.com/alessiosavi/go-knowledge-graph/internal/graph"
)

const (
	DefaultProductsLimit = 10
	MaxProductsLimit     = 50
	maxProductEvidence   = 8
	maxMatchBytes        = 2 << 20
	productIndexTimeout  = 5 * time.Second

	ProductsLimitError       = "products_limit must be an integer between 1 and 50"
	WithProductsDescription  = "Include products linked to the chunk's document and exact identifier matches on its full text (default false)"
	ProductsLimitDescription = "Maximum products per chunk after merging document links and identifier matches (1–50, default 10)"
	ProductsToolDescription  = " Set with_products to list document-linked products first, then exact identifier matches from the full chunk text. products_total counts both after deduplication, before products_limit and response-budget trimming. via identifies ref, url, page, and code evidence. Only URL and page links require the document and product store to share a domain. products_error reports code matching failures and links_partial reports incomplete document links. Retry a single chunk with a higher products_limit for more results. Use gkg products mentions-scan for the complete offline code scan."
)

// DecodeProductsLimit accepts an omitted field or a JSON integer in range.
func DecodeProductsLimit(raw json.RawMessage) (*int, error) {
	if raw == nil {
		return nil, nil
	}
	var limit *int
	if err := json.Unmarshal(raw, &limit); err != nil || limit == nil || *limit < 1 || *limit > MaxProductsLimit {
		return nil, argerr.New(ProductsLimitError)
	}
	return limit, nil
}

type MatchedIdentifier struct {
	Feature string `json:"feature"`
	Value   string `json:"value"`
}

type ChunkProduct struct {
	ID                  string              `json:"id"`
	Store               string              `json:"store"`
	ProductID           string              `json:"product_id"`
	Title               string              `json:"title"`
	Brand               string              `json:"brand"`
	Via                 []string            `json:"via"`
	Matched             []MatchedIdentifier `json:"matched"`
	OutsideReturnedText bool                `json:"outside_returned_text,omitempty"`
}

// ProductMentionIndexLoader is optional on the chunk Store.
type ProductMentionIndexLoader interface {
	LoadProductMentionIndex(context.Context) (*graph.ProductMentionIndex, error)
}

type productLinkIndexLoader interface {
	LoadProductLinkIndex(context.Context) (*graph.ProductLinkIndex, error)
	ProductLinkDisplays(context.Context, []string) (map[string]graph.Product, error)
}

func enrichProducts(ctx context.Context, store Store, result *Result, fullTexts []string, limit int) error {
	chunksByDoc := make(map[string][]int)
	for i, item := range result.Chunks {
		chunksByDoc[item.DocID] = append(chunksByDoc[item.DocID], i)
	}
	docIDs := slices.Sorted(maps.Keys(chunksByDoc))
	if len(docIDs) > 0 && docIDs[0] == "" {
		docIDs = docIDs[1:]
	}
	documents, partial := loadChunkDocuments(ctx, store, docIDs)
	result.LinksPartial = partial

	matchCodes := withinChunkMatchBudget(fullTexts)
	loadCtx, cancel := context.WithTimeout(ctx, productIndexTimeout)
	defer cancel()
	index, code, codeErr := loadChunkProductIndexes(loadCtx, store, documents, matchCodes)
	switch {
	case !matchCodes:
		result.ProductsError = "match_budget"
	case errors.Is(codeErr, graph.ErrProductMentionIndexTooLarge):
		result.ProductsError = "index_too_large"
	case codeErr != nil:
		result.ProductsError = "index_unavailable"
	}

	inferred := make(map[string]graph.InferredDocumentProducts, len(documents))
	for id, doc := range documents {
		owners, partial := index.ResolveDocumentProducts(doc)
		inferred[id] = owners
		if partial != "" {
			result.LinksPartial = partial
		}
	}
	var displays map[string]graph.Product
	if index.ProductDisplaysCapped() {
		var err error
		ids := inferredDisplayIDs(inferred, limit)
		displays, err = loadChunkProductDisplays(loadCtx, store, ids)
		if err != nil || len(displays) != len(ids) {
			// An incomplete display read cannot recover the ordered union.
			// Retain live refs and code matches using their own display rows.
			inferred = nil
			result.LinksPartial = "index_unavailable"
		}
	}

	pages := make([][]ChunkProduct, len(fullTexts))
	for docID, positions := range chunksByDoc {
		docProducts, linked := collectDocumentProducts(documents[docID], inferred[docID], index, displays, limit)
		for _, i := range positions {
			item := &result.Chunks[i]
			if code == nil && len(linked) == 0 {
				continue
			}
			var codeProducts []ChunkProduct
			if code != nil {
				codeProducts = collectChunkProducts(code.Match(fullTexts[i]), len(item.Text), index)
			}
			products, total := mergeChunkProducts(docProducts, linked, codeProducts, limit)
			empty := products[:0]
			item.Products, item.ProductsTotal = &empty, new(total)
			pages[i] = products
			item.ProductsOmittedForBudget = len(products)
		}
	}
	if index.ProductDisplaysCapped() {
		fillCodeProductBrands(loadCtx, store, pages)
	}
	return fitProductEntries(result, pages)
}

func loadChunkProductIndexes(ctx context.Context, store Store, documents map[string]graph.DocumentProductLinks,
	matchCodes bool,
) (*graph.ProductLinkIndex, *graph.ProductMentionIndex, error) {
	loader, ok := store.(productLinkIndexLoader)
	if !ok {
		if !matchCodes {
			return nil, nil, nil
		}
		code, err := loadChunkCodeIndex(ctx, store)
		return nil, code, err
	}
	if !matchCodes {
		hasDoneDocument := false
		for _, doc := range documents {
			if doc.Status == graph.StatusDone && doc.Domain != "" {
				hasDoneDocument = true
				break
			}
		}
		if !hasDoneDocument {
			return nil, nil, nil
		}
	}
	index, err := loader.LoadProductLinkIndex(ctx)
	if err == nil {
		err = ctx.Err()
	}
	if err == nil && index == nil {
		err = errors.New("product link loader returned no index")
	}
	if err != nil {
		slog.WarnContext(ctx, "get_chunks product link index unavailable", "error", err)
		return nil, nil, err
	}
	if !matchCodes {
		return index, nil, nil
	}
	code, err := index.MentionIndex()
	return index, code, err
}

func loadChunkDocuments(ctx context.Context, store Store, ids []string) (map[string]graph.DocumentProductLinks, string) {
	if len(ids) == 0 {
		return nil, ""
	}
	linker, ok := store.(DocumentProductLinker)
	if !ok {
		return nil, "index_unavailable"
	}
	ctx, cancel := context.WithTimeout(ctx, productIndexTimeout)
	defer cancel()
	docs, err := linker.DocumentProductLinks(ctx, ids)
	if err != nil {
		slog.WarnContext(ctx, "get_chunks document product links unavailable", "error", err)
		return nil, "index_unavailable"
	}
	return docs, ""
}

func withinChunkMatchBudget(fullTexts []string) bool {
	bytes := 0
	for _, full := range fullTexts {
		if len(full) > maxMatchBytes-bytes {
			return false
		}
		bytes += len(full)
	}
	return true
}

func loadChunkCodeIndex(ctx context.Context, store Store) (*graph.ProductMentionIndex, error) {
	loader, ok := store.(ProductMentionIndexLoader)
	if !ok {
		return nil, errors.New("store has no product mention loader")
	}
	index, err := loader.LoadProductMentionIndex(ctx)
	if err == nil {
		err = ctx.Err()
		if err == nil && index == nil {
			err = errors.New("product mention loader returned no index")
		}
	}
	if err != nil && !errors.Is(err, graph.ErrProductMentionIndexTooLarge) {
		slog.WarnContext(ctx, "get_chunks product mention index unavailable", "error", err)
	}
	if err != nil {
		return nil, err
	}
	return index, nil
}

func loadChunkProductDisplays(ctx context.Context, store Store, ids map[string]struct{}) (map[string]graph.Product, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	products, err := store.(productLinkIndexLoader).ProductLinkDisplays(ctx, slices.Collect(maps.Keys(ids)))
	if err != nil {
		slog.WarnContext(ctx, "get_chunks product displays unavailable", "error", err)
	}
	return products, err
}

func inferredDisplayIDs(inferred map[string]graph.InferredDocumentProducts, limit int) map[string]struct{} {
	ids := make(map[string]struct{})
	for _, owners := range inferred {
		// Each URL bucket is already ordered by product identity. At most
		// its first limit entries can be displayed in the merged list.
		for _, bucket := range owners.URL {
			for _, id := range bucket[:min(limit, len(bucket))] {
				ids[id] = struct{}{}
			}
		}
		if owners.Page != "" {
			ids[owners.Page] = struct{}{}
		}
	}
	return ids
}

func fillCodeProductBrands(ctx context.Context, store Store, pages [][]ChunkProduct) {
	ids := make(map[string]struct{})
	for _, products := range pages {
		for _, product := range products {
			if product.Via[0] == "code" {
				ids[product.ID] = struct{}{}
			}
		}
	}
	displays, _ := loadChunkProductDisplays(ctx, store, ids)
	for _, products := range pages {
		for i, product := range products {
			if display, ok := displays[product.ID]; ok && product.Via[0] == "code" {
				products[i].Brand = display.Brand
			}
		}
	}
}

func collectDocumentProducts(doc graph.DocumentProductLinks, owners graph.InferredDocumentProducts,
	index *graph.ProductLinkIndex, displays map[string]graph.Product, limit int,
) ([]ChunkProduct, map[string]struct{}) {
	if doc.Status != graph.StatusDone {
		return nil, nil
	}
	linked := make(map[string]struct{})
	positions := make(map[string]int)
	var products []ChunkProduct
	addEvidence := func(pos int, via string) {
		if evidence := products[pos].Via; evidence[len(evidence)-1] != via {
			products[pos].Via = append(evidence, via)
		}
	}
	add := func(product graph.Product, via string) {
		if pos, exists := positions[product.ID]; exists {
			addEvidence(pos, via)
			return
		}
		positions[product.ID] = len(products)
		out := newChunkProduct(product)
		out.Via = []string{via}
		products = append(products, out)
	}
	addOwner := func(id, via string) {
		if pos, ok := positions[id]; ok {
			// Keep the live display row and add evidence from the snapshot.
			addEvidence(pos, via)
		} else if product, ok := index.ProductDisplay(id); ok {
			add(product, via)
		} else if product, ok := displays[id]; ok && product.Domain == doc.Domain {
			add(product, via)
		}
	}
	for _, p := range doc.Refs {
		linked[p.ID] = struct{}{}
		add(p, "ref")
	}
	for _, ids := range owners.URL {
		for i, id := range ids {
			linked[id] = struct{}{}
			if _, staged := positions[id]; staged || i < limit {
				addOwner(id, "url")
			}
		}
	}
	if owners.Page != "" {
		// A page owner outside a URL prefix cannot outrank that prefix.
		// If it was also selected as a ref, the URL evidence is already set.
		if _, alreadyLinked := linked[owners.Page]; !alreadyLinked {
			addOwner(owners.Page, "page")
		} else if _, staged := positions[owners.Page]; staged {
			addOwner(owners.Page, "page")
		}
		linked[owners.Page] = struct{}{}
	}
	precedence := [...]string{"ref", "url", "page"}
	slices.SortFunc(products, func(a, b ChunkProduct) int {
		return cmp.Or(cmp.Compare(slices.Index(precedence[:], a.Via[0]), slices.Index(precedence[:], b.Via[0])),
			compareChunkProducts(a, b))
	})
	return products[:min(limit, len(products))], linked
}

func mergeChunkProducts(document []ChunkProduct, linked map[string]struct{}, code []ChunkProduct, limit int) ([]ChunkProduct, int) {
	products := append(make([]ChunkProduct, 0, min(limit, len(document)+len(code))), document...)
	positions := make(map[string]int, len(document))
	for i, product := range document {
		positions[product.ID] = i
	}
	total := len(linked)
	for _, product := range code {
		if pos, kept := positions[product.ID]; kept {
			products[pos].Via = append(slices.Clip(products[pos].Via), "code")
			products[pos].Matched = product.Matched
			products[pos].OutsideReturnedText = product.OutsideReturnedText
		} else if _, linked := linked[product.ID]; !linked {
			total++
			if len(products) < limit {
				products = append(products, product)
			}
		}
	}
	return products, total
}

// Start with every product trimmed, then restore the longest prefix that fits.
// This is equivalent to removing products from the last chunk first. Measuring
// only each changed item after encoding the envelope avoids re-encoding all
// chunk text, and any chunk dropping still happens after this step.
func fitProductEntries(result *Result, pages [][]ChunkProduct) error {
	wire, err := json.Marshal(result)
	if err != nil || len(wire) >= ResponseBudget {
		return err
	}
	remaining := ResponseBudget - len(wire)
	for i, products := range pages {
		if len(products) == 0 {
			continue
		}
		item := &result.Chunks[i]
		before, err := json.Marshal(item)
		if err != nil {
			return err
		}
		size := len(before)
		for kept := 1; kept <= len(products); kept++ {
			*item.Products = products[:kept]
			item.ProductsOmittedForBudget = len(products) - kept
			after, err := json.Marshal(item)
			if err != nil {
				return err
			}
			delta := len(after) - size
			if delta > remaining {
				*item.Products = products[:kept-1]
				item.ProductsOmittedForBudget++
				return nil
			}
			size = len(after)
			remaining -= delta
		}
	}
	return nil
}

func newChunkProduct(p graph.Product) ChunkProduct {
	return ChunkProduct{
		ID: p.ID, Store: p.Store, ProductID: p.ProductID, Title: p.Title, Brand: p.Brand,
		Matched: []MatchedIdentifier{},
	}
}

func collectChunkProducts(matches []graph.ProductMentionMatch, prefixBytes int, index *graph.ProductLinkIndex) []ChunkProduct {
	byID := make(map[string]*ChunkProduct)
	for _, match := range matches {
		product := match.Product
		out := byID[product.ID]
		if out == nil {
			if display, ok := index.ProductDisplay(product.ID); ok {
				product.Brand = display.Brand
			}
			entry := newChunkProduct(product)
			entry.Via = []string{"code"}
			entry.OutsideReturnedText = true
			out = &entry
			byID[product.ID] = out
		}
		out.OutsideReturnedText = out.OutsideReturnedText && match.Span[1] > prefixBytes
		for _, evidence := range match.Evidence {
			value := MatchedIdentifier{Feature: evidence.Feature, Value: evidence.ValueFold}
			i, found := slices.BinarySearchFunc(out.Matched, value, compareIdentifiers)
			if found || i >= maxProductEvidence {
				continue
			}
			out.Matched = slices.Insert(out.Matched, i, value)
			out.Matched = out.Matched[:min(len(out.Matched), maxProductEvidence)]
		}
	}
	products := make([]ChunkProduct, 0, len(byID))
	for out := range maps.Values(byID) {
		products = append(products, *out)
	}
	slices.SortFunc(products, compareChunkProducts)
	return products
}

func compareChunkProducts(a, b ChunkProduct) int {
	return cmp.Or(cmp.Compare(a.Store, b.Store), cmp.Compare(a.ProductID, b.ProductID), cmp.Compare(a.ID, b.ID))
}

func compareIdentifiers(a, b MatchedIdentifier) int {
	return cmp.Or(cmp.Compare(a.Feature, b.Feature), cmp.Compare(a.Value, b.Value))
}
