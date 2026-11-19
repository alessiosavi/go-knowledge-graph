package graph

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"time"

	"github.com/alessiosavi/go-knowledge-graph/internal/argerr"
	"github.com/neo4j/neo4j-go-driver/v6/neo4j"
)

const mergePlanStatsBatch = 128

type MergeFoldPlan struct {
	Header  MergeFoldPlanHeader
	Groups  []MergeFoldGroup
	Summary MergeFoldSummary
}

type MergeFoldPlanHeader struct {
	PlanVersion      int       `json:"plan_version"`
	FoldVersion      int       `json:"fold_version"`
	ExceptionsDigest string    `json:"exceptions_digest"`
	EntityCount      int       `json:"entity_count"`
	SkippedRows      int       `json:"skipped_rows"`
	GeneratedAt      time.Time `json:"generated_at"`
}

type MergeFoldGroup struct {
	Fold         string            `json:"fold"`
	Canonical    string            `json:"canonical"`
	Members      []MergeFoldMember `json:"members"`
	TypeMismatch bool              `json:"type_mismatch"`
	Rewire       int               `json:"rewire"`
	Collapses    int               `json:"collapses"`
	Blocking     []MergeFoldBlock  `json:"blocking"`
}

type MergeFoldMember struct {
	Key              string         `json:"key"`
	ID               string         `json:"id"`
	Name             string         `json:"name"`
	Type             string         `json:"type"`
	Domain           string         `json:"domain"`
	Aliases          []string       `json:"aliases"`
	MentionCount     int            `json:"mention_count"`
	DocumentCount    int            `json:"document_count"`
	EdgesOut         MergeFoldEdges `json:"edges_out"`
	EdgesIn          MergeFoldEdges `json:"edges_in"`
	Candidates       int            `json:"candidates"`
	Rejected         int            `json:"rejected"`
	EmbeddingPresent bool           `json:"embedding_present"`
	mentions         int
}

type MergeFoldEdges struct {
	Confirmed int `json:"confirmed"`
	Unjudged  int `json:"unjudged"`
	Other     int `json:"other"`
}

type MergeFoldSummary struct {
	Groups, Members, TypeMismatches, BlockingGroups int
	MaxRewire, P95Rewire, StaleHints                int
}

// PlanMergeFolds reads a consistent snapshot of the fold record and selected
// entities. An empty slug selects all key scopes. No graph setup is needed.
func (s *Store) PlanMergeFolds(ctx context.Context, slug string) (MergeFoldPlan, error) {
	if slug != "" && !ValidDomain(slug) {
		return MergeFoldPlan{}, argerr.Errorf("invalid domain %q", slug)
	}
	var plan MergeFoldPlan
	err := s.runReadTx(ctx, func(tx neo4j.ManagedTransaction) error {
		record, err := txReadFoldRecord(ctx, tx)
		if err != nil {
			return err
		}
		const rebuild = "run gkg fold rebuild"
		switch {
		case record == nil:
			return argerr.New("fold record is missing; " + rebuild)
		case record.Version != FoldKeyVersion:
			return argerr.Errorf("fold version is %d, expected %d; %s", record.Version, FoldKeyVersion, rebuild)
		case record.Status != "complete":
			return argerr.Errorf("fold rebuild status is %s; %s", record.Status, rebuild)
		}

		plan.Header = MergeFoldPlanHeader{
			PlanVersion: 1, FoldVersion: record.Version, ExceptionsDigest: record.ExceptionsDigest,
			GeneratedAt: time.Now().UTC(),
		}
		groups, err := scanMergeFoldGroups(ctx, tx, slug, record, &plan)
		if err != nil {
			return err
		}
		if err := loadMergeFoldStats(ctx, tx, groups); err != nil {
			return err
		}
		plan.Groups = groups
		plan.summarize()
		return nil
	})
	if err != nil {
		return MergeFoldPlan{}, err
	}
	return plan, nil
}

const (
	mergePlanDomainFirstPageQuery = `MATCH (e:Entity) WHERE e.key >= $last AND e.key < $end` + foldPageProjection
	mergePlanDomainPageQuery      = `MATCH (e:Entity) WHERE e.key > $last AND e.key < $end` + foldPageProjection
)

func scanMergeFoldGroups(ctx context.Context, tx neo4j.ManagedTransaction, slug string, record *FoldRecord, plan *MergeFoldPlan) ([]MergeFoldGroup, error) {
	query, nextPage := foldPageQuery, foldPageQuery
	params := map[string]any{"last": "", "limit": foldBatchSize}
	if slug != "" {
		query, nextPage = mergePlanDomainFirstPageQuery, mergePlanDomainPageQuery
		prefix := DomainKey(slug, "")
		params["last"] = prefix
		// The prefix ends in ':', whose immediate successor is ';'.
		params["end"] = prefix[:len(prefix)-1] + ";"
	} else {
		// The key cursor cannot visit missing or empty keys.
		rows, err := txRows(ctx, tx, foldKeylessCountQuery, nil)
		if err != nil {
			return nil, fmt.Errorf("graph: read entities without keys: %w", err)
		}
		plan.Header.SkippedRows = getInt(rows[0], "n")
	}
	keysByFold := make(map[string][]string)
	for {
		rows, err := txRows(ctx, tx, query, params)
		if err != nil {
			return nil, fmt.Errorf("graph: scan entities for merge plan: %w", err)
		}
		for _, row := range rows {
			if !foldRowEligible(row) {
				plan.Header.SkippedRows++
				continue
			}
			plan.Header.EntityCount++
			update, fold := foldUpdate(row, record.Exceptions)
			if update != nil {
				plan.Summary.StaleHints++
			}
			if fold != nil {
				keysByFold[*fold] = append(keysByFold[*fold], getString(row, "key"))
			}
		}
		if len(rows) < foldBatchSize {
			break
		}
		query = nextPage
		params["last"] = getString(rows[len(rows)-1], "key")
	}
	var groups []MergeFoldGroup
	for _, fold := range slices.Sorted(maps.Keys(keysByFold)) {
		keys := keysByFold[fold]
		if len(keys) < 2 {
			continue
		}
		group := MergeFoldGroup{Fold: fold, Members: make([]MergeFoldMember, len(keys)), Blocking: []MergeFoldBlock{}}
		for i, key := range keys {
			group.Members[i].Key = key
		}
		groups = append(groups, group)
	}
	return groups, nil
}

const mergePlanMembersQuery = `UNWIND $keys AS key
MATCH (e:Entity {key: key})
OPTIONAL MATCH (e)<-[m:MENTIONS]-(c:Chunk)
OPTIONAL MATCH (c)<-[:HAS_CHUNK]-(d:Document)
RETURN ` + entityRefProjection + `, e.aliases AS aliases, e.mention_count AS mention_count,
       e.embedding IS NOT NULL AS embedding_present, count(DISTINCT m) AS mentions,
       count(DISTINCT d) AS documents`

const mergePlanEdgesQuery = `UNWIND $keys AS key
MATCH (:Entity {key: key})-[r]->(:Entity)
RETURN key, true AS outgoing, null AS head_key, r.judge AS judge, count(r) AS n
UNION ALL
UNWIND $keys AS key
MATCH (head:Entity)-[r]->(:Entity {key: key})
RETURN key, false AS outgoing, head.key AS head_key, r.judge AS judge, count(r) AS n`

func loadMergeFoldStats(ctx context.Context, tx neo4j.ManagedTransaction, groups []MergeFoldGroup) error {
	members := make(map[string]*MergeFoldMember)
	for i := range groups {
		for j := range groups[i].Members {
			m := &groups[i].Members[j]
			members[m.Key] = m
		}
	}
	keys := slices.Sorted(maps.Keys(members))
	for batch := range slices.Chunk(keys, mergePlanStatsBatch) {
		rows, err := txRows(ctx, tx, mergePlanMembersQuery, map[string]any{"keys": batch})
		if err != nil {
			return fmt.Errorf("graph: read merge plan member properties and mentions: %w", err)
		}
		for _, row := range rows {
			m := members[getString(row, "key")]
			m.ID, m.Name = getString(row, "id"), getString(row, "name")
			m.Type, m.Domain = getString(row, "type"), getString(row, "domain")
			m.Aliases = append([]string{}, getStringSlice(row, "aliases")...)
			m.MentionCount = getInt(row, "mention_count")
			m.DocumentCount = getInt(row, "documents")
			m.EmbeddingPresent = getBool(row, "embedding_present")
			m.mentions = getInt(row, "mentions")
		}
	}
	losers := make(map[string]*MergeFoldGroup)
	for i := range groups {
		group := &groups[i]
		candidates := make([]foldCandidate, 0, len(group.Members))
		for _, m := range group.Members {
			candidates = append(candidates, foldCandidate{EntityRef: EntityRef{Key: m.Key}, MentionCount: m.MentionCount})
		}
		canonical, _ := chooseCanonical(candidates)
		group.Canonical = canonical.Key
		for _, m := range group.Members {
			group.TypeMismatch = group.TypeMismatch || m.Type != group.Members[0].Type
			if m.Key != group.Canonical {
				losers[m.Key] = group
				group.Rewire += m.mentions
			}
		}
	}
	for batch := range slices.Chunk(keys, mergePlanStatsBatch) {
		rows, err := txRows(ctx, tx, mergePlanEdgesQuery, map[string]any{"keys": batch})
		if err != nil {
			return fmt.Errorf("graph: read merge plan typed edges: %w", err)
		}
		for _, row := range rows {
			key, n := getString(row, "key"), getInt(row, "n")
			m, group := members[key], losers[key]
			out := getBool(row, "outgoing")
			if out {
				m.EdgesOut.add(getString(row, "judge"), n)
			} else {
				m.EdgesIn.add(getString(row, "judge"), n)
			}
			// The outgoing branch already counts edges whose source
			// and target are both losers in this group.
			if group != nil && (out || losers[getString(row, "head_key")] != group) {
				group.Rewire += n
			}
		}
	}
	return loadMergeFoldFacts(ctx, tx, keys, members, losers)
}

func (e *MergeFoldEdges) add(judge string, n int) {
	switch judge {
	case "confirmed":
		e.Confirmed += n
	case "":
		e.Unjudged += n
	default:
		e.Other += n
	}
}

func (p *MergeFoldPlan) summarize() {
	p.Summary.Groups = len(p.Groups)
	sizes := make([]int, 0, len(p.Groups))
	for _, g := range p.Groups {
		p.Summary.Members += len(g.Members)
		if g.TypeMismatch {
			p.Summary.TypeMismatches++
		}
		if len(g.Blocking) > 0 {
			p.Summary.BlockingGroups++
		}
		sizes = append(sizes, g.Rewire)
	}
	if len(sizes) > 0 {
		slices.Sort(sizes)
		last := len(sizes) - 1
		p.Summary.MaxRewire = sizes[last]
		p.Summary.P95Rewire = sizes[last-len(sizes)/20] // nearest rank
	}
}
