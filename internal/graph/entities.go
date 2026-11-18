package graph

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"slices"
	"sort"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/neo4j/neo4j-go-driver/v6/neo4j"
	"golang.org/x/text/cases"
	"golang.org/x/text/unicode/norm"

	"github.com/alessiosavi/go-knowledge-graph/internal/domain"
)

// EntityID returns the entity node id for a normalized entity key.
func EntityID(key string) string {
	sum := sha256.Sum256([]byte("entity|" + key))
	return hex.EncodeToString(sum[:])[:32]
}

// EntityKey builds the stable key of an entity from its normalized name.
func EntityKey(name string) string {
	return NormalizeSurface(name)
}

// ValidDomain reports whether s is a valid domain slug.
func ValidDomain(s string) bool { return domain.ValidDomain(s) }

// DomainKey prefixes a normalized entity key with its domain.
func DomainKey(domain, key string) string { return "@" + domain + ":" + key }

// ScopedEntityKey builds an entity key scoped to a document. The leading #
// cannot occur in an unscoped key because edge punctuation is stripped.
func ScopedEntityKey(docID, name string) string {
	return "#" + docID + ":" + NormalizeSurface(name)
}

// NormalizeSurface canonicalizes a name with NFKC, Unicode case folding,
// whitespace collapse, edge punctuation/space/backtick trimming and removal
// of one trailing possessive. Other symbols stay (C++ becomes c++).
func NormalizeSurface(s string) string {
	s = norm.NFKC.String(s)
	s = cases.Fold().String(s)
	s = strings.Join(strings.Fields(s), " ")
	trimEdge := func(r rune) bool {
		return unicode.IsPunct(r) || unicode.IsSpace(r) || r == '`'
	}
	// Keep a leading apostrophe until after removing the possessive so a
	// surface consisting only of "'s" or "’s" normalizes to empty.
	s = strings.TrimRightFunc(s, trimEdge)
	s = stripPossessive(s)
	return strings.TrimFunc(s, trimEdge)
}

// DisplayName removes edge whitespace, wrapping markdown and one trailing
// possessive from a name, preserving case and other punctuation.
func DisplayName(s string) string {
	s = trimNameWrappers(s)
	s = stripPossessive(s)
	return trimNameWrappers(s)
}

// trimNameWrappers peels balanced code/emphasis wrappers only. Underscores
// and unmatched markers can be part of identifiers (__init__, *args).
func trimNameWrappers(s string) string {
	s = strings.TrimSpace(s)
	for len(s) > 0 && (s[0] == '`' || s[0] == '*') {
		marker := string(s[0])
		left := len(s) - len(strings.TrimLeft(s, marker))
		right := len(s) - len(strings.TrimRight(s, marker))
		if left != right || len(s) < 2*left || (s[0] == '*' && left > 2) {
			break
		}
		s = strings.TrimSpace(s[left : len(s)-right])
	}
	return s
}

func stripPossessive(s string) string {
	for _, suffix := range []string{"'s", "’s"} {
		if strings.HasSuffix(s, suffix) {
			return strings.TrimSuffix(s, suffix)
		}
	}
	return s
}

// SanitizeRel maps a relation label to the identifier-safe Neo4j type used
// for fact edges: lowercase, every non [a-z0-9] character becomes an
// underscore. Exported so the pipeline can key per-chunk dedupes exactly
// like the store does.
func SanitizeRel(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range strings.ToLower(s) {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
		} else {
			b.WriteByte('_')
		}
	}
	return b.String()
}

// sanitizeRel is the internal alias kept for readability at call sites.
func sanitizeRel(s string) string { return SanitizeRel(s) }

type factKey struct{ head, rel, tail string }

// dedupeFacts collapses duplicate triples inside one extraction call,
// keeping the highest confidence; the relative order is preserved.
func dedupeFacts(facts []FactUpsert) []FactUpsert {
	best := make(map[factKey]FactUpsert, len(facts))
	order := make([]factKey, 0, len(facts))
	for _, f := range facts {
		k := factKey{f.HeadKey, sanitizeRel(f.Rel), f.TailKey}
		if cur, ok := best[k]; ok {
			if f.Confidence > cur.Confidence {
				best[k] = f
			}
			continue
		}
		best[k] = f
		order = append(order, k)
	}
	out := make([]FactUpsert, 0, len(order))
	for _, k := range order {
		out = append(out, best[k])
	}
	return out
}

type mentionEdge struct {
	key        string
	start, end int
	typ        string
}

// mentionEdges unions entity mentions and fact spans, deduplicating identical
// (entity, span) occurrences. Entity mentions supply the type when spans overlap.
func mentionEdges(ents []EntityUpsert, facts []FactUpsert) []mentionEdge {
	type mentionKey struct {
		key        string
		start, end int
	}
	seen := make(map[mentionKey]bool)
	out := make([]mentionEdge, 0, 2*len(facts))
	add := func(key string, start, end int, typ string) {
		if key == "" {
			return
		}
		k := mentionKey{key, start, end}
		if !seen[k] {
			seen[k] = true
			out = append(out, mentionEdge{key, start, end, typ})
		}
	}
	for _, e := range ents {
		for _, m := range e.Mentions {
			add(e.Key, m.Start, m.End, m.Type)
		}
	}
	for _, f := range facts {
		add(f.HeadKey, f.HeadMention.Start, f.HeadMention.End, "")
		add(f.TailKey, f.TailMention.Start, f.TailMention.End, "")
	}
	return out
}

// WriteOptions controls extraction writes. FactsAsCandidates stores facts as
// nodes until confirmed. Domain assigns entities and candidates to a domain.
type WriteOptions struct {
	FactsAsCandidates bool
	Domain            string
}

// ErrMissingJudgeInput reports a candidate fact without its canonical judge request.
var ErrMissingJudgeInput = errors.New("graph: fact candidate requires judge input")

// UpsertExtraction replaces one chunk's extraction using the default options.
func (s *Store) UpsertExtraction(ctx context.Context, docID, chunkID, extractorVersion string, ents []EntityUpsert, facts []FactUpsert) error {
	return s.UpsertExtractionOpts(ctx, docID, chunkID, extractorVersion, ents, facts, WriteOptions{})
}

// UpsertExtractionOpts replaces one chunk's mentions and fact edges inside a
// single transaction: the chunk's MENTIONS and every fact edge carrying its
// chunk_id are deleted, entities are merged by key, facts are recreated
// (deduplicated by head+rel+tail keeping the highest confidence) and
// mention edges are rebuilt from every entity mention plus the facts' spans.
// Every touched entity's mention_count and voted type are refreshed, including
// entities the new extraction no longer mentions. Facts whose relation label
// has no alphanumeric content or whose entity keys are empty are rejected
// before anything is written; entries with an empty entity Key are skipped.
//
// Upserts are serialized per Store: concurrent transactions that MERGE the
// same entity key abort at commit with a unique-constraint violation that
// the driver does not retry (single-process scope, like UpsertDocument).
func (s *Store) UpsertExtractionOpts(ctx context.Context, docID, chunkID, extractorVersion string, ents []EntityUpsert, facts []FactUpsert, opt WriteOptions) error {
	if opt.Domain != "" && !ValidDomain(opt.Domain) {
		return fmt.Errorf("graph: invalid domain slug %q", opt.Domain)
	}
	for _, f := range facts {
		if opt.FactsAsCandidates && len(f.JudgeInput) == 0 {
			return ErrMissingJudgeInput
		}
		if strings.Trim(sanitizeRel(f.Rel), "_") == "" {
			return fmt.Errorf("graph: fact %q -[%q]-> %q: relation label has no alphanumeric content", f.HeadKey, f.Rel, f.TailKey)
		}
		if f.HeadKey == "" || f.TailKey == "" {
			return fmt.Errorf("graph: fact with relation %q references an empty entity key", f.Rel)
		}
	}
	// Fact endpoints must be entities of this extraction call: a key that is
	// neither upserted here nor known to the call would be dropped by the
	// MATCH when creating the edge, hiding caller bugs.
	entKeys := make(map[string]bool, len(ents))
	for _, e := range ents {
		if e.Key != "" {
			entKeys[e.Key] = true
		}
	}
	for _, f := range facts {
		if !entKeys[f.HeadKey] || !entKeys[f.TailKey] {
			return fmt.Errorf("graph: fact %q -[%q]-> %q: endpoint not among this chunk's entities", f.HeadKey, f.Rel, f.TailKey)
		}
	}
	facts = dedupeFacts(facts)
	mentions := mentionEdges(ents, facts)

	touched := make(map[string]bool, len(ents))
	keys := make([]string, 0, len(ents))
	for _, e := range ents {
		if e.Key == "" || touched[e.Key] {
			continue
		}
		touched[e.Key] = true
		keys = append(keys, e.Key)
	}
	for _, f := range facts {
		for _, k := range []string{f.HeadKey, f.TailKey} {
			if !touched[k] {
				touched[k] = true
				keys = append(keys, k)
			}
		}
	}

	entRows := make([]map[string]any, 0, len(ents))
	for _, e := range ents {
		if e.Key == "" {
			continue
		}
		entRows = append(entRows, map[string]any{
			"key": e.Key, "id": EntityID(e.Key), "name": e.Name, "type": e.Type,
			"aliases": e.Aliases, "name_key": EntityKey(e.Name),
		})
	}
	factRows := make([]map[string]any, 0, len(facts))
	for _, f := range facts {
		row := map[string]any{
			"head": f.HeadKey, "tail": f.TailKey, "rel": sanitizeRel(f.Rel),
			"confidence": f.Confidence, "low": f.LowConfidence,
			"hs": f.HeadMention.Start, "he": f.HeadMention.End,
			"ts": f.TailMention.Start, "te": f.TailMention.End,
		}
		if opt.FactsAsCandidates {
			row["id"] = CandidateID(f.HeadKey, sanitizeRel(f.Rel), f.TailKey, chunkID)
			row["input"] = string(f.JudgeInput)
			sum := sha256.Sum256(f.JudgeInput)
			row["hash"] = hex.EncodeToString(sum[:])
		}
		factRows = append(factRows, row)
	}
	menRows := make([]map[string]any, 0, len(mentions))
	for _, m := range mentions {
		var typ any
		if m.typ != "" {
			typ = m.typ
		}
		menRows = append(menRows, map[string]any{"key": m.key, "start": m.start, "end": m.end, "type": typ})
	}

	return s.runWriteTxLocked(ctx, func(tx neo4j.ManagedTransaction) error {
		// Everything below is scoped to an existing chunk.
		res, err := tx.Run(ctx, "MATCH (c:Chunk {id: $cid}) RETURN 1", map[string]any{"cid": chunkID})
		if err != nil {
			return fmt.Errorf("graph: upsert extraction: %w", err)
		}
		recs, err := res.Collect(ctx)
		if err != nil {
			return fmt.Errorf("graph: upsert extraction: %w", err)
		}
		if len(recs) == 0 {
			return fmt.Errorf("graph: chunk %s: %w", chunkID, ErrNotFound)
		}

		// Entities the chunk currently mentions must be refreshed even if
		// the new extraction drops them entirely.
		res, err = tx.Run(ctx, `MATCH (c:Chunk {id: $cid})-[:MENTIONS]->(e:Entity) RETURN e.key AS key`,
			map[string]any{"cid": chunkID})
		if err != nil {
			return fmt.Errorf("graph: capture mentions: %w", err)
		}
		oldRecs, err := res.Collect(ctx)
		if err != nil {
			return fmt.Errorf("graph: capture mentions: %w", err)
		}
		for _, r := range oldRecs {
			k := getString(r, "key")
			if k != "" && !touched[k] {
				touched[k] = true
				keys = append(keys, k)
			}
		}

		if err := txRun(ctx, tx, `MATCH (c:Chunk {id: $cid})-[m:MENTIONS]->(:Entity) DELETE m`,
			map[string]any{"cid": chunkID}); err != nil {
			return fmt.Errorf("graph: clear mentions: %w", err)
		}
		if err := txRun(ctx, tx, `MATCH (:Entity)-[r]->(:Entity) WHERE r.chunk_id = $cid DELETE r`,
			map[string]any{"cid": chunkID}); err != nil {
			return fmt.Errorf("graph: clear facts: %w", err)
		}
		if err := txRun(ctx, tx, `MATCH (f:FactCandidate {chunk_id: $cid}) DELETE f`,
			map[string]any{"cid": chunkID}); err != nil {
			return fmt.Errorf("graph: clear fact candidates: %w", err)
		}
		if len(entRows) > 0 {
			// Merge stored aliases with this call's aliases instead of
			// clobbering them; a call without aliases keeps the stored list.
			keyList := make([]string, 0, len(entRows))
			for _, row := range entRows {
				keyList = append(keyList, row["key"].(string))
			}
			res, err := tx.Run(ctx, `UNWIND $keys AS k MATCH (e:Entity {key: k}) RETURN e.key AS key, coalesce(e.aliases, []) AS aliases`,
				map[string]any{"keys": keyList})
			if err != nil {
				return fmt.Errorf("graph: read aliases: %w", err)
			}
			aliasRecs, err := res.Collect(ctx)
			if err != nil {
				return fmt.Errorf("graph: read aliases: %w", err)
			}
			stored := make(map[string][]string, len(aliasRecs))
			for _, r := range aliasRecs {
				stored[getString(r, "key")] = getStringSlice(r, "aliases")
			}
			// Fold every row's aliases per key: duplicate rows within one
			// call accumulate instead of last-wins, and every row of the key
			// receives the full merged list.
			mergedByKey := make(map[string][]string, len(entRows))
			for _, row := range entRows {
				key := row["key"].(string)
				merged, ok := mergedByKey[key]
				if !ok {
					merged = append([]string{}, stored[key]...)
				}
				for _, a := range row["aliases"].([]string) {
					if !slices.Contains(merged, a) {
						merged = append(merged, a)
					}
				}
				mergedByKey[key] = merged
			}
			for _, row := range entRows {
				if merged := mergedByKey[row["key"].(string)]; len(merged) > 0 {
					row["aliases"] = merged
				}
			}

			if err := txRun(ctx, tx, `
UNWIND $ents AS ent
MERGE (e:Entity {key: ent.key})
ON CREATE SET e.id = ent.id, e.mention_count = 0, e.type = ent.type
SET e.name = ent.name, e.name_key = ent.name_key,
    e.domain = CASE WHEN $domain <> '' THEN $domain ELSE e.domain END,
    e.domain_tok = CASE WHEN $domain <> '' THEN $domain_tok ELSE e.domain_tok END,
    e.aliases = CASE WHEN size(ent.aliases) > 0 THEN ent.aliases ELSE coalesce(e.aliases, []) END`,
				map[string]any{"ents": entRows, "domain": opt.Domain, "domain_tok": domainTokenOrNil(opt.Domain)}); err != nil {
				return fmt.Errorf("graph: merge entities: %w", err)
			}
		}
		if len(factRows) > 0 {
			if opt.FactsAsCandidates {
				if err := txRun(ctx, tx, `
UNWIND $facts AS f
CREATE (c:FactCandidate {id: f.id})
SET c.head_key = f.head, c.rel = f.rel, c.tail_key = f.tail,
    c.confidence = f.confidence, c.low_confidence = f.low,
    c.head_start = f.hs, c.head_end = f.he, c.tail_start = f.ts, c.tail_end = f.te,
    c.judge = '', c.judge_input = f.input, c.input_hash = f.hash,
    c.doc_id = $doc, c.chunk_id = $cid, c.extractor_version = $ver,
    c.domain = $domain, c.domain_tok = $domain_tok, c.created_at = $now`,
					map[string]any{"facts": factRows, "ver": extractorVersion, "doc": docID, "cid": chunkID,
						"domain": opt.Domain, "domain_tok": domainTokenOrNil(opt.Domain), "now": time.Now().UTC().Format(timeLayout)}); err != nil {
					return fmt.Errorf("graph: create fact candidates: %w", err)
				}
			} else if _, err := txWriteFactEdge(ctx, tx, factRows, extractorVersion, docID, chunkID, ""); err != nil {
				return err
			}
		}
		if len(menRows) > 0 {
			if err := txRun(ctx, tx, `
UNWIND $mentions AS m
MATCH (c:Chunk {id: $cid}), (e:Entity {key: m.key})
CREATE (c)-[men:MENTIONS]->(e)
SET men.start = m.start, men.end = m.end, men.type = m.type`,
				map[string]any{"mentions": menRows, "cid": chunkID}); err != nil {
				return fmt.Errorf("graph: create mentions: %w", err)
			}
		}
		if len(keys) > 0 {
			if err := txRefreshMentionCounts(ctx, tx, keys); err != nil {
				return err
			}
		}
		return nil
	})
}

// txWriteFactEdge writes typed edges identically for direct extraction and promotion.
func txWriteFactEdge(ctx context.Context, tx neo4j.ManagedTransaction, facts []map[string]any, version, docID, chunkID, judge string) (int, error) {
	res, err := tx.Run(ctx, `
UNWIND $facts AS f
MATCH (a:Entity {key: f.head}), (b:Entity {key: f.tail})
WITH a, b, f, {t: f.rel} AS x
CREATE (a)-[r:x.t]->(b)
SET r.confidence = f.confidence, r.low_confidence = f.low, r.judge = $judge,
    r.extractor_version = $ver, r.doc_id = $doc, r.chunk_id = $cid,
    r.head_key = f.head, r.tail_key = f.tail,
    r.head_start = f.hs, r.head_end = f.he, r.tail_start = f.ts, r.tail_end = f.te
RETURN count(r) AS n`,
		map[string]any{"facts": facts, "ver": version, "doc": docID, "cid": chunkID, "judge": judge})
	if err != nil {
		return 0, fmt.Errorf("graph: create facts: %w", err)
	}
	recs, err := res.Collect(ctx)
	if err != nil {
		return 0, fmt.Errorf("graph: create facts: %w", err)
	}
	return getInt(recs[0], "n"), nil
}

// domainTokenOrNil keeps unassigned nodes free of a token for the empty slug.
func domainTokenOrNil(slug string) any {
	if slug == "" {
		return nil
	}
	return domain.DomainToken(slug)
}

// CandidateID uses the same identity as the corresponding audit row.
// rel is the sanitized relation type, as stored on the candidate.
func CandidateID(headKey, rel, tailKey, chunkID string) string {
	return FactAuditID(headKey, rel, tailKey, chunkID)
}

// txCandidate matches both identity and the exact request that was judged.
func txCandidate(ctx context.Context, tx neo4j.ManagedTransaction, id, inputHash string) (map[string]any, error) {
	res, err := tx.Run(ctx, `MATCH (c:FactCandidate {id: $id}) WHERE c.input_hash = $hash RETURN properties(c) AS props`,
		map[string]any{"id": id, "hash": inputHash})
	if err != nil {
		return nil, fmt.Errorf("graph: read fact candidate: %w", err)
	}
	recs, err := res.Collect(ctx)
	if err != nil {
		return nil, fmt.Errorf("graph: read fact candidate: %w", err)
	}
	if len(recs) == 0 {
		return nil, nil
	}
	props, _ := recs[0].Get("props")
	return props.(map[string]any), nil
}

func txDeleteCandidate(ctx context.Context, tx neo4j.ManagedTransaction, id, inputHash string) error {
	return txRun(ctx, tx, `MATCH (c:FactCandidate {id: $id}) WHERE c.input_hash = $hash DELETE c`, map[string]any{"id": id, "hash": inputHash})
}

// PromoteCandidate atomically writes the confirmed edge and removes its candidate.
// A vanished candidate, changed request or missing endpoint returns false without writing.
func (s *Store) PromoteCandidate(ctx context.Context, id, inputHash string) (bool, error) {
	applied := false
	err := s.runWriteTxLocked(ctx, func(tx neo4j.ManagedTransaction) error {
		applied = false
		c, err := txCandidate(ctx, tx, id, inputHash)
		if err != nil || c == nil {
			return err
		}
		row := map[string]any{"head": c["head_key"], "rel": c["rel"], "tail": c["tail_key"],
			"confidence": c["confidence"], "low": c["low_confidence"],
			"hs": c["head_start"], "he": c["head_end"], "ts": c["tail_start"], "te": c["tail_end"]}
		created, err := txWriteFactEdge(ctx, tx, []map[string]any{row}, c["extractor_version"].(string), c["doc_id"].(string), c["chunk_id"].(string), "confirmed")
		if err != nil || created == 0 {
			return err
		}
		if err := txDeleteCandidate(ctx, tx, id, inputHash); err != nil {
			return err
		}
		applied = true
		return nil
	})
	return applied && err == nil, err
}

// SetCandidateJudge records an unclear verdict without exposing a typed edge.
func (s *Store) SetCandidateJudge(ctx context.Context, id, inputHash, status string) (bool, error) {
	applied := false
	err := s.runWriteTxLocked(ctx, func(tx neo4j.ManagedTransaction) error {
		applied = false
		res, err := tx.Run(ctx, `MATCH (c:FactCandidate {id: $id}) WHERE c.input_hash = $hash SET c.judge = $status RETURN c.id`,
			map[string]any{"id": id, "hash": inputHash, "status": status})
		if err != nil {
			return fmt.Errorf("graph: set candidate judge: %w", err)
		}
		recs, err := res.Collect(ctx)
		if err != nil {
			return fmt.Errorf("graph: set candidate judge: %w", err)
		}
		applied = len(recs) > 0
		return nil
	})
	return applied && err == nil, err
}

// RefuteCandidate atomically records the rejection audit and removes its candidate.
func (s *Store) RefuteCandidate(ctx context.Context, id, inputHash string, audit FactAudit) (bool, error) {
	applied := false
	err := s.runWriteTxLocked(ctx, func(tx neo4j.ManagedTransaction) error {
		applied = false
		c, err := txCandidate(ctx, tx, id, inputHash)
		if err != nil || c == nil {
			return err
		}
		if err := txWriteFactAudit(ctx, tx, audit); err != nil {
			return err
		}
		if err := txDeleteCandidate(ctx, tx, id, inputHash); err != nil {
			return err
		}
		applied = true
		return nil
	})
	return applied && err == nil, err
}

// FactAudit is one audited fact refusal: a candidate dropped below the
// store floor or a fact refuted by the judge; both land as :RejectedFact.
type FactAudit struct {
	HeadKey          string
	Rel              string
	TailKey          string
	DocID            string
	ChunkID          string
	Score            float64
	JudgeVerdict     string
	Reason           string
	ExtractorVersion string
}

// FactAuditID identifies an audit row: hex(sha256(head|rel|tail|chunk))[:32].
func FactAuditID(headKey, rel, tailKey, chunkID string) string {
	sum := sha256.Sum256([]byte(headKey + "|" + sanitizeRel(rel) + "|" + tailKey + "|" + chunkID))
	return hex.EncodeToString(sum[:])[:32]
}

// WriteFactAudit records one audit row (:RejectedFact).
func (s *Store) WriteFactAudit(ctx context.Context, a FactAudit) error {
	// Locked, so it is ordered against DeleteDocument within the process.
	return s.runWriteTxLocked(ctx, func(tx neo4j.ManagedTransaction) error {
		return txWriteFactAudit(ctx, tx, a)
	})
}

// txWriteFactAudit merges one audit row inside an existing transaction.
func txWriteFactAudit(ctx context.Context, tx neo4j.ManagedTransaction, a FactAudit) error {
	a.Rel = sanitizeRel(a.Rel)
	// An audit for a document that no longer exists (deleted while its
	// judge call was in flight) is not written: nothing could delete it.
	err := txRun(ctx, tx, `
OPTIONAL MATCH (d:Document {id: $doc})
WITH d WHERE $doc = '' OR d IS NOT NULL
MERGE (rf:RejectedFact {id: $id})
SET rf.head_key = $head, rf.rel = $rel, rf.tail_key = $tail, rf.doc_id = $doc,
    rf.chunk_id = $chunk, rf.score = $score, rf.judge_verdict = $verdict,
    rf.reason = $reason, rf.at = $at, rf.extractor_version = $ever`,
		map[string]any{
			"id":      FactAuditID(a.HeadKey, a.Rel, a.TailKey, a.ChunkID),
			"head":    a.HeadKey,
			"rel":     a.Rel,
			"tail":    a.TailKey,
			"doc":     a.DocID,
			"chunk":   a.ChunkID,
			"score":   a.Score,
			"verdict": a.JudgeVerdict,
			"reason":  a.Reason,
			"at":      time.Now().UTC().Format(timeLayout),
			"ever":    a.ExtractorVersion,
		})
	if err != nil {
		return fmt.Errorf("graph: write fact audit: %w", err)
	}
	return nil
}

// SetFactJudge records a judge verdict (confirmed|unclear) on the fact edge
// identified by (head, rel, tail, chunk).
func (s *Store) SetFactJudge(ctx context.Context, headKey, rel, tailKey, chunkID, status string) error {
	err := s.runWrite(ctx, `
MATCH (a:Entity {key: $head})-[r]->(b:Entity {key: $tail})
WHERE r.chunk_id = $chunk AND type(r) = $rel
SET r.judge = $status`,
		map[string]any{
			"head": headKey, "tail": tailKey, "chunk": chunkID,
			"rel": sanitizeRel(rel), "status": status,
		})
	if err != nil {
		return fmt.Errorf("graph: set fact judge: %w", err)
	}
	return nil
}

// RejectFact deletes a refuted fact edge and records the audit row in one
// transaction (refuted facts are never silently dropped).
func (s *Store) RejectFact(ctx context.Context, headKey, rel, tailKey, chunkID string, audit FactAudit) error {
	return s.runWriteTxLocked(ctx, func(tx neo4j.ManagedTransaction) error {
		if err := txRun(ctx, tx, `
MATCH (a:Entity {key: $head})-[r]->(b:Entity {key: $tail})
WHERE r.chunk_id = $chunk AND type(r) = $rel
DELETE r`,
			map[string]any{"head": headKey, "tail": tailKey, "chunk": chunkID, "rel": sanitizeRel(rel)}); err != nil {
			return fmt.Errorf("graph: reject fact: %w", err)
		}
		return txWriteFactAudit(ctx, tx, audit)
	})
}

// SetEntityAliases replaces an entity's alias list (test/support helper
// alongside the extraction path).
func (s *Store) SetEntityAliases(ctx context.Context, key string, aliases []string) error {
	err := s.runWrite(ctx, `
MATCH (e:Entity {key: $key})
SET e.aliases = $aliases`,
		map[string]any{"key": key, "aliases": aliases})
	if err != nil {
		return fmt.Errorf("graph: set entity aliases: %w", err)
	}
	return nil
}

const entityByIDQuery = `
MATCH (e:Entity {id: $q})
RETURN e.id AS id, e.key AS key, e.name AS name, e.type AS type,
       e.domain AS domain, coalesce(e.aliases, []) AS aliases, coalesce(e.mention_count, 0) AS mention_count`

const entityByKeyQuery = `
MATCH (e:Entity {key: $q})
RETURN e.id AS id, e.key AS key, e.name AS name, e.type AS type,
       e.domain AS domain, coalesce(e.aliases, []) AS aliases, coalesce(e.mention_count, 0) AS mention_count`

const typedFactVisibility = "(r.judge = 'confirmed' OR (NOT $confirmed_only AND ($include OR coalesce(r.low_confidence, false) = false)))"

// EntityView returns an entity with its aliases, mention count and facts,
// grouped by (relation, other entity) with larger groups first.
// Judge-confirmed facts are visible regardless of includeLowConfidence.
// Together lists up to ten co-mentioned entities ranked by shared chunks,
// then mention count descending and key ascending, regardless of fact confidence.
// idOrKey matches the entity id first, falling back to the entity key.
func (s *Store) EntityView(ctx context.Context, idOrKey string, includeLowConfidence bool) (EntityView, error) {
	return s.EntityViewOpts(ctx, idOrKey, ViewOptions{IncludeLowConfidence: includeLowConfidence})
}

// EntityViewOpts applies the requested fact visibility options.
func (s *Store) EntityViewOpts(ctx context.Context, idOrKey string, opt ViewOptions) (EntityView, error) {
	if opt.Domain != "" && !ValidDomain(opt.Domain) {
		return EntityView{}, fmt.Errorf("graph: invalid domain slug %q", opt.Domain)
	}
	params := map[string]any{"q": idOrKey}
	recs, err := s.runRead(ctx, entityByIDQuery, params)
	if err == nil && len(recs) == 0 {
		recs, err = s.runRead(ctx, entityByKeyQuery, params)
	}
	if err != nil {
		return EntityView{}, fmt.Errorf("graph: entity view: %w", err)
	}
	if len(recs) == 0 {
		return EntityView{}, fmt.Errorf("graph: entity %s: %w", idOrKey, ErrNotFound)
	}
	r := recs[0]
	if opt.Domain != "" && getString(r, "domain") != opt.Domain {
		return EntityView{}, fmt.Errorf("graph: entity %s: %w", idOrKey, ErrNotFound)
	}
	view := EntityView{
		EntityRef: EntityRef{
			Domain: getString(r, "domain"),
			ID:     getString(r, "id"),
			Key:    getString(r, "key"),
			Name:   getString(r, "name"),
			Type:   getString(r, "type"),
		},
		Aliases:      getStringSlice(r, "aliases"),
		MentionCount: getInt(r, "mention_count"),
	}

	recs, err = s.runRead(ctx, `
MATCH (e:Entity {key: $key})-[r]-(o:Entity)
WHERE `+typedFactVisibility+`
OPTIONAL MATCH (c:Chunk {id: r.chunk_id})
RETURN type(r) AS rel, o.id AS id, o.key AS key, o.name AS name, o.type AS type, o.domain AS domain,
       coalesce(r.confidence, 0) AS confidence, coalesce(r.judge, '') AS judge,
       coalesce(r.low_confidence, false) AS low, coalesce(r.doc_id, '') AS doc_id,
       coalesce(r.chunk_id, '') AS chunk_id,
       coalesce(r.head_key, '') AS head_key, coalesce(r.tail_key, '') AS tail_key,
       coalesce(r.head_start, -1) AS head_start, coalesce(r.tail_start, -1) AS tail_start,
       coalesce(c.text, '') AS text`,
		map[string]any{"key": view.Key, "include": opt.IncludeLowConfidence, "confirmed_only": opt.ConfirmedOnly})
	if err != nil {
		return EntityView{}, fmt.Errorf("graph: entity facts: %w", err)
	}
	if opt.IncludeUnverified && !opt.ConfirmedOnly {
		const projection = `
OPTIONAL MATCH (c:Chunk {id: r.chunk_id})
RETURN r.rel AS rel, o.id AS id, o.key AS key, o.name AS name, o.type AS type, o.domain AS domain,
       coalesce(r.confidence, 0) AS confidence, coalesce(r.judge, '') AS judge, true AS unverified,
       coalesce(r.low_confidence, false) AS low, coalesce(r.doc_id, '') AS doc_id,
       coalesce(r.chunk_id, '') AS chunk_id,
       r.head_key AS head_key, r.tail_key AS tail_key,
       coalesce(r.head_start, -1) AS head_start, coalesce(r.tail_start, -1) AS tail_start,
       coalesce(c.text, '') AS text`
		candidates, err := s.runRead(ctx, `
MATCH (r:FactCandidate {head_key: $key})
WHERE coalesce(r.judge, '') IN ['', 'unclear']
MATCH (o:Entity {key: r.tail_key})`+projection+`
UNION ALL
MATCH (r:FactCandidate {tail_key: $key})
WHERE r.head_key <> $key AND coalesce(r.judge, '') IN ['', 'unclear']
MATCH (o:Entity {key: r.head_key})`+projection, map[string]any{"key": view.Key})
		if err != nil {
			return EntityView{}, fmt.Errorf("graph: entity candidates: %w", err)
		}
		recs = append(recs, candidates...)
	}
	groupCount := make(map[string]int, len(recs))
	facts := make([]FactView, 0, len(recs))
	for _, fr := range recs {
		outgoing := getString(fr, "head_key") == view.Key
		center := getInt(fr, "tail_start")
		if outgoing {
			center = getInt(fr, "head_start")
		}
		fv := FactView{
			Rel: getString(fr, "rel"),
			Other: EntityRef{
				ID:     getString(fr, "id"),
				Key:    getString(fr, "key"),
				Name:   getString(fr, "name"),
				Type:   getString(fr, "type"),
				Domain: getString(fr, "domain"),
			},
			Confidence:    getFloat(fr, "confidence"),
			Judge:         getString(fr, "judge"),
			Unverified:    getBool(fr, "unverified"),
			LowConfidence: getBool(fr, "low"),
			Outgoing:      outgoing,
			DocID:         getString(fr, "doc_id"),
			ChunkID:       getString(fr, "chunk_id"),
			Excerpt:       ExcerptWindow(getString(fr, "text"), center, 120),
		}
		facts = append(facts, fv)
		groupCount[fv.Rel+"\x00"+fv.Other.Key]++
	}
	sort.SliceStable(facts, func(i, j int) bool {
		gi := groupCount[facts[i].Rel+"\x00"+facts[i].Other.Key]
		gj := groupCount[facts[j].Rel+"\x00"+facts[j].Other.Key]
		if gi != gj {
			return gi > gj
		}
		if facts[i].Rel != facts[j].Rel {
			return facts[i].Rel < facts[j].Rel
		}
		if facts[i].Other.Key != facts[j].Other.Key {
			return facts[i].Other.Key < facts[j].Other.Key
		}
		if facts[i].Confidence != facts[j].Confidence {
			return facts[i].Confidence > facts[j].Confidence
		}
		return facts[i].ChunkID < facts[j].ChunkID
	})
	view.Facts = facts
	view.Hidden, err = s.hiddenCounts(ctx, view.Key)
	if err != nil {
		return EntityView{}, err
	}

	recs, err = s.runRead(ctx, `
MATCH (e:Entity {key: $key})<-[:MENTIONS]-(c:Chunk)-[:MENTIONS]->(o:Entity)
WHERE o <> e
WITH o, count(DISTINCT c) AS shared_chunks
RETURN o.id AS id, o.key AS key, o.name AS name, o.type AS type, o.domain AS domain, shared_chunks
ORDER BY shared_chunks DESC, coalesce(o.mention_count, 0) DESC, o.key ASC
LIMIT 10`, map[string]any{"key": view.Key})
	if err != nil {
		return EntityView{}, fmt.Errorf("graph: entity co-mentions: %w", err)
	}
	view.Together = make([]CoMention, 0, len(recs))
	for _, r := range recs {
		view.Together = append(view.Together, CoMention{
			EntityRef: EntityRef{
				ID:     getString(r, "id"),
				Key:    getString(r, "key"),
				Name:   getString(r, "name"),
				Type:   getString(r, "type"),
				Domain: getString(r, "domain"),
			},
			SharedChunks: getInt(r, "shared_chunks"),
		})
	}
	return view, nil
}

// hiddenCounts counts distinct fact tuples per state without joining endpoints:
// an audit still explains a missing fact when its other endpoint no longer exists.
// The head/tail branches are disjoint, so self-loops count only once.
func (s *Store) hiddenCounts(ctx context.Context, key string) (HiddenCounts, error) {
	recs, err := s.runRead(ctx, `
MATCH (f:FactCandidate {head_key: $key})
RETURN coalesce(f.judge, '') AS judge, count(DISTINCT [f.head_key, f.rel, f.tail_key]) AS n, false AS rejected
UNION ALL
MATCH (f:FactCandidate {tail_key: $key})
WHERE f.head_key <> $key
RETURN coalesce(f.judge, '') AS judge, count(DISTINCT [f.head_key, f.rel, f.tail_key]) AS n, false AS rejected
UNION ALL
MATCH (f:RejectedFact {head_key: $key})
RETURN '' AS judge, count(DISTINCT [f.head_key, f.rel, f.tail_key]) AS n, true AS rejected
UNION ALL
MATCH (f:RejectedFact {tail_key: $key})
WHERE f.head_key <> $key
RETURN '' AS judge, count(DISTINCT [f.head_key, f.rel, f.tail_key]) AS n, true AS rejected`, map[string]any{"key": key})
	if err != nil {
		return HiddenCounts{}, fmt.Errorf("graph: hidden facts: %w", err)
	}
	var counts HiddenCounts
	for _, r := range recs {
		if getBool(r, "rejected") {
			counts.Rejected += getInt(r, "n")
		} else {
			switch getString(r, "judge") {
			case "":
				counts.Unjudged += getInt(r, "n")
			case "unclear":
				counts.Unclear += getInt(r, "n")
			}
		}
	}
	return counts, nil
}

// EntityByName preserves the single-result contract: highest mention count, then id.
func (s *Store) EntityByName(ctx context.Context, name string) (EntityRef, error) {
	refs, err := s.EntityCandidatesByName(ctx, name, "")
	if err != nil {
		return EntityRef{}, err
	}
	if len(refs) == 0 {
		return EntityRef{}, fmt.Errorf("graph: entity %q: %w", name, ErrNotFound)
	}
	return refs[0], nil
}

// EntityCandidatesByName matches EntityKey folding at v4 (including punctuation
// and possessives), using indexed name_key even for document-scoped entities.
// Legacy graphs retain their case-folded display-name scan.
func (s *Store) EntityCandidatesByName(ctx context.Context, name, slug string) ([]EntityRef, error) {
	if slug != "" && !ValidDomain(slug) {
		return nil, fmt.Errorf("graph: invalid domain slug %q", slug)
	}
	version, err := s.SchemaVersion(ctx)
	if err != nil {
		return nil, err
	}
	where := "toLower(trim(e.name)) = toLower(trim($name))"
	if version >= SchemaVersionV4 {
		where = "e.name_key = $k"
	}
	if slug != "" {
		where += " AND e.domain = $domain"
	}
	// One candidate per domain (the most-mentioned), and a hard cap: at v4 a
	// name_key is shared by every domain and by every document-scoped key.
	recs, err := s.runRead(ctx, `MATCH (e:Entity) WHERE `+where+`
 WITH e ORDER BY coalesce(e.mention_count,0) DESC, e.id ASC
 WITH coalesce(e.domain,'') AS dom, collect(e)[0] AS e
 RETURN e.id AS id,e.key AS key,e.name AS name,e.type AS type,e.domain AS domain
 ORDER BY coalesce(e.mention_count,0) DESC,e.id ASC
 LIMIT 100`, map[string]any{"name": name, "k": EntityKey(name), "domain": slug})
	if err != nil {
		return nil, fmt.Errorf("graph: entity candidates by name: %w", err)
	}
	refs := make([]EntityRef, 0, len(recs))
	for _, r := range recs {
		refs = append(refs, EntityRef{ID: getString(r, "id"), Key: getString(r, "key"), Name: getString(r, "name"), Type: getString(r, "type"), Domain: getString(r, "domain")})
	}
	return refs, nil
}

// ExcerptWindow returns up to width bytes of text centred near byte offset
// center, adjusted to UTF-8 rune boundaries. Exported for the search
// package's snippets.
func ExcerptWindow(text string, center, width int) string {
	if text == "" {
		return ""
	}
	if width <= 0 || width > len(text) {
		width = len(text)
	}
	start := center - width/3
	if start < 0 {
		start = 0
	}
	end := start + width
	if end > len(text) {
		end = len(text)
		start = end - width
		if start < 0 {
			start = 0
		}
	}
	for start > 0 && !utf8.RuneStart(text[start]) {
		start--
	}
	for end < len(text) && !utf8.RuneStart(text[end]) {
		end++
	}
	return strings.TrimSpace(text[start:end])
}
