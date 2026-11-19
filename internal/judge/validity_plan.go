package judge

import (
	"bytes"
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"maps"
	"slices"
	"strings"

	"github.com/alessiosavi/go-knowledge-graph/internal/graph"
	"github.com/alessiosavi/go-knowledge-graph/internal/ingest"
	"github.com/alessiosavi/go-knowledge-graph/internal/validity"
)

type validityRewriteRow struct {
	rejudgeManifestRow
	SourceID string `json:"source_id"`
}

// PlanValidity freezes the live inputs, projection and rewritten judge requests.
func PlanValidity(ctx context.Context, st *graph.Store, model, presetsDir, preset, manifestPath, verdictPath, out string) (validity.PlanSummary, error) {
	var summary validity.PlanSummary
	checked, err := ingest.LoadCheckedPreset(presetsDir, preset, "")
	if err != nil {
		return summary, validityInputError(err)
	}
	lexicon := checked.Hygiene.Validity
	if lexicon == nil {
		return summary, validityInputError(fmt.Errorf("preset %q has no validity policy", preset))
	}
	rules, err := validity.NewRules(*lexicon)
	if err != nil {
		return summary, validityInputError(err)
	}
	definitions, err := rejudgeRelationDefinitions(preset, checked.Relations)
	if err != nil {
		return summary, validityInputError(err)
	}
	manifestBytes, err := readValidityFile(manifestPath)
	if err != nil {
		return summary, validityInputError(err)
	}
	manifest, err := decodeRejudgeManifest(bytes.NewReader(manifestBytes))
	if err != nil {
		return summary, validityInputError(err)
	}
	if len(manifest.rows) == 0 || len(manifest.header.Mismatches) != 0 {
		return summary, validityInputError(fmt.Errorf("bulk manifest must be non-empty and have no reconciliation mismatches"))
	}
	manifestRows, _, _, err := validateRejudgeManifest(manifest, model, presetsDir)
	if err != nil {
		return summary, validityInputError(err)
	}
	summary.ManifestDigest = fmt.Sprintf("%x", sha256.Sum256(manifestBytes))
	bulk, err := readSealedValidityRun(verdictPath, summary.ManifestDigest, manifestRows)
	if err != nil {
		return summary, validityInputError(err)
	}

	var inputs []validity.InputsRow
	var rewrites []validityRewriteRow
	var inventory []validityInventoryRow
	err = st.ReadValidityCatalog(ctx, preset, func(edges []graph.CatalogEdge, reader graph.CatalogReader) error {
		catalog, err := classifyValidityCatalog(edges, reader, definitions)
		if err != nil {
			return err
		}
		inventory = catalog.inventory
		if err := matchValidityInventory(catalog.planned, manifestRows, preset); err != nil {
			return validityInputError(err)
		}
		docs := make(map[string]struct{})
		for _, e := range catalog.planned {
			docs[e.fact.DocID] = struct{}{}
		}
		matches, err := reader.TitleMatches(slices.Sorted(maps.Keys(docs)))
		if err != nil {
			return err
		}
		inputs, rewrites, err = projectValidity(catalog, matches, rules, manifestRows, bulk.verdicts)
		return err
	})
	if err != nil {
		return summary, err
	}
	files, inventoryDigest, err := inventoryArtifacts(preset, inventory)
	if err != nil {
		return summary, err
	}
	inputs[0].InventoryDigest = inventoryDigest
	inputBytes, err := validityJSONLines(inputs)
	if err != nil {
		return summary, err
	}
	rewriteBytes, err := encodeValidityRewrites(rewrites)
	if err != nil {
		return summary, err
	}

	summary.RulesID = lexicon.Rules
	summary.LexiconDigest, err = validity.LexiconDigest(*lexicon)
	if err != nil {
		return summary, err
	}
	lexiconBytes, err := json.Marshal(lexicon)
	if err != nil {
		return summary, err
	}
	summary.VerdictSeal = bulk.seal
	summary.Counts = make(validityCounts)
	summary.BucketCounts = make(map[string]int)
	for _, row := range inventory {
		summary.BucketCounts[row.Bucket]++
	}
	for _, row := range inputs {
		validityCounts(summary.Counts).add(row.Outcome, row.Relation, row.Rule)
	}
	summary.DraftDigest = validityDraftDigest(summary, inputBytes, rewriteBytes)
	summaryBytes, err := validityJSONLines([]validity.PlanSummary{summary})
	if err != nil {
		return summary, err
	}
	files = append(files,
		validityFile{"lexicon.json", lexiconBytes},
		validityFile{"source-manifest.jsonl", manifestBytes},
		validityFile{"source-verdicts.jsonl", bulk.bytes},
		validityFile{"source-attempts.json", bulk.ledger},
		validityFile{"inputs.jsonl", inputBytes},
		validityFile{"rewrites.jsonl", rewriteBytes},
		validityFile{"plan.json", summaryBytes},
	)
	return summary, writeValidityFiles(ctx, out, files)
}

func validityDraftDigest(summary validity.PlanSummary, inputs, rewrites []byte) string {
	return validityDigest([]byte(summary.ManifestDigest), []byte(summary.VerdictSeal), inputs, rewrites,
		[]byte(summary.RulesID), []byte(summary.LexiconDigest))
}

func matchValidityInventory(planned map[string]rejudgeEvidence, frozen map[string]rejudgeManifestRow, preset string) error {
	if len(planned) != len(frozen) {
		return fmt.Errorf("inventory mismatch: %d planned ids, %d manifest ids", len(planned), len(frozen))
	}
	for _, id := range slices.Sorted(maps.Keys(frozen)) {
		row := frozen[id]
		e, ok := planned[id]
		if !ok {
			return fmt.Errorf("inventory mismatch: manifest id %s is outside the planned bucket", id)
		}
		prior := e.fact.Judge
		if prior == "" {
			prior = "unjudged"
		}
		if row.Judge != e.fact.Judge || row.Prior != prior || row.Preset != preset {
			return fmt.Errorf("inventory mismatch: id %s has a different preset or prior judge", id)
		}
		if row.RejudgeIdentity != e.fact.RejudgeIdentity || row.Request.DocID != e.fact.DocID ||
			!bytes.Equal(row.Request.RequestBytes, e.bytes) {
			return fmt.Errorf("manifest id %s differs from the live identity or judge request", id)
		}
	}
	return nil
}

func projectValidity(catalog validityCatalog, matches map[string][]graph.CatalogTitleMatch, rules *validity.Rules,
	manifest map[string]rejudgeManifestRow, verdicts map[string]rejudgeVerdict) ([]validity.InputsRow, []validityRewriteRow, error) {
	inputs := make([]validity.InputsRow, 0, len(catalog.planned))
	for _, id := range slices.Sorted(maps.Keys(catalog.planned)) {
		fact := catalog.planned[id].fact
		row := validity.InputsRow{
			ID: id, Domain: fact.Domain, Preset: fact.Preset, Bucket: catalogPlanned, PriorJudge: fact.Judge,
			Input:  validityParityInput(fact),
			Target: validityTitleTarget(matches[fact.DocID]),
		}
		projectValiditySource(&row, fact, rules, verdicts[id].Verdict, catalog.occupants)
		inputs = append(inputs, row)
	}

	rankValidityCandidates(inputs, catalog)
	var rewrites []validityRewriteRow
	for _, row := range inputs {
		if row.Outcome != validity.OutcomeRewriteCandidate {
			continue
		}
		rewrite, err := makeValidityRewrite(row, catalog.planned[row.ID], manifest[row.ID])
		if err != nil {
			return nil, nil, err
		}
		rewrites = append(rewrites, rewrite)
	}
	slices.SortFunc(rewrites, func(a, b validityRewriteRow) int { return cmp.Compare(a.ID, b.ID) })
	return inputs, rewrites, nil
}

func validityParityInput(fact graph.RejudgeFact) validity.Input {
	return validity.Input{Head: fact.HeadName, Relation: fact.Relation, Tail: fact.TailName, Title: fact.Title,
		Evidence: ingest.EvidenceSentence(fact.Text, fact.HeadStart, fact.HeadEnd)}
}

func rankValidityCandidates(inputs []validity.InputsRow, catalog validityCatalog) {
	groups := make(map[validity.DestinationKey][]int)
	for i, row := range inputs {
		if row.Outcome == validity.OutcomeRewriteCandidate {
			groups[*row.Destination] = append(groups[*row.Destination], i)
		}
	}
	for _, group := range groups {
		ranks := make([]validity.CandidateRankInput, 0, len(group))
		for _, i := range group {
			id := inputs[i].ID
			ranks = append(ranks, validity.CandidateRankInput{SourceID: id, Confidence: catalog.planned[id].fact.Confidence})
		}
		slices.SortFunc(ranks, func(a, b validity.CandidateRankInput) int {
			return cmp.Or(cmp.Compare(b.Confidence, a.Confidence), cmp.Compare(a.SourceID, b.SourceID))
		})
		winnerID := ranks[0].SourceID
		for _, i := range group {
			row := &inputs[i]
			row.CandidateGroup = ranks
			if row.ID != winnerID {
				row.Outcome, row.Reason = validity.OutcomeValidityDrop, "collision-merged:"+winnerID
			}
		}
	}
}

func validityTitleTarget(matches []graph.CatalogTitleMatch) validity.TitleTarget {
	var target validity.TitleTarget
	for _, match := range matches {
		target.TitleMatchKeys = append(target.TitleMatchKeys, match.Key)
	}
	if len(matches) == 1 {
		target.Key, target.Name, target.Type = matches[0].Key, matches[0].Name, matches[0].Type
	}
	return target
}

func projectValiditySource(row *validity.InputsRow, fact graph.RejudgeFact, rules *validity.Rules, verdict string,
	occupants map[validity.DestinationKey][]validity.Occupant) {
	switch verdict {
	case "refuted":
		row.Outcome = validity.OutcomeReject
		return
	case "unclear":
		row.Outcome = validity.OutcomeDemote
		return
	}
	row.Decision = rules.Decide(row.Input)
	switch row.Action {
	case validity.Drop:
		row.Outcome = validity.OutcomeValidityDrop
	case validity.Keep:
		row.Outcome = validity.OutcomeStamp
	case validity.Reattach:
		row.Outcome = validity.OutcomeValidityDrop
		switch row.Target.Key {
		case "":
			row.Reason = "no-target"
			if len(row.Target.TitleMatchKeys) > 1 {
				row.Reason = "ambiguous-target"
			}
		case fact.HeadKey:
			row.Outcome, row.Reason = validity.OutcomeStamp, "reattach-self"
		case fact.TailKey:
			row.Reason = "self-loop"
		default:
			key := validity.DestinationKey{HeadKey: row.Target.Key, Rel: fact.Relation, TailKey: fact.TailKey, ChunkID: fact.ChunkID}
			row.Destination = &key
			row.Occupants = occupants[key]
			slices.SortFunc(row.Occupants, func(a, b validity.Occupant) int {
				return cmp.Or(cmp.Compare(a.ID, b.ID), cmp.Compare(*a.EdgeID, *b.EdgeID))
			})
			if len(row.Occupants) != 0 {
				row.Reason = "collision-occupied"
			} else {
				row.Outcome = validity.OutcomeRewriteCandidate
			}
		}
	}
}

func makeValidityRewrite(row validity.InputsRow, source rejudgeEvidence, frozen rejudgeManifestRow) (validityRewriteRow, error) {
	key := *row.Destination
	surface := strings.TrimSpace(row.Target.Name)
	input := source.input
	input.HeadSurface = &surface
	input.Head.MentionType = row.Target.Type
	request, reason := BuildV2Request(input)
	if reason != "" {
		return validityRewriteRow{}, validityInputError(fmt.Errorf("rewrite for %s: %s", row.ID, reason))
	}
	identity := source.fact.RejudgeIdentity
	identity.HeadKey = key.HeadKey
	identity.ID = graph.FactAuditID(key.HeadKey, key.Rel, key.TailKey, key.ChunkID)
	identity.Judge = ""
	return validityRewriteRow{
		rejudgeManifestRow: rejudgeManifestRow{
			RejudgeIdentity: identity, Preset: frozen.Preset, Prior: "unjudged",
			Request: Request{Profile: frozen.Request.Profile, RequestBytes: request, DocID: frozen.Request.DocID},
		},
		SourceID: row.ID,
	}, nil
}

func encodeValidityRewrites(rows []validityRewriteRow) ([]byte, error) {
	var out bytes.Buffer
	enc := json.NewEncoder(&out)
	if err := enc.Encode(rejudgeManifestHeader{Version: rejudgeManifestVersion}); err != nil {
		return nil, err
	}
	for _, row := range rows {
		if err := enc.Encode(row); err != nil {
			return nil, err
		}
	}
	return out.Bytes(), nil
}

type validityCounts map[validity.Outcome]map[string]map[string]int

func (c validityCounts) add(outcome validity.Outcome, rel, rule string) {
	if c[outcome] == nil {
		c[outcome] = make(map[string]map[string]int)
	}
	if c[outcome][rel] == nil {
		c[outcome][rel] = make(map[string]int)
	}
	c[outcome][rel][rule]++
}
