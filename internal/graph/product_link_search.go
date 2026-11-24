package graph

import (
	"cmp"
	"context"
	"errors"
	"slices"
	"strings"
	"unicode"
	"unicode/utf8"

	"golang.org/x/text/unicode/norm"
)

// ErrProductLinkSearchTooLarge indicates that the snapshot exceeded the search leg's cap.
var ErrProductLinkSearchTooLarge = errors.New("product link search index too large")

type searchableProduct struct {
	product     Product
	title       []string
	titlePhrase string
	brandPhrase string
	length      int
}

func foldProductSearchText(text string) string {
	lower := strings.ToLower(norm.NFKC.String(text))
	// Same mark rule as fold keys: Latin diacritics go, marks of other scripts stay.
	return norm.NFC.String(string(stripFoldKeyLatinMarks([]rune(norm.NFD.String(lower)))))
}

func productSearchTokens(folded string) []string {
	return strings.FieldsFunc(folded, func(r rune) bool {
		// Marks left by the fold (non-Latin scripts) belong to their word.
		return !unicode.IsLetter(r) && !unicode.IsNumber(r) && !unicode.IsMark(r)
	})
}

func productSearchPhrase(tokens []string) string {
	if len(tokens) == 0 {
		return ""
	}
	// Spaces mark token boundaries for exact and contiguous matches.
	return " " + strings.Join(tokens, " ") + " "
}

func (index *ProductLinkIndex) addProductSearchRow(p Product, budget *productLinkBudget) {
	foldedTitle := foldProductSearchText(p.Title)
	title := productSearchTokens(foldedTitle)
	if len(title) == 0 {
		return
	}
	titlePhrase := productSearchPhrase(title)
	brandPhrase := productSearchPhrase(productSearchTokens(foldProductSearchText(p.Brand)))
	// Title tokens share foldedTitle's backing string and each retains a
	// string header (16 bytes on 64-bit Go).
	bytes := p.linkRowBytes() + len(foldedTitle) + len(titlePhrase) + len(brandPhrase) + 16*len(title)
	if !budget.add(bytes) {
		index.searchRows, index.searchOff = nil, true
		return
	}
	index.searchRows = append(index.searchRows, searchableProduct{
		product: p, title: title, titlePhrase: titlePhrase, brandPhrase: brandPhrase, length: utf8.RuneCountInString(p.Title),
	})
}

// SearchProductLinks searches product titles in the immutable link snapshot.
// Only bound stores are indexed. An empty domain searches every binding.
func (s *Store) SearchProductLinks(ctx context.Context, query, domain string, limit int) ([]Product, error) {
	words := productSearchTokens(foldProductSearchText(query))
	if len(words) == 0 || limit <= 0 {
		return nil, nil
	}
	index, err := s.LoadProductLinkIndex(ctx)
	if err != nil {
		return nil, err
	}
	return index.searchProducts(ctx, words, domain, limit)
}

type productSearchMatch struct {
	row     *searchableProduct
	group   int
	matched int
	brand   bool
}

func (index *ProductLinkIndex) searchProducts(ctx context.Context, words []string, domain string, limit int) ([]Product, error) {
	if index.searchOff {
		return nil, ErrProductLinkSearchTooLarge
	}
	phrase := productSearchPhrase(words)
	counts := make(map[string]int, len(words))
	for _, word := range words {
		counts[word]++
	}
	used := make(map[string]int)
	best := make([]productSearchMatch, 0, min(limit, len(index.searchRows)))
	for i := range index.searchRows {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		row := &index.searchRows[i]
		if domain != "" && row.product.Domain != domain {
			continue
		}
		match := productSearchMatch{row: row, matched: len(row.title)}
		switch {
		case phrase == row.titlePhrase:
			match.group = 1
		case len(row.title) < 2:
			// A one-word title would match too many queries; it matches only exactly.
			continue
		case strings.Contains(phrase, row.titlePhrase):
			match.group = 2
		default:
			clear(used)
			match.matched = 0
			for _, token := range row.title {
				if used[token] < counts[token] {
					used[token]++
					match.matched++
				}
			}
			if match.matched*2 < len(row.title) {
				continue
			}
			match.group = 3
		}
		match.brand = row.brandPhrase != "" && strings.Contains(phrase, row.brandPhrase)
		rank, _ := slices.BinarySearchFunc(best, match, compareProductSearchMatches)
		if rank < limit {
			best = slices.Insert(best, rank, match)
			best = best[:min(limit, len(best))]
		}
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	out := make([]Product, len(best))
	for i, match := range best {
		out[i] = match.row.product
	}
	return out, nil
}

func compareProductSearchMatches(a, b productSearchMatch) int {
	if a.group != b.group {
		return cmp.Compare(a.group, b.group)
	}
	if a.brand != b.brand {
		if a.brand {
			return -1
		}
		return 1
	}
	return cmp.Or(
		cmp.Compare(b.matched*len(a.row.title), a.matched*len(b.row.title)),
		cmp.Compare(a.row.length, b.row.length),
		cmp.Compare(a.row.product.Store, b.row.product.Store),
		cmp.Compare(a.row.product.ProductID, b.row.product.ProductID),
	)
}
