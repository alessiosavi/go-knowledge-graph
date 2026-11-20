package graph

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"

	"github.com/neo4j/neo4j-go-driver/v6/neo4j"
	"github.com/neo4j/neo4j-go-driver/v6/neo4j/dbtype"

	"github.com/alessiosavi/go-knowledge-graph/internal/validity"
)

const (
	ValidityApplyLease = "repair-validity"
	ValidityApplyTTL   = 120 * time.Second
)

// CheckValidityApply checks the graph-wide apply preconditions without writing.
func (s *Store) CheckValidityApply(ctx context.Context) (string, error) {
	var blocker string
	err := s.runReadTx(ctx, func(tx neo4j.ManagedTransaction) error {
		read := func(query string, params map[string]any) ([]*neo4j.Record, error) {
			return txRows(ctx, tx, query, params)
		}
		rows, err := read(`MATCH (d:Document) WHERE NOT coalesce(d.status, '') IN $terminal
RETURN coalesce(d.id, toString(id(d))) AS id, coalesce(d.status, '<missing>') AS status ORDER BY id LIMIT 1`,
			map[string]any{"terminal": terminalStatuses})
		if err != nil {
			return err
		}
		if len(rows) != 0 {
			blocker = fmt.Sprintf("document id=%q is in a non-terminal status (status=%q)", getString(rows[0], "id"), getString(rows[0], "status"))
			return nil
		}
		rows, err = read(`MATCH (l:JudgeLease) WHERE l.expires_at > $now
RETURN coalesce(l.id, toString(id(l))) AS id, l.owner AS owner, l.expires_at AS expires_at ORDER BY id LIMIT 1`,
			map[string]any{"now": time.Now().UnixMilli()})
		if err != nil {
			return err
		}
		if len(rows) != 0 {
			blocker = validityLeaseBlocker(rows[0])
			return nil
		}
		marked, err := findValidityRun(read)
		if marked != "" {
			blocker = marked + " already carries validity_run"
		}
		return err
	})
	return blocker, err
}

func findValidityRun(read rejudgeRead) (string, error) {
	rows, err := read(`MATCH (rf:RejectedFact) WHERE rf.validity_run IS NOT NULL
RETURN coalesce(rf.id, toString(id(rf))) AS id ORDER BY id LIMIT 1`, nil)
	if err != nil {
		return "", err
	}
	if len(rows) != 0 {
		return fmt.Sprintf("RejectedFact id=%q", getString(rows[0], "id")), nil
	}
	rows, err = read(`MATCH ()-[r]->() WHERE r.validity_run IS NOT NULL RETURN id(r) AS id ORDER BY id LIMIT 1`, nil)
	if err != nil {
		return "", err
	}
	if len(rows) != 0 {
		return fmt.Sprintf("edge id=%d", getInt(rows[0], "id")), nil
	}
	return "", nil
}

func validityLeaseBlocker(row *neo4j.Record) string {
	return fmt.Sprintf("a live judge lease exists (name=%q owner=%q expires_at=%s)",
		getString(row, "id"), getString(row, "owner"), getString(row, "expires_at"))
}

// AcquireValidityApplyLease acquires the apply lease and returns a rollback
// that restores its previous state if the lineage report cannot be opened.
func (s *Store) AcquireValidityApplyLease(ctx context.Context, owner string) (rollback func(context.Context) error, blocker string, err error) {
	var previous map[string]any
	acquired, err := s.acquireLease(ctx, ValidityApplyLease, owner, ValidityApplyTTL, false, &previous, []string{BackfillLeaseName}, false)
	if errors.Is(err, ErrLeaseExcluded) {
		return nil, err.Error(), nil
	}
	if err != nil {
		return nil, "", err
	}
	params := map[string]any{"name": ValidityApplyLease, "owner": owner}
	if !acquired {
		rows, err := s.runRead(ctx, `MATCH (l:JudgeLease {id:$name})
RETURN l.id AS id, l.owner AS owner, l.expires_at AS expires_at`, params)
		if err != nil {
			return nil, "", err
		}
		if len(rows) == 0 {
			return nil, fmt.Sprintf("could not acquire judge lease name=%q", ValidityApplyLease), nil
		}
		return nil, validityLeaseBlocker(rows[0]), nil
	}
	return func(ctx context.Context) error {
		if previous == nil {
			return s.ReleaseLease(ctx, ValidityApplyLease, owner)
		}
		params["previous_owner"], params["previous_expires_at"] = previous["owner"], previous["expires_at"]
		return s.runWrite(ctx, `MATCH (l:JudgeLease {id:$name}) WHERE l.owner = $owner
SET l.owner = $previous_owner, l.expires_at = $previous_expires_at`, params)
	}, "", nil
}

// ValidityEdgeCounts counts stored typed edges visible in EntityView.
type ValidityEdgeCounts struct {
	Default       int `json:"default"`
	ConfirmedOnly int `json:"confirmed_only"`
}

func (s *Store) CountValidityEdges(ctx context.Context) (ValidityEdgeCounts, error) {
	rows, err := s.runRead(ctx, `
MATCH (:Entity)-[r]->(:Entity)
RETURN coalesce(sum(CASE WHEN `+typedFactVisibility+` THEN 1 ELSE 0 END), 0) AS visible,
       count(CASE WHEN r.judge = 'confirmed' THEN 1 END) AS confirmed`,
		map[string]any{"include": false, "confirmed_only": false})
	if err != nil {
		return ValidityEdgeCounts{}, err
	}
	return ValidityEdgeCounts{getInt(rows[0], "visible"), getInt(rows[0], "confirmed")}, nil
}

// ValidityWrite carries an approved outcome and its provenance into a row transaction.
type ValidityWrite struct {
	Source                       RejudgeIdentity
	DocID                        string
	Outcome                      validity.Outcome
	ValidityRun, Rule, Reason    string
	Judge, JudgeRun, JudgePrompt string
	Model                        string
	Destination                  *validity.DestinationKey
	HeadSurface                  string
}

const validityLogicalEdge = `
MATCH (h:Entity {key:$head})-[r]->(t:Entity {key:$tail})
WHERE type(r) = $rel AND coalesce(r.chunk_id, '') = $chunk`

// The explicit predicates also reject lossy numeric conversions in hydration.
const validityExactEdge = validityLogicalEdge + `
  AND id(r) = $edge
  AND coalesce(r.head_start, 0) = $hs AND coalesce(r.head_end, 0) = $he
  AND coalesce(r.tail_start, 0) = $ts AND coalesce(r.tail_end, 0) = $te
  AND coalesce(r.confidence, 0) = $confidence AND coalesce(r.low_confidence, false) = $low
  AND coalesce(r.extractor_version, '') = $version AND r.judge = $prior
  AND properties(r) = $before`

// ApplyValidityFact fences and checks one source and applies any approved edge changes in the same transaction.
func (s *Store) ApplyValidityFact(ctx context.Context, owner string, first bool, w ValidityWrite, check func(RejudgeFact, CatalogReader) error) error {
	return s.runLeaseWrite(ctx, true, func(tx neo4j.ManagedTransaction) error {
		ok, err := txRenewFencedLease(ctx, tx, ValidityApplyLease, owner, ValidityApplyTTL)
		if err != nil {
			return err
		}
		if !ok {
			return errors.New("repair-validity lease lost")
		}
		read := func(query string, params map[string]any) ([]*neo4j.Record, error) {
			return txRows(ctx, tx, query, params)
		}
		if first {
			// Another apply could have finished between preflight and acquisition.
			marked, err := findValidityRun(read)
			if err != nil {
				return err
			}
			if marked != "" {
				return fmt.Errorf("validity_run appeared after preflight on %s", marked)
			}
		}

		f := w.Source
		params := map[string]any{"head": f.HeadKey, "rel": f.Relation, "tail": f.TailKey, "chunk": f.ChunkID}
		rows, err := read(validityLogicalEdge+`
OPTIONAL MATCH (d:Document {id:r.doc_id})
RETURN id(r) AS edge, coalesce(h.domain, '') AS domain, d IS NOT NULL AS has_doc`, params)
		if err != nil {
			return err
		}
		if len(rows) != 1 {
			return fmt.Errorf("source logical key matched %d edges, expected 1", len(rows))
		}
		edge := getInt(rows[0], "edge")
		facts, err := readRejudgeFacts(read, []map[string]any{{
			"id": f.ID, "edge": edge, "head_key": f.HeadKey, "rel": f.Relation, "tail_key": f.TailKey,
			"chunk_id": f.ChunkID, "domain": getString(rows[0], "domain"), "idx": 0,
		}})
		if err != nil {
			return err
		}
		if len(facts) != 1 || facts[0].RejudgeIdentity != f || facts[0].DocID != w.DocID {
			return errors.New("source no longer matches the sealed edge identity")
		}
		fact := facts[0]
		if fact.Before["validity_run"] != nil {
			return errors.New("source already carries validity_run")
		}
		audit := w.Outcome == validity.OutcomeReject || w.Outcome == validity.OutcomeValidityDrop
		if audit && !getBool(rows[0], "has_doc") {
			// A missing document makes the counted audit ineligible. The
			// transaction must roll back even if the audit unexpectedly matches.
			if err := writeValidityAudit(read, fact, w); err != nil {
				return err
			}
			return errors.New("audit document changed during the transaction")
		}
		if err := check(fact, CatalogReader{read}); err != nil {
			return err
		}

		params["edge"], params["before"] = edge, fact.Before
		params["hs"], params["he"], params["ts"], params["te"] = f.HeadStart, f.HeadEnd, f.TailStart, f.TailEnd
		params["confidence"], params["low"], params["version"], params["prior"] = f.Confidence, f.LowConfidence, f.ExtractorVersion, f.Judge
		switch w.Outcome {
		case validity.OutcomeDeferred:
			return nil
		case validity.OutcomeReject, validity.OutcomeValidityDrop:
			if err := writeValidityAudit(read, fact, w); err != nil {
				return err
			}
		case validity.OutcomeStamp, validity.OutcomeDemote:
			props := map[string]any{
				"judge": w.Judge, "judge_run": w.JudgeRun, "judge_prompt": w.JudgePrompt,
				"judge_prior": f.Judge, "validity_run": w.ValidityRun,
			}
			if w.Model != "" {
				props["judge_model"] = w.Model
			}
			params["props"] = props
			return validityWriteOne(read, "update source edge", validityExactEdge+`
SET r += $props
RETURN count(r) AS n`, params)
		case validity.OutcomeRewriteConfirmed, validity.OutcomeRewriteUnclear:
			if err := writeValidityRewrite(read, fact, w); err != nil {
				return err
			}
		default:
			return fmt.Errorf("invalid write outcome %q", w.Outcome)
		}
		return validityWriteOne(read, "delete source edge", validityExactEdge+` DELETE r RETURN count(*) AS n`, params)
	})
}

func validityWriteOne(read rejudgeRead, action, query string, params map[string]any) error {
	rows, err := read(query, params)
	if err != nil {
		return fmt.Errorf("%s: %w", action, err)
	}
	n := 0
	if len(rows) == 1 {
		n = getInt(rows[0], "n")
	}
	if n != 1 {
		return fmt.Errorf("%s count %d, expected 1", action, n)
	}
	return nil
}

func writeValidityAudit(read rejudgeRead, fact RejudgeFact, w ValidityWrite) error {
	rows, err := read(`MATCH (rf:RejectedFact {id:$id}) RETURN properties(rf) AS prior, rf.judge_model AS model`, map[string]any{"id": fact.ID})
	if err != nil {
		return err
	}
	var prior any
	model := judgeModelOrNil(w.Model)
	switch len(rows) {
	case 0:
	case 1:
		props, _ := rows[0].Get("prior")
		raw, err := json.Marshal(validityJSONProperty(props))
		if err != nil {
			return fmt.Errorf("encode prior audit: %w", err)
		}
		prior = string(raw)
		if model == nil {
			model, _ = rows[0].Get("model")
		}
	default:
		return fmt.Errorf("source has %d prior RejectedFact rows, expected at most 1", len(rows))
	}
	before, err := json.Marshal(validityJSONProperty(fact.Before))
	if err != nil {
		return fmt.Errorf("encode edge before-image: %w", err)
	}
	props := factAuditProperties(FactAudit{
		HeadKey: fact.HeadKey, Rel: fact.Relation, TailKey: fact.TailKey, DocID: fact.DocID, ChunkID: fact.ChunkID,
		Score: fact.Confidence, JudgeVerdict: w.Judge, Reason: w.Reason, ExtractorVersion: fact.ExtractorVersion,
	})
	props["id"], props["validity_rule"], props["validity_run"] = fact.ID, w.Rule, w.ValidityRun
	props["judge_run"], props["judge_prompt"], props["judge_model"] = w.JudgeRun, w.JudgePrompt, model
	props["edge_before"], props["prior_audit"] = string(before), prior
	return validityWriteOne(read, "write RejectedFact", `
MATCH (:Document {id:$doc})
MERGE (rf:RejectedFact {id:$id})
SET rf = $props
RETURN count(rf) AS n`, map[string]any{"doc": fact.DocID, "id": fact.ID, "props": props})
}

func writeValidityRewrite(read rejudgeRead, fact RejudgeFact, w ValidityWrite) error {
	k := w.Destination
	params := map[string]any{"head": k.HeadKey, "rel": k.Rel, "tail": k.TailKey, "chunk": k.ChunkID}
	rows, err := read(validityLogicalEdge+` RETURN count(r) AS n`, params)
	if err != nil {
		return err
	}
	if n := getInt(rows[0], "n"); n != 0 {
		return fmt.Errorf("rewrite destination matched %d edges, expected 0", n)
	}
	props := map[string]any{
		"head_key": k.HeadKey, "tail_key": k.TailKey, "doc_id": fact.DocID, "chunk_id": fact.ChunkID,
		"head_surface": w.HeadSurface, "reattached_from": fact.ID, "validity_rule": w.Rule, "validity_run": w.ValidityRun,
		"judge": w.Judge, "judge_run": w.JudgeRun, "judge_prompt": w.JudgePrompt,
		"judge_model": judgeModelOrNil(w.Model),
	}
	for _, key := range []string{"head_start", "head_end", "tail_start", "tail_end", "confidence", "low_confidence", "extractor_version"} {
		if value, ok := fact.Before[key]; ok {
			props[key] = value
		}
	}
	params["props"] = props
	return validityWriteOne(read, "create rewrite edge", `
MATCH (h:Entity {key:$head}), (t:Entity {key:$tail})
WITH h, t, {rel:$rel} AS k
CREATE (h)-[r:k.rel]->(t)
SET r = $props
RETURN count(r) AS n`, params)
}

// validityJSONProperty preserves numeric types (including integral floats) in
// the audit JSON. Bolt-only values are tagged for a later recovery tool.
func validityJSONProperty(value any) any {
	switch v := value.(type) {
	case map[string]any:
		props := make(map[string]any, len(v))
		for key, item := range v {
			props[key] = validityJSONProperty(item)
		}
		return props
	case []any:
		items := make([]any, len(v))
		for i, item := range v {
			items[i] = validityJSONProperty(item)
		}
		return items
	case float64:
		number := strconv.FormatFloat(v, 'g', -1, 64)
		if math.IsInf(v, 0) || math.IsNaN(v) {
			return map[string]any{"type": "float", "value": number}
		}
		if !strings.ContainsAny(number, ".eE") {
			number += ".0"
		}
		return json.Number(number)
	case dbtype.Date, dbtype.LocalTime, dbtype.Time, dbtype.LocalDateTime:
		return map[string]any{"type": fmt.Sprintf("%T", v), "value": fmt.Sprint(v)}
	case time.Time, dbtype.Duration, dbtype.Point2D, dbtype.Point3D, []byte:
		return map[string]any{"type": fmt.Sprintf("%T", v), "value": v}
	default:
		return v
	}
}
