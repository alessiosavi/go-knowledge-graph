package graph

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/neo4j/neo4j-go-driver/v6/neo4j"

	"github.com/alessiosavi/go-knowledge-graph/internal/domain"
)

const (
	DefaultMentionsLimit = 5
	MaxMentionsLimit     = 20

	mentionRawLimit      = 1000
	mentionEligibleLimit = 200
	mentionByteBudget    = 2 << 20
	mentionKeyLimit      = 64
	mentionTermLimit     = 512
	mentionQueryBytes    = 16 << 10
	mentionTokenBytes    = 40 // Memgraph 3.14 drops tokens of this length or longer.
)

const (
	MentionsIndexTooLarge     = "index_too_large"
	MentionsIndexUnavailable  = "index_unavailable"
	MentionsSearchUnavailable = "search_unavailable"
)

// ValidMentionsLimit checks an explicitly supplied display limit.
func ValidMentionsLimit(limit int) bool { return limit >= 1 && limit <= MaxMentionsLimit }

type ProductMentionFieldMatch struct {
	Feature string `json:"feature"`
	Value   string `json:"value"`
}

type ProductMentionEntry struct {
	Key       string
	Span      [2]int
	Feature   string
	ValueFold string
}

type ProductChunkMention struct {
	DocID     string                     `json:"doc_id"`
	ChunkID   string                     `json:"chunk_id"`
	Seq       int                        `json:"seq"`
	DocTitle  string                     `json:"doc_title"`
	DocSource string                     `json:"doc_source"`
	Domain    string                     `json:"domain"`
	Excerpt   string                     `json:"excerpt"`
	Matched   []ProductMentionFieldMatch `json:"matched"`
	Entries   []ProductMentionEntry      `json:"-"`
}

type ProductMentionFields struct {
	Mentions        []ProductChunkMention `json:"mentions"`
	MentionsTotal   int                   `json:"mentions_total"`
	MentionsKeys    int                   `json:"mentions_keys"`
	MentionsPartial bool                  `json:"mentions_partial"`
}

type ProductDetailWithMentions struct {
	ProductDetail
	*ProductMentionFields
	MentionsError string `json:"mentions_error,omitempty"`
}

// LimitMentions caps the display without changing the number of checked hits.
// A zero limit denotes an omitted input.
func (out *ProductDetailWithMentions) LimitMentions(limit int) {
	if out.ProductMentionFields == nil {
		return
	}
	if limit == 0 {
		limit = DefaultMentionsLimit
	}
	out.Mentions = out.Mentions[:min(limit, len(out.Mentions))]
}

// ProductMentionRead records the work performed by the shared read.
type ProductMentionRead struct {
	IndexLoad, SearchMatch time.Duration
	RawSaturated           bool
	EligibleSaturated      bool
	DroppedKeys            int
}

// GetProductWithMentions reads the product, then the global identifier index,
// then candidate chunks, in separate snapshots. Matches use the full chunk
// text with the captured index. The caller may cap the displayed mentions.
func (s *Store) GetProductWithMentions(ctx context.Context, lookup ProductLookup) (out ProductDetailWithMentions, read ProductMentionRead, err error) {
	out.ProductDetail, err = s.GetProduct(ctx, lookup)
	if err != nil {
		return out, read, err
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	start := time.Now()
	index, err := s.LoadProductMentionIndex(ctx)
	read.IndexLoad = time.Since(start)
	if err != nil {
		out.MentionsError = MentionsIndexUnavailable
		if errors.Is(err, ErrProductMentionIndexTooLarge) {
			out.MentionsError = MentionsIndexTooLarge
		}
		slog.WarnContext(ctx, "load product mention index", "product", out.ID, "error", err)
		return out, read, nil
	}

	start = time.Now()
	defer func() { read.SearchMatch = time.Since(start) }()
	query := index.buildMentionQuery(out.ID, lookup.Domain)
	read.DroppedKeys = query.droppedKeys
	fields := &ProductMentionFields{Mentions: []ProductChunkMention{}, MentionsKeys: query.keys, MentionsPartial: query.partial}
	if query.text != "" {
		var candidates mentionCandidates
		candidates, err = s.productMentionCandidates(ctx, query.text, lookup.Domain)
		if err == nil {
			read.RawSaturated, read.EligibleSaturated = candidates.rawSaturated, candidates.eligibleSaturated
			fields.MentionsPartial = fields.MentionsPartial || candidates.rawSaturated || candidates.eligibleSaturated || candidates.byteLimited
			fields.Mentions, err = matchProductMentionCandidates(ctx, index, out.ID, candidates.rows)
			fields.MentionsTotal = len(fields.Mentions)
		}
	}
	if err == nil {
		err = ctx.Err()
	}
	if err != nil {
		out.MentionsError = MentionsSearchUnavailable
		slog.WarnContext(ctx, "search product mentions", "product", out.ID, "error", err)
		return out, read, nil
	}
	out.ProductMentionFields = fields
	return out, read, nil
}

// KeyedProducts gives the number of globally unambiguous keys per graph
// product ID, including products with no matching passages.
func (index *ProductMentionIndex) KeyedProducts() map[string]int {
	counts := make(map[string]int)
	for _, key := range index.keys {
		counts[key.product.ID]++
	}
	return counts
}

type mentionQuery struct {
	text        string
	keys        int
	partial     bool
	droppedKeys int
}

type mentionClause struct {
	text  string
	terms int
}

func (index *ProductMentionIndex) buildMentionQuery(productID, scope string) mentionQuery {
	var keys []string
	for key, entry := range index.keys {
		if entry.product.ID == productID {
			keys = append(keys, key)
		}
	}
	slices.Sort(keys)
	var out mentionQuery
	prefix, suffix, terms := "", "", 0
	if scope != "" {
		prefix, suffix, terms = `data.domain_tok:"`+domain.DomainToken(scope)+`" AND (`, ")", 1
	}
	const or = " OR "
	queryBytes := len(prefix) + len(suffix)
	var emitted []string
	seen := make(map[string]bool)
	for i, key := range keys {
		if i == mentionKeyLimit {
			out.partial = true
			break
		}
		clauses, dropped := mentionKeyClauses(key, index.keys[key].evidence)
		out.partial = out.partial || dropped
		if len(clauses) == 0 {
			continue
		}
		nextBytes, nextTerms := queryBytes, terms
		var added []string
		for _, clause := range clauses {
			if seen[clause.text] {
				continue
			}
			if len(emitted)+len(added) > 0 {
				nextBytes += len(or)
			}
			nextBytes += len(clause.text)
			nextTerms += clause.terms
			added = append(added, clause.text)
		}
		if len(added) == 0 {
			continue
		}
		if nextBytes > mentionQueryBytes || nextTerms > mentionTermLimit {
			out.partial = true
			break
		}
		out.keys++
		queryBytes, terms = nextBytes, nextTerms
		for _, clause := range added {
			seen[clause] = true
		}
		emitted = append(emitted, added...)
	}
	out.droppedKeys = len(keys) - out.keys
	if len(emitted) != 0 {
		out.text = prefix + strings.Join(emitted, or) + suffix
	}
	return out
}

func mentionKeyClauses(key string, evidence []ProductMentionEvidence) ([]mentionClause, bool) {
	if strings.ContainsFunc(key, func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsMark(r) && !unicode.IsNumber(r)
	}) {
		return nil, true
	}
	var clauses []mentionClause
	dropped := !validMentionTerm(key)
	if !dropped {
		clauses = append(clauses, mentionClause{mentionTerm(key), 1})
	}
	seen := make(map[string]bool)
	for _, observation := range evidence {
		runs := strings.FieldsFunc(observation.ValueFold, func(r rune) bool {
			return !unicode.IsLetter(r) && !unicode.IsMark(r) && !unicode.IsNumber(r)
		})
		if slices.ContainsFunc(runs, func(run string) bool { return !validMentionTerm(run) }) {
			dropped = true
			continue
		}
		if len(runs) < 2 {
			continue
		}
		for i, run := range runs {
			runs[i] = mentionTerm(run)
		}
		clause := "(" + strings.Join(runs, " AND ") + ")"
		if !seen[clause] {
			seen[clause] = true
			clauses = append(clauses, mentionClause{clause, len(runs)})
		}
	}
	return clauses, dropped
}

func validMentionTerm(term string) bool {
	return len(term) != 0 && len(term) < mentionTokenBytes && !strings.ContainsFunc(term, func(r rune) bool {
		return !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9')
	})
}

func mentionTerm(term string) string { return `data.text:"` + term + `"` }

type mentionCandidate struct {
	ProductChunkMention
	chars int
	text  string
}

type mentionCandidates struct {
	rows                                         []mentionCandidate
	rawSaturated, eligibleSaturated, byteLimited bool
}

// Keep one null candidate when no hit matches an eligible document so even
// a completely filtered search can report the pre-filter count.
var mentionCandidateQuery = fmt.Sprintf(`
CALL text_search.search('chunk_text_v2', $q, {limit: %d}) YIELD node, score
WITH collect({node: node, score: score}) AS raw
WITH raw, size(raw) AS raw_n
UNWIND CASE WHEN raw_n = 0 THEN [{node: null, score: null}] ELSE raw END AS hit
WITH raw_n, hit.node AS node, hit.score AS score
OPTIONAL MATCH (d:Document {id: node.doc_id})-[:HAS_CHUNK]->(node)
WHERE d.status = 'done' AND ($domain = '' OR d.domain = $domain)
WITH DISTINCT raw_n, d, CASE WHEN d IS NULL THEN null ELSE node END AS ch,
     CASE WHEN d IS NULL THEN null ELSE score END AS score
ORDER BY CASE WHEN d IS NULL THEN 1 ELSE 0 END, score DESC, ch.id
LIMIT %d
RETURN raw_n, ch.id AS id, ch.seq AS seq, size(ch.text) AS chars, d.id AS doc_id,
       d.title AS doc_title, d.source AS doc_source, d.domain AS domain`, mentionRawLimit, mentionEligibleLimit+1)

func (s *Store) productMentionCandidates(ctx context.Context, query, scope string) (out mentionCandidates, err error) {
	err = s.runReadTx(ctx, func(tx neo4j.ManagedTransaction) error {
		rows, err := txRows(ctx, tx, mentionCandidateQuery, map[string]any{"q": query, "domain": scope})
		if err != nil {
			return err
		}
		var eligible []mentionCandidate
		for _, row := range rows {
			out.rawSaturated = getInt(row, "raw_n") >= mentionRawLimit
			if id := getString(row, "id"); id != "" {
				eligible = append(eligible, mentionCandidate{
					ProductChunkMention: ProductChunkMention{
						ChunkID: id, Seq: getInt(row, "seq"), DocID: getString(row, "doc_id"),
						DocTitle: getString(row, "doc_title"), DocSource: getString(row, "doc_source"), Domain: getString(row, "domain"),
					},
					chars: getInt(row, "chars"),
				})
			}
		}
		if len(eligible) > mentionEligibleLimit {
			out.eligibleSaturated = true
			eligible = eligible[:mentionEligibleLimit]
		}
		out.rows, out.byteLimited = selectMentionCandidates(eligible)
		return txFetchMentionText(ctx, tx, out.rows)
	})
	return out, err
}

func selectMentionCandidates(rows []mentionCandidate) ([]mentionCandidate, bool) {
	// Memgraph size(text) counts Unicode characters. Charge four bytes
	// each, so the second statement can never fetch over the byte budget.
	const charBudget = mentionByteBudget / 4
	remaining := charBudget
	var selected []mentionCandidate
	partial := false
	for _, row := range rows {
		if row.chars > charBudget {
			partial = true
			continue
		}
		if row.chars > remaining {
			return selected, true
		}
		remaining -= row.chars
		selected = append(selected, row)
	}
	return selected, partial
}

func txFetchMentionText(ctx context.Context, tx neo4j.ManagedTransaction, rows []mentionCandidate) error {
	ids := make([]string, len(rows))
	positions := make(map[string]int, len(rows))
	for i, row := range rows {
		ids[i] = row.ChunkID
		positions[row.ChunkID] = i
	}
	texts, err := txRows(ctx, tx, `
UNWIND $ids AS id
MATCH (ch:Chunk {id: id})
RETURN id, ch.text AS text`, map[string]any{"ids": ids})
	if err != nil {
		return err
	}
	for _, row := range texts {
		rows[positions[getString(row, "id")]].text = getString(row, "text")
	}
	return nil
}

func matchProductMentionCandidates(ctx context.Context, index *ProductMentionIndex, productID string, rows []mentionCandidate) ([]ProductChunkMention, error) {
	mentions := make([]ProductChunkMention, 0, len(rows))
	for _, row := range rows {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		out := row.ProductChunkMention
		evidence := make(map[ProductMentionFieldMatch]struct{})
		for _, match := range index.Match(row.text) {
			if match.Product.ID != productID {
				continue
			}
			if len(out.Entries) == 0 {
				out.Excerpt = mentionExcerpt(row.text, match.Span[0])
			}
			for _, item := range match.Evidence {
				evidence[ProductMentionFieldMatch{Feature: item.Feature, Value: item.ValueFold}] = struct{}{}
				out.Entries = append(out.Entries, ProductMentionEntry{
					Key: match.Key, Span: match.Span, Feature: item.Feature, ValueFold: item.ValueFold,
				})
			}
		}
		if len(out.Entries) == 0 {
			continue
		}
		for item := range evidence {
			out.Matched = append(out.Matched, item)
		}
		slices.SortFunc(out.Matched, func(a, b ProductMentionFieldMatch) int {
			return cmp.Or(cmp.Compare(a.Feature, b.Feature), cmp.Compare(a.Value, b.Value))
		})
		out.Matched = out.Matched[:min(8, len(out.Matched))]
		mentions = append(mentions, out)
	}
	slices.SortFunc(mentions, func(a, b ProductChunkMention) int {
		return cmp.Or(cmp.Compare(a.DocSource, b.DocSource), cmp.Compare(a.DocID, b.DocID),
			cmp.Compare(a.Seq, b.Seq), cmp.Compare(a.ChunkID, b.ChunkID))
	})
	return mentions, nil
}

func mentionExcerpt(text string, matchStart int) string {
	start := max(0, matchStart-96)
	for !utf8.RuneStart(text[start]) {
		start++
	}
	end := min(len(text), start+300)
	for end < len(text) && !utf8.RuneStart(text[end]) {
		end--
	}
	return text[start:end]
}
