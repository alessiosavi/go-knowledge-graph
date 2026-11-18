package graph

import (
	"context"
	"fmt"
	"time"

	"github.com/neo4j/neo4j-go-driver/v6/neo4j"
)

// JudgeVerdict survives extraction resets and migrations so an unchanged
// request can be judged again without another provider call. DocID is the
// document that first asked: deleting that document deletes the verdict,
// so no document-derived text outlives its document.
type JudgeVerdict struct {
	Key, Status, Reason, Model, PromptVersion, DocID string
	Confidence                                       float64
	At                                               time.Time
}

// JudgeVerdict returns a cached verdict and whether its key exists.
func (s *Store) JudgeVerdict(ctx context.Context, key string) (JudgeVerdict, bool, error) {
	recs, err := s.runRead(ctx, `
MATCH (v:JudgeVerdict {key: $key})
RETURN v.key AS key, v.status AS status, v.reason AS reason, v.model AS model,
       v.prompt_version AS prompt_version, coalesce(v.confidence, 0.0) AS confidence, v.at AS at,
       coalesce(v.doc_id, '') AS doc_id`, map[string]any{"key": key})
	if err != nil {
		return JudgeVerdict{}, false, fmt.Errorf("graph: read judge verdict: %w", err)
	}
	if len(recs) == 0 {
		return JudgeVerdict{}, false, nil
	}
	r := recs[0]
	// The time is informational: a node without a parsable one still
	// serves its verdict (zero At) instead of making the key unreadable.
	at, _ := time.Parse(timeLayout, getString(r, "at"))
	return JudgeVerdict{
		Key: getString(r, "key"), Status: getString(r, "status"), Reason: getString(r, "reason"),
		Model: getString(r, "model"), PromptVersion: getString(r, "prompt_version"), DocID: getString(r, "doc_id"),
		Confidence: getFloat(r, "confidence"), At: at,
	}, true, nil
}

// PutJudgeVerdict merges a successful verdict by its complete request key.
// The first writer's DocID is kept; a DocID naming a missing document
// writes nothing.
func (s *Store) PutJudgeVerdict(ctx context.Context, v JudgeVerdict) error {
	return s.putJudgeVerdict(ctx, v, false)
}

// PutJudgeVerdictStrict requires the verdict to have been written. Rejudge
// keeps the paid result in its local ledger if the document has disappeared.
func (s *Store) PutJudgeVerdictStrict(ctx context.Context, v JudgeVerdict) error {
	return s.putJudgeVerdict(ctx, v, true)
}

func (s *Store) putJudgeVerdict(ctx context.Context, v JudgeVerdict, strict bool) error {
	if v.Key == "" {
		return fmt.Errorf("graph: judge verdict key is empty")
	}
	switch v.Status {
	case "confirmed", "unclear", "refuted":
	default:
		return fmt.Errorf("graph: invalid judge verdict status %q", v.Status)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	// A verdict for a document that no longer exists (deleted while its
	// judge call was in flight) is not written: nothing could delete it.
	// Store.mu orders this write against DeleteDocument in this process.
	const query = `
OPTIONAL MATCH (d:Document {id: $doc_id})
WITH d WHERE $doc_id = '' OR d IS NOT NULL
MERGE (v:JudgeVerdict {key: $key})
SET v.status = $status, v.reason = $reason, v.model = $model, v.confidence = $confidence,
    v.prompt_version = $prompt_version, v.at = $at, v.doc_id = coalesce(v.doc_id, $doc_id)`
	params := map[string]any{
		"doc_id": v.DocID, "key": v.Key, "status": v.Status, "reason": v.Reason, "model": v.Model, "confidence": v.Confidence,
		"prompt_version": v.PromptVersion, "at": v.At.UTC().Format(timeLayout),
	}
	var err error
	if strict {
		err = s.runWriteTx(ctx, func(tx neo4j.ManagedTransaction) error {
			res, err := tx.Run(ctx, query+"\nRETURN count(v) AS n", params)
			if err != nil {
				return err
			}
			rows, err := res.Collect(ctx)
			if err != nil {
				return err
			}
			if len(rows) != 1 || getInt(rows[0], "n") != 1 {
				return fmt.Errorf("expected one verdict write for document %q", v.DocID)
			}
			return nil
		})
	} else {
		err = s.runWrite(ctx, query, params)
	}
	if err != nil {
		return fmt.Errorf("graph: put judge verdict: %w", err)
	}
	return nil
}
