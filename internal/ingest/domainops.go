package ingest

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/alessiosavi/go-knowledge-graph/internal/graph"
)

// DomainOpRequest describes an admin move or partition merge inside serve.
type DomainOpRequest struct {
	OpID, Action string
	DocIDs       []string
	Target, From string
}

// Kept separate from pipelineStore: ordinary ingestion fakes do not administer
// domains. Production's graph.Store implements both interfaces.
type domainOpStore interface {
	SchemaVersion(context.Context) (int, error)
	DomainOperation(context.Context, string) (graph.DomainOpPlan, error)
	CreateDomainOp(context.Context, graph.DomainOpPlan) (graph.DomainOpPlan, error)
	SetDomainOpResult(context.Context, graph.DomainOp) error
	DomainDocumentIDs(context.Context, string) ([]string, error)
	DomainDecisions(context.Context, string) ([]graph.DecisionRecord, error)
	MoveDocument(context.Context, string, string, string, string, string) error
	DeleteEmptyDomain(context.Context, string) (bool, error)
}

func (p *Pipeline) domainStore(ctx context.Context) (domainOpStore, error) {
	st, ok := p.store.(domainOpStore)
	if !ok {
		return nil, fmt.Errorf("%w: domain operations unavailable", ErrStore)
	}
	v, err := st.SchemaVersion(ctx)
	if err != nil {
		return nil, err
	}
	if v < graph.SchemaVersionV4 {
		return nil, graph.ErrSchemaNotV4
	}
	return st, nil
}

func (p *Pipeline) RunDomainOp(ctx context.Context, req DomainOpRequest) (graph.DomainOp, error) {
	p.domainOpsMu.Lock()
	defer p.domainOpsMu.Unlock()
	st, err := p.domainStore(ctx)
	if err != nil {
		return graph.DomainOp{}, err
	}
	if (req.Action != "move" && req.Action != "merge") || !graph.ValidDomain(req.Target) || (req.Action == "merge" && (!graph.ValidDomain(req.From) || req.From == req.Target)) || (req.Action == "move" && len(req.DocIDs) == 0) {
		return graph.DomainOp{}, graph.ErrInvalidDomainOp
	}
	if req.OpID == "" {
		req.OpID = rand.Text()
	}
	plan, err := st.DomainOperation(ctx, req.OpID)
	if err == nil {
		if plan.Op.Action != req.Action || plan.Target != req.Target || plan.From != req.From {
			return plan.Op, graph.ErrInvalidDomainOp
		}
		if req.Action == "move" {
			ids := slices.Clone(req.DocIDs)
			slices.Sort(ids)
			ids = slices.Compact(ids)
			stored := slices.Clone(plan.Op.Docs)
			slices.Sort(stored)
			if !slices.Equal(ids, stored) {
				return plan.Op, graph.ErrInvalidDomainOp
			}
		}
		if req.Action == "merge" && plan.Op.State == "incomplete" {
			ids, err := st.DomainDocumentIDs(ctx, plan.From)
			if err != nil {
				return plan.Op, err
			}
			for _, id := range ids {
				if _, exists := plan.Targets[id]; !exists {
					plan.Op.Docs = append(plan.Op.Docs, id)
					plan.Targets[id] = plan.Target
				}
			}
		}
		return p.executeDomainOp(ctx, st, plan)
	}
	if !errors.Is(err, graph.ErrNotFound) {
		return graph.DomainOp{}, err
	}
	ids := slices.Clone(req.DocIDs)
	if req.Action == "merge" {
		// A merge requires both existing partitions; moves may create a target.
		if _, err := st.DomainDocumentIDs(ctx, req.Target); err != nil {
			return graph.DomainOp{}, err
		}
		ids, err = st.DomainDocumentIDs(ctx, req.From)
		if err != nil {
			return graph.DomainOp{}, err
		}
	}
	seen := map[string]bool{}
	docs := []string{}
	targets := map[string]string{}
	for _, id := range ids {
		if id == "" {
			return graph.DomainOp{}, graph.ErrInvalidDomainOp
		}
		if !seen[id] {
			docs = append(docs, id)
			targets[id] = req.Target
			seen[id] = true
		}
	}
	plan = graph.DomainOpPlan{Op: graph.DomainOp{OpID: req.OpID, Action: req.Action, State: "running", Docs: docs, At: time.Now().UTC()}, Targets: targets, Target: req.Target, From: req.From}
	return p.executeDomainOp(ctx, st, plan)
}

func (p *Pipeline) executeDomainOp(ctx context.Context, st domainOpStore, plan graph.DomainOpPlan) (op graph.DomainOp, err error) {
	op = plan.Op
	if op.State == "completed" {
		return op, nil
	}
	records, err := st.DomainDecisions(ctx, op.OpID)
	if err != nil {
		return op, err
	}
	moved := map[string]bool{}
	for _, r := range records {
		moved[r.DocID] = true
	}
	for _, id := range op.Skipped {
		moved[id] = true
	}

	// Admit the whole pending snapshot before any graph mutation. Hold document
	// locks through execution; domainMu is acquired only after these locks and
	// released between moves so detection never reverses the lock order.
	ids := slices.Clone(op.Docs)
	slices.Sort(ids)
	locks := []*sync.Mutex{}
	defer func() {
		for i := len(locks) - 1; i >= 0; i-- {
			locks[i].Unlock()
		}
	}()
	statuses := map[string]graph.Status{}
	blocked := []string{}
	for _, id := range ids {
		if moved[id] {
			continue
		}
		lock := p.docLock(id)
		if !lock.TryLock() {
			blocked = append(blocked, id)
			continue
		}
		locks = append(locks, lock)
		doc, err := p.store.Document(ctx, id)
		if err != nil {
			return op, err
		}
		statuses[id] = doc.Status
		if doc.Status != graph.StatusDone && doc.Status != graph.StatusSuperseded {
			blocked = append(blocked, id)
		}
	}
	if len(blocked) > 0 {
		return op, fmt.Errorf("%w: documents: %s", graph.ErrNotDone, strings.Join(blocked, ", "))
	}
	stored, err := st.CreateDomainOp(ctx, plan)
	if err != nil {
		return op, err
	}
	op = stored.Op
	op.State, op.Remaining = "running", nil
	if err = st.SetDomainOpResult(ctx, op); err != nil {
		return op, err
	}
	queued := []string{}
	defer func() {
		// Committed live resets remain queued even if a later write fails.
		p.mu.Lock()
		queueCtx := p.queueCtx
		p.mu.Unlock()
		if len(queued) > 0 {
			go func() {
				for _, id := range queued {
					p.enqueue(queueCtx, id)
				}
			}()
		}
		if err != nil {
			op.State = "failed"
			err = errors.Join(err, st.SetDomainOpResult(context.WithoutCancel(ctx), op))
		}
	}()
	for _, id := range op.Docs {
		if moved[id] {
			continue
		}
		if op.Action == "undo" && statuses[id] == graph.StatusSuperseded {
			op.Skipped = append(op.Skipped, id)
			continue
		}
		p.domainMu.Lock()
		moveErr := st.MoveDocument(ctx, id, plan.Targets[id], op.OpID, op.Action, "admin")
		p.domainMu.Unlock()
		if op.Action == "undo" && errors.Is(moveErr, graph.ErrSuperseded) {
			op.Skipped = append(op.Skipped, id)
			continue
		}
		if moveErr != nil {
			return op, moveErr
		}
		if statuses[id] == graph.StatusDone {
			queued = append(queued, id)
		}
	}
	if plan.From != "" {
		p.domainMu.Lock()
		deleted, deleteErr := st.DeleteEmptyDomain(ctx, plan.From)
		if deleteErr == nil && !deleted {
			op.Remaining, deleteErr = st.DomainDocumentIDs(ctx, plan.From)
			// A previous attempt may have deleted the source before recording
			// completion. Its absence also satisfies merge completion.
			if errors.Is(deleteErr, graph.ErrNotFound) {
				deleted, deleteErr = true, nil
			}
		}
		p.domainMu.Unlock()
		if deleteErr != nil {
			return op, deleteErr
		}
		if !deleted {
			op.State = "incomplete"
			err = st.SetDomainOpResult(ctx, op)
			return op, err
		}
	}
	op.State = "completed"
	err = st.SetDomainOpResult(ctx, op)
	return op, err
}

// UndoDomainOp moves every document back under a new, resumable operation ID.
// Live members must be done before any reset; superseded members are reported
// as skipped and retain their current domain.
func (p *Pipeline) UndoDomainOp(ctx context.Context, opID string) (graph.DomainOp, error) {
	p.domainOpsMu.Lock()
	defer p.domainOpsMu.Unlock()
	st, err := p.domainStore(ctx)
	if err != nil {
		return graph.DomainOp{}, err
	}
	original, err := st.DomainOperation(ctx, opID)
	if err != nil {
		return graph.DomainOp{}, err
	}
	if original.Op.State != "completed" {
		return original.Op, graph.ErrNotDone
	}
	if original.Op.Action != "move" && original.Op.Action != "merge" && original.Op.Action != "undo" {
		return original.Op, graph.ErrInvalidDomainOp
	}
	undoID := opID + "-undo"
	if existing, err := st.DomainOperation(ctx, undoID); err == nil {
		if existing.Op.Action != "undo" || existing.UndoOf != opID {
			return existing.Op, graph.ErrInvalidDomainOp
		}
		return p.executeDomainOp(ctx, st, existing)
	} else if !errors.Is(err, graph.ErrNotFound) {
		return graph.DomainOp{}, err
	}
	ids := slices.DeleteFunc(slices.Clone(original.Op.Docs), func(id string) bool {
		return slices.Contains(original.Op.Skipped, id)
	})
	slices.Sort(ids)
	records, err := st.DomainDecisions(ctx, opID)
	if err != nil {
		return graph.DomainOp{}, err
	}
	targets := map[string]string{}
	for _, r := range records {
		if graph.ValidDomain(r.PreviousDomain) {
			targets[r.DocID] = r.PreviousDomain
		}
	}
	if len(targets) != len(ids) {
		return graph.DomainOp{}, fmt.Errorf("%w: incomplete undo ledger", graph.ErrInvalidDomainOp)
	}
	plan := graph.DomainOpPlan{Op: graph.DomainOp{OpID: undoID, Action: "undo", State: "running", Docs: ids, At: time.Now().UTC()}, Targets: targets, UndoOf: opID}
	return p.executeDomainOp(ctx, st, plan)
}
