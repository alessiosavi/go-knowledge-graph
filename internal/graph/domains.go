package graph

import (
	"context"
	"crypto/rand"
	"fmt"
	"time"

	"github.com/alessiosavi/go-knowledge-graph/internal/domain"
	"github.com/neo4j/neo4j-go-driver/v6/neo4j"
)

// DecisionRecord is the durable evidence for a domain assignment or proposal.
type DecisionRecord struct {
	OpID, DocID, Domain, PreviousDomain, Action, By string
	Top                                             []domain.DomainScore
	Params                                          domain.Params
	Note                                            string
	At                                              time.Time
}

// Assignment publishes the domain, profile and decision together.
type Assignment struct {
	DocID, Domain, DomainName, CreatedBy string
	NewDomain                            bool
	Profile                              domain.Profile
	Decision                             DecisionRecord
}

// DomainProfiles reads only persisted profiles. Unfinished assignments count:
// their profiles were committed atomically, before mentions are written.
// Superseded documents no longer belong to the live corpus.
func (s *Store) DomainProfiles(ctx context.Context, excludeDocID string) ([]domain.DocProfile, error) {
	rows, err := s.runRead(ctx, `
MATCH (d:Document)
WHERE d.id <> $exclude AND d.domain IS NOT NULL AND d.domain <> ''
  AND d.name_profile IS NOT NULL AND d.status <> $superseded
RETURN d.id AS id,d.domain AS domain,d.source AS source,d.name_profile AS profile
ORDER BY d.id`, map[string]any{"exclude": excludeDocID, "superseded": string(StatusSuperseded)})
	if err != nil {
		return nil, fmt.Errorf("graph: domain profiles: %w", err)
	}
	out := make([]domain.DocProfile, 0, len(rows))
	for _, r := range rows {
		v, _ := r.Get("profile")
		stored, ok := v.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("graph: document %s has invalid name profile", getString(r, "id"))
		}
		profile := make(domain.Profile, len(stored))
		for name, v := range stored {
			n, ok := v.(int64)
			if !ok {
				return nil, fmt.Errorf("graph: document %s has invalid name frequency for %q", getString(r, "id"), name)
			}
			profile[name] = int(n)
		}
		out = append(out, domain.DocProfile{DocID: getString(r, "id"), Domain: getString(r, "domain"), Source: getString(r, "source"), Profile: profile})
	}
	return out, nil
}

func profileProperties(p domain.Profile) map[string]any {
	out := make(map[string]any, len(p))
	for name, n := range p {
		out[name] = n
	}
	return out
}

// AssignDomain commits one assignment. The private hook exercises rollback after
// all statements have run, without adding a hook to the public store interface.
func (s *Store) AssignDomain(ctx context.Context, a Assignment) error {
	return s.assignDomain(ctx, a, nil)
}

func (s *Store) assignDomain(ctx context.Context, a Assignment, beforeCommit func() error) error {
	if !ValidDomain(a.Domain) {
		return fmt.Errorf("graph: invalid domain slug %q", a.Domain)
	}
	r := a.Decision
	if r.DocID == "" {
		r.DocID = a.DocID
	}
	if r.Domain == "" {
		r.Domain = a.Domain
	}
	if r.OpID == "" {
		r.OpID = rand.Text()
	}
	if r.At.IsZero() {
		r.At = time.Now().UTC()
	}
	if r.By == "" {
		r.By = a.CreatedBy
	}
	if a.DomainName == "" {
		a.DomainName = a.Domain
	}
	top := make([]any, 0, min(3, len(r.Top)))
	for _, score := range r.Top[:min(3, len(r.Top))] {
		top = append(top, []any{score.Domain, score.Score})
	}
	return s.runWriteTxLocked(ctx, func(tx neo4j.ManagedTransaction) error {
		status, err := txDocumentStatus(ctx, tx, a.DocID)
		if err != nil {
			return err
		}
		if status == StatusSuperseded {
			return ErrSuperseded
		}
		tok := domain.DomainToken(a.Domain)
		result, err := tx.Run(ctx, `MATCH (n:Domain {tok:$tok}) WHERE n.id <> $id RETURN n.id AS id`, map[string]any{"tok": tok, "id": a.Domain})
		if err != nil {
			return err
		}
		collisions, err := result.Collect(ctx)
		if err != nil {
			return err
		}
		if len(collisions) > 0 {
			return fmt.Errorf("graph: domain token collision: %s and %s", a.Domain, getString(collisions[0], "id"))
		}
		if err := txRun(ctx, tx, `
MERGE (n:Domain {id:$domain})
ON CREATE SET n.name=$name,n.created_at=$at,n.created_by=$by
SET n.tok=$tok
WITH n
MATCH (d:Document {id:$doc})
SET d.domain=$domain,d.domain_tok=$tok,d.name_profile=$profile`, map[string]any{"domain": a.Domain, "name": a.DomainName, "at": r.At.UTC().Format(timeLayout), "by": a.CreatedBy, "tok": tok, "doc": a.DocID, "profile": profileProperties(a.Profile)}); err != nil {
			return err
		}
		if err := txRun(ctx, tx, `MATCH (:Document {id:$doc})-[:HAS_CHUNK]->(c:Chunk) SET c.domain=$domain,c.domain_tok=$tok`, map[string]any{"doc": a.DocID, "domain": a.Domain, "tok": tok}); err != nil {
			return err
		}
		if err := txRun(ctx, tx, `
CREATE (:DomainDecision {op_id:$op,doc_id:$doc,domain:$domain,previous_domain:$previous,
action:$action,by:$by,top:$top,params:$params,note:$note,at:$at})`, map[string]any{
			"op": r.OpID, "doc": r.DocID, "domain": r.Domain, "previous": r.PreviousDomain, "action": r.Action, "by": r.By, "top": top,
			"params": map[string]any{"tau": r.Params.Tau, "delta": r.Params.Delta, "tau_override": r.Params.TauOverride}, "note": r.Note, "at": r.At.UTC().Format(timeLayout),
		}); err != nil {
			return err
		}
		if beforeCommit != nil {
			return beforeCommit()
		}
		return nil
	})
}

// UpdateNameProfile refreshes profiles after all extraction writes, including
// replacements for duplicates extracted in their own domain on the second pass.
func (s *Store) UpdateNameProfile(ctx context.Context, docID string, p domain.Profile) error {
	return s.runWriteTxLocked(ctx, func(tx neo4j.ManagedTransaction) error {
		status, err := txDocumentStatus(ctx, tx, docID)
		if err != nil {
			return err
		}
		if status == StatusSuperseded {
			return ErrSuperseded
		}
		return txRun(ctx, tx, `MATCH (d:Document {id:$doc}) SET d.name_profile=$profile`, map[string]any{"doc": docID, "profile": profileProperties(p)})
	})
}

// MentionNamesForChunks returns the extraction-bearing originals' entity names.
func (s *Store) MentionNamesForChunks(ctx context.Context, chunkIDs []string) (map[string][]string, error) {
	out := make(map[string][]string, len(chunkIDs))
	if len(chunkIDs) == 0 {
		return out, nil
	}
	rows, err := s.runRead(ctx, `UNWIND $ids AS id MATCH (c:Chunk {id:id})-[:MENTIONS]->(e:Entity) RETURN DISTINCT c.id AS id,e.name AS name ORDER BY id,name`, map[string]any{"ids": chunkIDs})
	if err != nil {
		return nil, fmt.Errorf("graph: duplicate mention names: %w", err)
	}
	for _, r := range rows {
		id := getString(r, "id")
		out[id] = append(out[id], getString(r, "name"))
	}
	return out, nil
}

// ClearDuplicateOf makes chunks eligible for normal extraction again.
func (s *Store) ClearDuplicateOf(ctx context.Context, chunkIDs []string) error {
	if len(chunkIDs) == 0 {
		return nil
	}
	return s.runWrite(ctx, `UNWIND $ids AS id MATCH (c:Chunk {id:id}) SET c.duplicate_of=''`, map[string]any{"ids": chunkIDs})
}
