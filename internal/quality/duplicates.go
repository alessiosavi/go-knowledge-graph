package quality

import (
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"regexp"
	"slices"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/alessiosavi/go-knowledge-graph/internal/graph"
	"github.com/alessiosavi/go-knowledge-graph/internal/ingest"
	"golang.org/x/text/unicode/norm"
)

type duplicateGroup struct {
	Domain             string            `json:"domain"`
	Fold               string            `json:"fold"`
	Tier               string            `json:"tier"`
	Reason             string            `json:"reason"`
	SuggestedCanonical string            `json:"suggested_canonical"`
	Members            []duplicateMember `json:"members"`
}

type duplicateMember struct {
	Key               string    `json:"key"`
	Name              string    `json:"name"`
	Type              string    `json:"type"`
	MentionCount      int       `json:"mention_count"`
	DocumentCount     int       `json:"document_count"`
	TypedEdges        edgeCount `json:"typed_edges"`
	Candidates        int       `json:"candidates"`
	Rejected          int       `json:"rejected"`
	EvidenceSentences []string  `json:"evidence_sentences"`
	support           stringSet
}

type edgeCount struct {
	Confirmed int `json:"confirmed"`
	Other     int `json:"other"`
}

type tierCounts struct {
	A     int `json:"A"`
	Other int `json:"other"`
}

// DuplicateSummary contains aggregate census counts and hub sizes.
type DuplicateSummary struct {
	Groups       tierCounts `json:"groups"`
	Members      tierCounts `json:"members"`
	MaxGroupSize int        `json:"max_group_size"`
	P95GroupSize int        `json:"p95_group_size"`
}

var (
	digitRuns      = regexp.MustCompile(`\p{Nd}+`)
	versionOrModel = regexp.MustCompile(`\p{Nd}+(\.\p{Nd}+)+|[vV]\p{Nd}|\p{L}[\p{L}\p{M}]*[\p{Zs}\t]*\p{Nd}+`)
)

func symbolCounts(name string) [5]int {
	const symbols = "+#@/&"
	var counts [len(symbols)]int
	for _, r := range norm.NFKD.String(name) {
		if i := strings.IndexRune(symbols, r); i >= 0 {
			counts[i]++
		}
	}
	return counts
}

func duplicateKeyDomain(key string) string {
	qualified, hasPrefix := strings.CutPrefix(key, "@")
	domain, name, hasName := strings.Cut(qualified, ":")
	if hasPrefix && hasName && name != "" && graph.ValidDomain(domain) {
		return domain
	}
	return ""
}

func duplicateTierReason(fold string, members []duplicateMember) string {
	first := members[0]
	domain := duplicateKeyDomain(first.Key)
	if domain == "" {
		return "key_domain_mismatch"
	}
	for _, member := range members[1:] {
		if duplicateKeyDomain(member.Key) != domain {
			return "key_domain_mismatch"
		}
	}
	for _, member := range members[1:] {
		if member.Type != first.Type {
			return "type_mismatch"
		}
	}
	if first.Type == "" {
		return "type_missing"
	}
	letters := 0
	for _, r := range fold {
		if unicode.IsLetter(r) {
			letters++
		}
	}
	if letters < 4 {
		return "fold_too_short"
	}
	symbols := symbolCounts(first.Name)
	for _, member := range members[1:] {
		if symbolCounts(member.Name) != symbols {
			return "symbol_mismatch"
		}
	}
	runs := digitRuns.FindAllString(norm.NFKD.String(first.Name), -1)
	for _, member := range members[1:] {
		if !slices.Equal(digitRuns.FindAllString(norm.NFKD.String(member.Name), -1), runs) {
			return "digit_run_mismatch"
		}
	}
	for _, member := range members {
		if versionOrModel.MatchString(member.Name) {
			return "version_or_model"
		}
	}
	return ""
}

func duplicateCensusGroups(entities map[string]entity, entityDocs map[string]stringSet) map[string][]duplicateGroup {
	type groupKey struct{ domain, fold string }
	byFold := make(map[groupKey][]duplicateMember)
	for key, support := range entityDocs {
		if documentScoped(key) {
			continue
		}
		e := entities[key]
		fold := duplicateFold(e.name)
		if fold == "" {
			continue
		}
		group := groupKey{duplicateKeyDomain(key), fold}
		byFold[group] = append(byFold[group], duplicateMember{
			Key: key, Name: e.name, Type: e.typ, DocumentCount: len(support), EvidenceSentences: []string{}, support: support,
		})
	}
	groups := make(map[string][]duplicateGroup)
	for key, members := range byFold {
		if len(members) < 2 {
			continue
		}
		slices.SortFunc(members, func(a, b duplicateMember) int { return cmp.Compare(a.Key, b.Key) })
		reason := duplicateTierReason(key.fold, members)
		tier := "A"
		if reason != "" {
			tier = "other"
		}
		groups[key.domain] = append(groups[key.domain], duplicateGroup{Domain: key.domain, Fold: key.fold, Tier: tier, Reason: reason, Members: members})
	}
	return groups
}

func duplicateSizePercentile(counts []int) (maxSize, p95Size int) {
	if len(counts) == 0 {
		return 0, 0
	}
	slices.Sort(counts)
	// Nearest rank: ceil(0.95*n), then convert to a zero-based index.
	last := len(counts) - 1
	return counts[last], counts[last-len(counts)/20]
}

const duplicateBatchSize = 128

func writeDuplicates(ctx context.Context, st *graph.Store, out io.Writer, groups map[string][]duplicateGroup) (DuplicateSummary, error) {
	var summary DuplicateSummary
	var edges []int
	enc := json.NewEncoder(out)
	for _, domain := range slices.Sorted(maps.Keys(groups)) {
		rows := groups[domain]
		slices.SortFunc(rows, func(a, b duplicateGroup) int { return cmp.Compare(a.Fold, b.Fold) })
		members := make(map[string]*duplicateMember)
		for _, group := range rows {
			for i := range group.Members {
				member := &group.Members[i]
				members[member.Key] = member
			}
		}
		for keys := range slices.Chunk(slices.Sorted(maps.Keys(members)), duplicateBatchSize) {
			if err := loadDuplicateSizes(ctx, st, keys, members); err != nil {
				return DuplicateSummary{}, err
			}
			docIDs := make(stringSet)
			for _, key := range keys {
				maps.Copy(docIDs, members[key].support)
			}
			if err := loadDuplicateEvidence(ctx, st, keys, members, slices.Collect(maps.Keys(docIDs))); err != nil {
				return DuplicateSummary{}, err
			}
		}
		for _, group := range rows {
			if group.Tier == "A" {
				summary.Groups.A++
				summary.Members.A += len(group.Members)
			} else {
				summary.Groups.Other++
				summary.Members.Other += len(group.Members)
			}
			canonical := group.Members[0]
			size := 0
			for _, member := range group.Members {
				// Members are key-sorted, so ties retain the smallest key.
				if member.MentionCount > canonical.MentionCount {
					canonical = member
				}
				size += member.MentionCount + member.TypedEdges.Confirmed + member.TypedEdges.Other + member.Candidates + member.Rejected
			}
			group.SuggestedCanonical = canonical.Key
			edges = append(edges, size)
			if err := enc.Encode(group); err != nil {
				return DuplicateSummary{}, fmt.Errorf("quality: write census row: %w", err)
			}
		}
	}
	summary.MaxGroupSize, summary.P95GroupSize = duplicateSizePercentile(edges)
	return summary, nil
}

// Counts are per member across the stored graph, including legacy edges that
// lack provenance. The incoming branches exclude self-loops counted outgoing.
const duplicateSizesQuery = `
UNWIND $keys AS key
MATCH (:Chunk)-[m:MENTIONS]->(:Entity {key: key})
RETURN key, 'mentions' AS kind, count(m) AS n, 0 AS confirmed
UNION ALL
UNWIND $keys AS key
MATCH (:Entity {key: key})-[r]->(:Entity)
RETURN key, 'typed' AS kind, count(r) AS n, count(CASE WHEN r.judge = 'confirmed' THEN r END) AS confirmed
UNION ALL
UNWIND $keys AS key
MATCH (e:Entity {key: key})<-[r]-(head:Entity) WHERE head <> e
RETURN key, 'typed' AS kind, count(r) AS n, count(CASE WHEN r.judge = 'confirmed' THEN r END) AS confirmed
UNION ALL
UNWIND $keys AS key
MATCH (f:FactCandidate {head_key: key})
RETURN key, 'candidate' AS kind, count(f) AS n, 0 AS confirmed
UNION ALL
UNWIND $keys AS key
MATCH (f:FactCandidate {tail_key: key}) WHERE f.head_key IS NULL OR f.head_key <> key
RETURN key, 'candidate' AS kind, count(f) AS n, 0 AS confirmed
UNION ALL
UNWIND $keys AS key
MATCH (f:RejectedFact {head_key: key})
RETURN key, 'rejected' AS kind, count(f) AS n, 0 AS confirmed
UNION ALL
UNWIND $keys AS key
MATCH (f:RejectedFact {tail_key: key}) WHERE f.head_key IS NULL OR f.head_key <> key
RETURN key, 'rejected' AS kind, count(f) AS n, 0 AS confirmed`

func loadDuplicateSizes(ctx context.Context, st *graph.Store, keys []string, members map[string]*duplicateMember) error {
	result, err := st.RunReadQuery(ctx, duplicateSizesQuery, map[string]any{"keys": keys}, 0)
	if err != nil {
		return fmt.Errorf("quality: read census edge counts: %w", err)
	}
	for _, row := range result.Rows {
		member := members[textValue(row, "key")]
		n := int(floatValue(row, "n"))
		switch textValue(row, "kind") {
		case "mentions":
			member.MentionCount = n
		case "typed":
			confirmed := int(floatValue(row, "confirmed"))
			member.TypedEdges.Confirmed += confirmed
			member.TypedEdges.Other += n - confirmed
		case "candidate":
			member.Candidates += n
		case "rejected":
			member.Rejected += n
		}
	}
	return nil
}

// Match the indexed entity key before expanding to documents. Apply the
// document filter after expansion so the planner starts from the member.
// Limit each member's spans before returning any chunk text over Bolt.
const duplicateEvidenceQuery = `
UNWIND $keys AS key
CALL {
  WITH key
  MATCH (e:Entity {key: key}) WITH e
  MATCH (e)<-[m:MENTIONS]-(c:Chunk)<-[:HAS_CHUNK]-(d:Document)
  WHERE m.start >= 0 AND m.end > m.start
  WITH c, m, d WHERE d.id IN $doc_ids AND d.status = 'done' AND c.text <> ''
  RETURN c, m ORDER BY c.id, m.start, m.end LIMIT 2
}
RETURN key, c.text AS text, m.start AS start, m.end AS end ORDER BY key, c.id, m.start, m.end`

func loadDuplicateEvidence(ctx context.Context, st *graph.Store, keys []string, members map[string]*duplicateMember, docIDs []string) error {
	result, err := st.RunReadQuery(ctx, duplicateEvidenceQuery, map[string]any{"keys": keys, "doc_ids": docIDs}, 0)
	if err != nil {
		return fmt.Errorf("quality: read census evidence: %w", err)
	}
	for _, row := range result.Rows {
		member := members[textValue(row, "key")]
		text := textValue(row, "text")
		start, end := int(floatValue(row, "start")), int(floatValue(row, "end"))
		if start < 0 || end <= start || end > len(text) {
			continue
		}
		sentence := strings.TrimSpace(truncateEvidence(ingest.EvidenceSentence(text, start, end), 200))
		if sentence != "" && !slices.Contains(member.EvidenceSentences, sentence) {
			// The sentence may be a substring of a much larger chunk.
			member.EvidenceSentences = append(member.EvidenceSentences, strings.Clone(sentence))
		}
	}
	return nil
}

// truncateEvidence cuts s to at most n bytes at a rune start. Invalid UTF-8
// can leave no rune start before n; the cut then empties the string.
func truncateEvidence(s string, n int) string {
	if len(s) <= n {
		return s
	}
	cut := n
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut]
}
