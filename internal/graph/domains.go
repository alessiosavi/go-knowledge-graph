package graph

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"reflect"
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
	// NewDomain is informational for the caller (it picks DomainName for a
	// new domain); AssignDomain itself creates the Domain node by MERGE.
	NewDomain bool
	Profile   domain.Profile
	Decision  DecisionRecord
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
		profile, err := storedProfile(r)
		if err != nil {
			return nil, err
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

// AssignDomain commits the first assignment. The v4 migration uses the same
// implementation with writers stopped. Callers that can change an assigned
// domain must use the guarded MoveDocument path.
func (s *Store) AssignDomain(ctx context.Context, a Assignment) error {
	return s.assignDomain(ctx, a, nil)
}

func (s *Store) assignDomain(ctx context.Context, a Assignment, beforeCommit func() error) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.assignDomainUnlocked(ctx, a, beforeCommit)
}

// assignDomainUnlocked is also used while migration owns Store.mu.
func (s *Store) assignDomainUnlocked(ctx context.Context, a Assignment, beforeCommit func() error) error {
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
	return s.runWriteTx(ctx, func(tx neo4j.ManagedTransaction) error {
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
	rows, err := s.runRead(ctx, `UNWIND $ids AS id MATCH (c:Chunk {id:id})-[:MENTIONS]->(e:Entity) WHERE NOT e.key STARTS WITH '#' RETURN DISTINCT c.id AS id,e.name AS name ORDER BY id,name`, map[string]any{"ids": chunkIDs})
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

var (
	ErrSchemaNotV4       = errors.New("graph: schema_not_v4")
	ErrInvalidDomainOp   = errors.New("graph: invalid domain operation")
	ErrDomainHasProducts = errors.New("graph: domain_has_products")
)

// DomainOp records scheduling progress; completed means every live reset was
// queued and a merge source was removed. Incomplete merges list remaining IDs.
// Undo additionally checks that live extraction jobs have finished.
type DomainOp struct {
	OpID, Action, State string
	Docs                []string
	Skipped             []string `json:"skipped,omitempty"`
	Remaining           []string `json:"remaining,omitempty"`
	At                  time.Time
}

// DomainOpPlan persists immutable targets for admitted documents. An incomplete
// merge can append new source members on retry.
type DomainOpPlan struct {
	Op                   DomainOp
	Targets              map[string]string
	Target, From, UndoOf string
}

func txRequireDomains(ctx context.Context, tx neo4j.ManagedTransaction) error {
	res, err := tx.Run(ctx, `MATCH (m:GkgMeta {id:$id}) RETURN m.value AS version`, map[string]any{"id": extractionSchemaMetaID})
	if err != nil {
		return err
	}
	rows, err := res.Collect(ctx)
	if err != nil {
		return err
	}
	if len(rows) == 0 || getInt(rows[0], "version") < SchemaVersionV4 {
		return ErrSchemaNotV4
	}
	return nil
}

const domainBindingQuery = `
MATCH (s:ProductStore)
WHERE s.domain IS NOT NULL AND (s.domain IN $domains
   OR EXISTS { MATCH (d:Document {domain:s.domain}) WHERE d.id IN $docs })
RETURN s.domain AS domain, s.store AS store ORDER BY domain, store LIMIT 1`

func domainBindingError(rows []*neo4j.Record, err error) error {
	if err != nil {
		return err
	}
	if len(rows) > 0 {
		return fmt.Errorf("%w: domain %q is bound to product store %q", ErrDomainHasProducts,
			getString(rows[0], "domain"), getString(rows[0], "store"))
	}
	return nil
}

// RequireUnboundDomains checks explicit slugs and the current domains of
// pending documents in one snapshot before admitting an operation.
func (s *Store) RequireUnboundDomains(ctx context.Context, domains, docIDs []string) error {
	return domainBindingError(s.runRead(ctx, domainBindingQuery, map[string]any{"domains": domains, "docs": docIDs}))
}

// txGuardDomainMutation rechecks the binding at write time. Writes to the
// shared Domain nodes serialize with release binding under snapshot isolation.
func txGuardDomainMutation(ctx context.Context, tx neo4j.ManagedTransaction, domains ...string) error {
	params := map[string]any{"domains": domains, "docs": []string{}}
	if err := domainBindingError(txRows(ctx, tx, domainBindingQuery, params)); err != nil {
		return err
	}
	return txRun(ctx, tx, `MATCH (d:Domain) WHERE d.id IN $domains
SET d.product_guard=coalesce(d.product_guard,0)+1`, params)
}

// MoveDocument publishes a new partition and ledger in one transaction. Live
// documents reset extraction; superseded versions only update domain properties.
// The pipeline holds the document lock until this commits.
func (s *Store) MoveDocument(ctx context.Context, docID, slug, opID, action, by string) error {
	if !ValidDomain(slug) {
		return fmt.Errorf("%w: invalid domain slug %q", ErrInvalidDomainOp, slug)
	}
	return s.runWriteTxLocked(ctx, func(tx neo4j.ManagedTransaction) error {
		if err := txRequireDomains(ctx, tx); err != nil {
			return err
		}
		res, err := tx.Run(ctx, `MATCH (r:DomainDecision {op_id:$op,doc_id:$doc}) RETURN r.domain AS domain`, map[string]any{"op": opID, "doc": docID})
		if err != nil {
			return err
		}
		rows, err := res.Collect(ctx)
		if err != nil {
			return err
		}
		if len(rows) > 0 {
			if getString(rows[0], "domain") != slug {
				return ErrInvalidDomainOp
			}
			return nil
		}
		res, err = tx.Run(ctx, `MATCH (d:Document {id:$doc}) RETURN d.domain AS domain,d.status AS status`, map[string]any{"doc": docID})
		if err != nil {
			return err
		}
		rows, err = res.Collect(ctx)
		if err != nil {
			return err
		}
		if len(rows) == 0 {
			return ErrNotFound
		}
		previous := getString(rows[0], "domain")
		status := Status(getString(rows[0], "status"))
		if err := txGuardDomainMutation(ctx, tx, previous, slug); err != nil {
			return err
		}
		if status == StatusSuperseded {
			if action == "undo" {
				return ErrSuperseded
			}
		} else if err := txResetExtraction(ctx, tx, docID, ""); err != nil {
			return err
		}
		if !ValidDomain(previous) {
			return fmt.Errorf("%w: document has no valid previous domain", ErrInvalidDomainOp)
		}
		tok := domain.DomainToken(slug)
		res, err = tx.Run(ctx, `MATCH (n:Domain {tok:$tok}) WHERE n.id<>$domain RETURN n.id`, map[string]any{"tok": tok, "domain": slug})
		if err != nil {
			return err
		}
		rows, err = res.Collect(ctx)
		if err != nil {
			return err
		}
		if len(rows) > 0 {
			return fmt.Errorf("graph: domain token collision for %s", slug)
		}
		params := map[string]any{"doc": docID, "domain": slug, "tok": tok, "op": opID, "action": action, "by": by, "previous": previous, "at": time.Now().UTC().Format(timeLayout)}
		for _, q := range []string{
			`MERGE (n:Domain {id:$domain}) ON CREATE SET n.name=$domain,n.created_at=$at,n.created_by='admin',n.product_guard=1 SET n.tok=$tok`,
			`MATCH (d:Document {id:$doc}) SET d.domain=$domain,d.domain_tok=$tok`,
			`MATCH (:Document {id:$doc})-[:HAS_CHUNK]->(c:Chunk) SET c.domain=$domain,c.domain_tok=$tok`,
			`CREATE (:DomainDecision {op_id:$op,doc_id:$doc,domain:$domain,previous_domain:$previous,action:$action,by:$by,at:$at,top:[],params:{},note:''})`,
		} {
			if err := txRun(ctx, tx, q, params); err != nil {
				return err
			}
		}
		return nil
	})
}

// DomainDecisions reads an operation's ledger, or the full ledger for show.
func (s *Store) DomainDecisions(ctx context.Context, opID string) ([]DecisionRecord, error) {
	rows, err := s.runRead(ctx, `MATCH (r:DomainDecision) WHERE $op='' OR r.op_id=$op RETURN r.op_id AS op,r.doc_id AS doc,r.domain AS domain,r.previous_domain AS previous,r.action AS action,r.by AS by,r.at AS at,r.note AS note,r.top AS top,r.params AS params ORDER BY at,doc`, map[string]any{"op": opID})
	if err != nil {
		return nil, err
	}
	out := make([]DecisionRecord, 0, len(rows))
	for _, r := range rows {
		d := DecisionRecord{OpID: getString(r, "op"), DocID: getString(r, "doc"), Domain: getString(r, "domain"), PreviousDomain: getString(r, "previous"), Action: getString(r, "action"), By: getString(r, "by"), At: parseTime(getString(r, "at")), Note: getString(r, "note")}
		if v, _ := r.Get("top"); v != nil {
			if list, ok := v.([]any); ok {
				for _, v := range list {
					if pair, ok := v.([]any); ok && len(pair) == 2 {
						slug, _ := pair[0].(string)
						score, _ := pair[1].(float64)
						d.Top = append(d.Top, domain.DomainScore{Domain: slug, Score: score})
					}
				}
			}
		}
		if v, _ := r.Get("params"); v != nil {
			if m, ok := v.(map[string]any); ok {
				d.Params.Tau, _ = m["tau"].(float64)
				d.Params.Delta, _ = m["delta"].(float64)
				d.Params.TauOverride, _ = m["tau_override"].(float64)
			}
		}
		out = append(out, d)
	}
	return out, nil
}

func (s *Store) DomainOperation(ctx context.Context, id string) (DomainOpPlan, error) {
	rows, err := s.runRead(ctx, `MATCH (o:DomainOp {op_id:$id}) RETURN o.action AS action,o.state AS state,o.docs AS docs,o.at AS at,o.targets AS targets,o.target AS target,o.from AS from,o.undo_of AS undo_of,o.skipped AS skipped,o.remaining AS remaining`, map[string]any{"id": id})
	if err != nil {
		return DomainOpPlan{}, err
	}
	if len(rows) == 0 {
		return DomainOpPlan{}, ErrNotFound
	}
	r := rows[0]
	plan := DomainOpPlan{Op: DomainOp{OpID: id, Action: getString(r, "action"), State: getString(r, "state"), Docs: getStringSlice(r, "docs"), Skipped: getStringSlice(r, "skipped"), Remaining: getStringSlice(r, "remaining"), At: parseTime(getString(r, "at"))}, Targets: map[string]string{}, Target: getString(r, "target"), From: getString(r, "from"), UndoOf: getString(r, "undo_of")}
	if v, _ := r.Get("targets"); v != nil {
		if m, ok := v.(map[string]any); ok {
			for doc, slug := range m {
				plan.Targets[doc], _ = slug.(string)
			}
		}
	}
	return plan, nil
}

// CreateDomainOp uses MERGE for crash-safe admission; conflicting reuse fails.
func (s *Store) CreateDomainOp(ctx context.Context, plan DomainOpPlan) (DomainOpPlan, error) {
	targets := make(map[string]any, len(plan.Targets))
	for id, slug := range plan.Targets {
		targets[id] = slug
	}
	err := s.runWriteTxLocked(ctx, func(tx neo4j.ManagedTransaction) error {
		if err := txRequireDomains(ctx, tx); err != nil {
			return err
		}
		return txRun(ctx, tx, `MERGE (o:DomainOp {op_id:$id}) ON CREATE SET o.action=$action,o.state='running',o.docs=$docs,o.at=$at,o.targets=$targets,o.target=$target,o.from=$from,o.undo_of=$undo`, map[string]any{"id": plan.Op.OpID, "action": plan.Op.Action, "docs": plan.Op.Docs, "at": plan.Op.At.UTC().Format(timeLayout), "targets": targets, "target": plan.Target, "from": plan.From, "undo": plan.UndoOf})
	})
	if err != nil {
		return DomainOpPlan{}, err
	}
	stored, err := s.DomainOperation(ctx, plan.Op.OpID)
	if err != nil {
		return stored, err
	}
	if stored.Op.Action != plan.Op.Action || stored.Target != plan.Target || stored.From != plan.From || stored.UndoOf != plan.UndoOf {
		return stored, fmt.Errorf("%w: op_id reused for different targets", ErrInvalidDomainOp)
	}
	if !reflect.DeepEqual(stored.Targets, plan.Targets) {
		// An incomplete merge may admit new source members on retry. Existing
		// targets remain immutable, including those already in its ledger.
		if stored.Op.Action != "merge" || stored.Op.State != "incomplete" {
			return stored, fmt.Errorf("%w: op_id reused for different targets", ErrInvalidDomainOp)
		}
		for id, target := range stored.Targets {
			if plan.Targets[id] != target {
				return stored, ErrInvalidDomainOp
			}
		}
		for _, target := range plan.Targets {
			if target != stored.Target {
				return stored, ErrInvalidDomainOp
			}
		}
		if err := s.runWrite(ctx, `MATCH (o:DomainOp {op_id:$id}) SET o.docs=$docs,o.targets=$targets`, map[string]any{"id": stored.Op.OpID, "docs": plan.Op.Docs, "targets": targets}); err != nil {
			return stored, err
		}
		stored.Op.Docs, stored.Targets = plan.Op.Docs, plan.Targets
	}
	return stored, nil
}
func (s *Store) SetDomainOpState(ctx context.Context, id, state string) error {
	return s.runWrite(ctx, `MATCH (o:DomainOp {op_id:$id}) SET o.state=$state`, map[string]any{"id": id, "state": state})
}

// SetDomainOpResult persists the state and the document IDs explaining it.
func (s *Store) SetDomainOpResult(ctx context.Context, op DomainOp) error {
	return s.runWrite(ctx, `MATCH (o:DomainOp {op_id:$id}) SET o.state=$state,o.skipped=$skipped,o.remaining=$remaining`, map[string]any{"id": op.OpID, "state": op.State, "skipped": op.Skipped, "remaining": op.Remaining})
}

// DomainDocumentIDs includes busy and superseded members so merge cannot
// silently drop a partition that still has documents.
func (s *Store) DomainDocumentIDs(ctx context.Context, slug string) ([]string, error) {
	rows, err := s.runRead(ctx, `MATCH (n:Domain {id:$domain}) OPTIONAL MATCH (d:Document {domain:n.id}) RETURN d.id AS id ORDER BY id`, map[string]any{"domain": slug})
	if err != nil {
		return nil, err
	}
	if len(rows) == 0 {
		return nil, ErrNotFound
	}
	out := []string{}
	for _, r := range rows {
		if id := getString(r, "id"); id != "" {
			out = append(out, id)
		}
	}
	return out, nil
}

// DeleteEmptyDomain checks all document versions and product bindings. Merge
// completion also guards its destination so even an empty merge checks both ends.
func (s *Store) DeleteEmptyDomain(ctx context.Context, slug string, destinations ...string) (bool, error) {
	deleted := false
	err := s.runWriteTxLocked(ctx, func(tx neo4j.ManagedTransaction) error {
		if err := txGuardDomainMutation(ctx, tx, append([]string{slug}, destinations...)...); err != nil {
			return err
		}
		rows, err := txRows(ctx, tx, `
MATCH (n:Domain {id:$domain})
WHERE NOT EXISTS { MATCH (:Document {domain:n.id}) }
  AND NOT EXISTS { MATCH (s:ProductStore {domain:n.id}) WHERE s.domain IS NOT NULL }
DELETE n RETURN count(*) AS deleted`, map[string]any{"domain": slug})
		if err != nil {
			return err
		}
		deleted = len(rows) > 0 && getInt(rows[0], "deleted") > 0
		return nil
	})
	return err == nil && deleted, err
}

// RenameDomain records the display-name change as a completed admin operation.
func (s *Store) RenameDomain(ctx context.Context, slug, name string) (DomainOp, error) {
	op := DomainOp{OpID: rand.Text(), Action: "rename", State: "completed", Docs: []string{}, At: time.Now().UTC()}
	if !ValidDomain(slug) || name == "" {
		return op, fmt.Errorf("%w: invalid domain or empty name", ErrInvalidDomainOp)
	}
	err := s.runWriteTxLocked(ctx, func(tx neo4j.ManagedTransaction) error {
		if err := txRequireDomains(ctx, tx); err != nil {
			return err
		}
		res, err := tx.Run(ctx, `MATCH (n:Domain {id:$domain}) RETURN n.id`, map[string]any{"domain": slug})
		if err != nil {
			return err
		}
		rows, err := res.Collect(ctx)
		if err != nil {
			return err
		}
		if len(rows) == 0 {
			return ErrNotFound
		}
		return txRun(ctx, tx, `MATCH (n:Domain {id:$domain}) SET n.name=$name CREATE (:DomainDecision {op_id:$op,doc_id:'',domain:$domain,previous_domain:$domain,action:'rename',by:'admin',at:$at,note:$name,top:[],params:{}}) CREATE (:DomainOp {op_id:$op,action:'rename',state:'completed',docs:[],targets:{},at:$at})`, map[string]any{"domain": slug, "name": name, "op": op.OpID, "at": op.At.Format(timeLayout)})
	})
	return op, err
}

// ProfilesFromMentions reads every live document in id order. Persisted profiles
// survive interrupted migrations; only missing profiles read legacy mentions.
// Mention reads are bounded to one document at a time. BuildProfile folds names
// and counts distinct chunks, including folded aliases.
func (s *Store) ProfilesFromMentions(ctx context.Context) ([]domain.DocProfile, error) {
	rows, err := s.runRead(ctx, `MATCH (d:Document)
WHERE d.status IS NULL OR d.status <> 'superseded'
RETURN d.id AS id,d.source AS source,d.domain AS domain,d.name_profile AS profile ORDER BY d.id`, nil)
	if err != nil {
		return nil, fmt.Errorf("graph: migration profiles: %w", err)
	}
	out := make([]domain.DocProfile, 0, len(rows))
	for _, r := range rows {
		p := domain.DocProfile{DocID: getString(r, "id"), Source: getString(r, "source"), Domain: getString(r, "domain")}
		if v, _ := r.Get("profile"); v != nil {
			p.Profile, err = storedProfile(r)
			if err != nil {
				return nil, err
			}
		} else {
			mentions, err := s.runRead(ctx, `MATCH (d:Document {id:$id})-[:HAS_CHUNK]->(c:Chunk)-[:MENTIONS]->(e:Entity)
WHERE NOT e.key STARTS WITH '#'
RETURN c.id AS chunk,e.name AS name`, map[string]any{"id": p.DocID})
			if err != nil {
				return nil, fmt.Errorf("graph: migration mentions for %s: %w", p.DocID, err)
			}
			names := make(map[string][]string)
			for _, mention := range mentions {
				chunk := getString(mention, "chunk")
				names[chunk] = append(names[chunk], getString(mention, "name"))
			}
			p.Profile = domain.BuildProfile(names)
		}
		out = append(out, p)
	}
	return out, nil
}

func storedProfile(r *neo4j.Record) (domain.Profile, error) {
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
	return profile, nil
}
