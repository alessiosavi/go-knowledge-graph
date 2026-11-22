package main

import (
	"bytes"
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"hash"
	"io"
	"maps"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/alessiosavi/go-knowledge-graph/internal/graph"
	"github.com/alessiosavi/go-knowledge-graph/internal/productobs"
)

const productsMentionsParityUsage = "usage: gkg products mentions-parity -summary SUMMARY.json -pairs PAIRS.jsonl -summary-sha256 HEX -pairs-sha256 HEX [-out REPORT.json] [-timeout 30m]"

func runProductMentionsParity(args []string, stdout, stderr io.Writer) int {
	fs := productFlags("products mentions-parity", productsMentionsParityUsage, stderr)
	summaryPath := fs.String("summary", "", "frozen mentions-scan summary")
	pairsPath := fs.String("pairs", "", "frozen mentions-scan pairs")
	summaryHash := fs.String("summary-sha256", "", "expected SHA-256 digest of -summary")
	pairsHash := fs.String("pairs-sha256", "", "expected SHA-256 digest of -pairs")
	outPath := fs.String("out", "", "also write the report to a private file")
	timeout := fs.Duration("timeout", 30*time.Minute, "time limit for parity")
	if err := fs.Parse(args); err != nil {
		return productFlagExit(err)
	}
	if fs.NArg() != 0 || *summaryPath == "" || *pairsPath == "" || *summaryHash == "" || *pairsHash == "" || *timeout <= 0 {
		fs.Usage()
		return 2
	}
	summaryBytes, err := readParityInput(*summaryPath, "-summary-sha256", *summaryHash)
	if err != nil {
		return productCommandError(fs.Name(), stderr, fmt.Errorf("REFUSED: %w", err))
	}
	pairsBytes, err := readParityInput(*pairsPath, "-pairs-sha256", *pairsHash)
	if err != nil {
		return productCommandError(fs.Name(), stderr, fmt.Errorf("REFUSED: %w", err))
	}
	summary, oracle, err := decodeProductParityInputs(summaryBytes, pairsBytes)
	if err != nil {
		return productCommandError(fs.Name(), stderr, fmt.Errorf("REFUSED: %w", err))
	}

	var output *os.File
	if *outPath != "" {
		output, err = stageProductOutput("-out", *outPath, []string{*summaryPath, *pairsPath})
		if err != nil {
			return productCommandError(fs.Name(), stderr, err)
		}
		defer os.Remove(output.Name())
		defer output.Close()
	}
	return withProductStore(fs.Name(), stderr, func(parent context.Context, st *graph.Store) error {
		ctx, cancel := context.WithTimeout(parent, *timeout)
		defer cancel()
		report, err := measureProductMentionParity(ctx, st, summary, oracle)
		if err != nil {
			return fmt.Errorf("REFUSED: %w", err)
		}
		dst := stdout
		if output != nil {
			dst = io.MultiWriter(stdout, output)
		}
		encoder := json.NewEncoder(dst)
		encoder.SetIndent("", "  ")
		if err := encoder.Encode(report); err != nil {
			return fmt.Errorf("write parity report: %w", err)
		}
		if output != nil {
			if err := finishProductOutput(output, *outPath); err != nil {
				return err
			}
		}
		if report.Status != "PASS" {
			return errors.New("FAIL: " + strings.Join(report.Failures, "; "))
		}
		return nil
	})
}

func readParityInput(path, digestFlag, digest string) ([]byte, error) {
	expected, err := hex.DecodeString(digest)
	if err != nil || len(expected) != sha256.Size {
		return nil, fmt.Errorf("%s must be a 64-character SHA-256 digest", digestFlag)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	actual := sha256.Sum256(data)
	if !bytes.Equal(actual[:], expected) {
		return nil, fmt.Errorf("%s mismatch for %s", digestFlag, path)
	}
	return data, nil
}

type parityPinnedSummary struct {
	MatcherRevision string                   `json:"matcher_revision"`
	Release         string                   `json:"release"`
	ActivationSeq   *int64                   `json:"activation_seq"`
	Stores          []parityPinnedStore      `json:"stores"`
	Documents       []productMentionDocument `json:"documents"`
	Counts          productMentionCounts     `json:"counts"`
}

type parityPinnedStore struct {
	Store       string          `json:"store"`
	SeenSeq     *int64          `json:"seen_seq"`
	SnapshotSeq *int64          `json:"snapshot_seq"`
	Domain      *string         `json:"domain"`
	WipedSeq    *int64          `json:"wiped_seq"`
	ReleaseID   json.RawMessage `json:"release_id"`
	releaseID   *string
}

type parityPairID struct{ product, chunk string }

type parityOracle struct {
	pairs   map[parityPairID]productMentionPair
	byChunk map[string][]parityPairID
}

func decodeProductParityInputs(summaryData, pairsData []byte) (parityPinnedSummary, parityOracle, error) {
	var summary parityPinnedSummary
	if err := json.Unmarshal(summaryData, &summary); err != nil {
		return summary, parityOracle{}, fmt.Errorf("summary: %w", err)
	}
	if summary.MatcherRevision != productobs.CodeMatcherRevision {
		return summary, parityOracle{}, fmt.Errorf("matcher_revision: frozen %q, live %q", summary.MatcherRevision, productobs.CodeMatcherRevision)
	}
	if summary.Release == "" || summary.ActivationSeq == nil || len(summary.Documents) == 0 {
		return summary, parityOracle{}, errors.New("summary needs release, activation_seq and documents")
	}
	documents := make(map[string]bool, len(summary.Documents))
	for _, doc := range summary.Documents {
		if doc.ID == "" || doc.ChunksSHA256 == "" || doc.ChunkCount < 0 || documents[doc.ID] {
			return summary, parityOracle{}, fmt.Errorf("invalid or duplicate frozen document %q", doc.ID)
		}
		documents[doc.ID] = true
	}
	stores := make(map[string]bool, len(summary.Stores))
	for i := range summary.Stores {
		store := &summary.Stores[i]
		if store.Store == "" || stores[store.Store] || (store.SeenSeq == nil && store.SnapshotSeq == nil) {
			return summary, parityOracle{}, fmt.Errorf("invalid or duplicate frozen store %q (seen_seq or snapshot_seq required)", store.Store)
		}
		if store.ReleaseID != nil {
			if err := json.Unmarshal(store.ReleaseID, &store.releaseID); err != nil {
				return summary, parityOracle{}, fmt.Errorf("store %s release_id: %w", store.Store, err)
			}
		}
		stores[store.Store] = true
	}
	out := parityOracle{pairs: make(map[parityPairID]productMentionPair), byChunk: make(map[string][]parityPairID)}
	products := make(map[string]bool)
	decoder := json.NewDecoder(bytes.NewReader(pairsData))
	for {
		var pair productMentionPair
		err := decoder.Decode(&pair)
		if err == io.EOF {
			break
		}
		if err != nil {
			return summary, parityOracle{}, fmt.Errorf("pairs JSONL: %w", err)
		}
		id := parityPairID{pair.ID, pair.ChunkID}
		if pair.ID == "" || pair.ProductID == "" || pair.ChunkID == "" || len(pair.Matches) == 0 || !documents[pair.DocID] || !stores[pair.Store] {
			return summary, parityOracle{}, fmt.Errorf("invalid pair identity for product %q, chunk %q, document %q", id.product, id.chunk, pair.DocID)
		}
		if _, duplicate := out.pairs[id]; duplicate {
			return summary, parityOracle{}, fmt.Errorf("duplicate pair for product %q, chunk %q", id.product, id.chunk)
		}
		entries := make(map[graph.ProductMentionEntry]bool, len(pair.Matches))
		for _, match := range pair.Matches {
			entry := parityEntry(match)
			if entries[entry] || match.Span[0] < 0 || match.Span[1] <= match.Span[0] ||
				match.Span[1]-match.Span[0] != len(match.Text) || match.Tokens < 1 || match.Tokens > 3 ||
				productobs.CodeKey(match.Text) != match.Key || !productobs.EligibleCodeKey(match.Key) {
				return summary, parityOracle{}, fmt.Errorf("invalid or duplicate match entry for product %q, chunk %q", id.product, id.chunk)
			}
			entries[entry] = true
		}
		out.pairs[id] = pair
		out.byChunk[id.chunk] = append(out.byChunk[id.chunk], id)
		products[id.product] = true
	}
	if len(out.pairs) != summary.Counts.Pairs || len(out.byChunk) != summary.Counts.ChunksWithPairs || len(products) != summary.Counts.ProductsWithPairs {
		return summary, parityOracle{}, fmt.Errorf("counts do not match oracle: pairs=%d, chunks_with_pairs=%d, products_with_pairs=%d",
			len(out.pairs), len(out.byChunk), len(products))
	}
	return summary, out, nil
}

type parityReadStore interface {
	ReadProductMentions(context.Context, graph.ProductMentionSelection, func(graph.ProductMentionInput) error, func(graph.Chunk) error) error
	GetProductWithMentions(context.Context, graph.ProductLookup) (graph.ProductDetailWithMentions, graph.ProductMentionRead, error)
}

type paritySnapshot struct {
	release       string
	activationSeq int64
	stores        []graph.ProductMentionStore
	documents     map[string]*parityDocumentState
	index         *graph.ProductMentionIndex
	indexHash     string
	pairErr       error
}

type parityDocumentState struct {
	doc    graph.Document
	chunks int
	digest hash.Hash
}

var errParityStoredChunkHash = errors.New("stored chunk hash differs from text")

func captureParitySnapshot(ctx context.Context, st parityReadStore, selection graph.ProductMentionSelection, oracle parityOracle) (paritySnapshot, error) {
	var out paritySnapshot
	var validated int
	begin := func(input graph.ProductMentionInput) error {
		out.release, out.activationSeq, out.stores = input.Release, input.ActivationSeq, input.Stores
		out.documents = make(map[string]*parityDocumentState, len(input.Documents))
		for _, doc := range input.Documents {
			out.documents[doc.ID] = &parityDocumentState{doc: doc, digest: sha256.New()}
		}
		out.indexHash = parityIndexDigest(input.Identifiers)
		if oracle.pairs != nil {
			out.index = graph.BuildProductMentionIndex(input.Identifiers)
		}
		return ctx.Err()
	}
	visit := func(chunk graph.Chunk) error {
		// Ingest stores the hex encoding of SHA-256 over the chunk's UTF-8 text.
		if got := fmt.Sprintf("%x", sha256.Sum256([]byte(chunk.Text))); got != chunk.Hash {
			return fmt.Errorf("%w: document %s chunk %s", errParityStoredChunkHash, chunk.DocID, chunk.ID)
		}
		doc := out.documents[chunk.DocID]
		doc.chunks++
		fmt.Fprintf(doc.digest, "%s\t%s\n", chunk.ID, chunk.Hash)
		for _, id := range oracle.byChunk[chunk.ID] {
			pair := oracle.pairs[id]
			if out.pairErr == nil {
				out.pairErr = validateParityPair(out.index, chunk, pair)
			}
			validated++
		}
		return ctx.Err()
	}
	if err := st.ReadProductMentions(ctx, selection, begin, visit); err != nil {
		return out, err
	}
	if validated != len(oracle.pairs) && out.pairErr == nil {
		out.pairErr = errors.New("pairs refer to chunk IDs absent from the frozen documents")
	}
	return out, nil
}

func validateParityPair(index *graph.ProductMentionIndex, chunk graph.Chunk, pair productMentionPair) error {
	if chunk.DocID != pair.DocID || chunk.Seq != pair.ChunkSeq {
		return fmt.Errorf("pair identity differs from frozen chunk %s", chunk.ID)
	}
	for _, match := range pair.Matches {
		var found bool
		if match.Span[1] <= len(chunk.Text) && chunk.Text[match.Span[0]:match.Span[1]] == match.Text {
			for _, live := range index.Match(match.Text) {
				if live.Span == [2]int{0, len(match.Text)} && live.Key == match.Key && live.Tokens == match.Tokens &&
					live.Product.ID == pair.ID && live.Product.ProductID == pair.ProductID && live.Product.Store == pair.Store &&
					slices.Contains(live.Evidence, graph.ProductMentionEvidence{Feature: match.Feature, ValueFold: match.ValueFold}) {
					found = true
					break
				}
			}
		}
		if !found {
			return fmt.Errorf("pair product/key/span identity differs for product %s, chunk %s, key %s", pair.ID, chunk.ID, match.Key)
		}
	}
	return nil
}

func parityIndexDigest(observations []graph.ProductIdentifierObservation) string {
	type triple [3]string
	var values []triple
	for _, obs := range observations {
		if obs.ValueFold != nil {
			values = append(values, triple{obs.Product.ID, obs.Feature, *obs.ValueFold})
		}
		for _, item := range obs.ItemsFold {
			values = append(values, triple{obs.Product.ID, obs.Feature, item})
		}
	}
	slices.SortFunc(values, func(a, b triple) int { return slices.Compare(a[:], b[:]) })
	digest := sha256.New()
	encoder := json.NewEncoder(digest)
	for _, value := range slices.Compact(values) {
		_ = encoder.Encode(value) // SHA-256 writes and string encoding cannot fail.
	}
	return hex.EncodeToString(digest.Sum(nil))
}

func checkParityPinned(summary parityPinnedSummary, live paritySnapshot) error {
	if summary.Release != live.release {
		return fmt.Errorf("release: frozen %q, live %q", summary.Release, live.release)
	}
	if *summary.ActivationSeq != live.activationSeq {
		return fmt.Errorf("activation_seq: frozen %d, live %d", *summary.ActivationSeq, live.activationSeq)
	}
	if len(summary.Stores) != len(live.stores) {
		return fmt.Errorf("bound stores: frozen %d, live %d", len(summary.Stores), len(live.stores))
	}
	stores := make(map[string]graph.ProductMentionStore, len(live.stores))
	for _, store := range live.stores {
		stores[store.Store] = store
	}
	for _, pinned := range summary.Stores {
		store, ok := stores[pinned.Store]
		if !ok {
			return fmt.Errorf("bound store %s is absent", pinned.Store)
		}
		if err := checkPinnedStore(pinned, store); err != nil {
			return err
		}
	}
	for _, pinned := range summary.Documents {
		state := live.documents[pinned.ID]
		if state == nil {
			return fmt.Errorf("frozen document %s is absent", pinned.ID)
		}
		if state.chunks != pinned.ChunkCount {
			return fmt.Errorf("document %s chunk_count: frozen %d, live %d", pinned.ID, pinned.ChunkCount, state.chunks)
		}
		if got := hex.EncodeToString(state.digest.Sum(nil)); got != pinned.ChunksSHA256 {
			return fmt.Errorf("document %s chunks_sha256: frozen %s, live %s", pinned.ID, pinned.ChunksSHA256, got)
		}
	}
	stats := live.index.Stats
	if stats.IdentifierValues != summary.Counts.IdentifierValues || stats.EligibleKeys != summary.Counts.EligibleKeys ||
		stats.IneligibleValues != summary.Counts.IneligibleValues || len(stats.Ambiguous) != summary.Counts.AmbiguousKeys {
		return errors.New("summary identifier_values, eligible_keys, ineligible_values or ambiguous_keys differs from live index")
	}
	return nil
}

func checkPinnedStore(pinned parityPinnedStore, live graph.ProductMentionStore) error {
	for _, field := range []struct {
		name string
		want *int64
		got  int64
	}{
		{"seen_seq", pinned.SeenSeq, live.SeenSeq},
		{"snapshot_seq", pinned.SnapshotSeq, live.SnapshotSeq},
		{"wiped_seq", pinned.WipedSeq, live.WipedSeq},
	} {
		if field.want != nil && *field.want != field.got {
			return fmt.Errorf("store %s %s: frozen %d, live %d", live.Store, field.name, *field.want, field.got)
		}
	}
	if pinned.Domain != nil && *pinned.Domain != live.Domain {
		return fmt.Errorf("store %s domain differs", live.Store)
	}
	if pinned.ReleaseID != nil && !equalParityPointer(pinned.releaseID, live.ReleaseID) {
		return fmt.Errorf("store %s release_id differs", live.Store)
	}
	return nil
}

func equalParityPointer[T comparable](a, b *T) bool {
	return a == b || (a != nil && b != nil && *a == *b)
}

func checkParityDrift(before, after paritySnapshot) error {
	if before.release != after.release || before.activationSeq != after.activationSeq {
		return errors.New("release or activation_seq changed")
	}
	if len(before.stores) != len(after.stores) {
		return errors.New("bound stores changed")
	}
	for n, a := range before.stores {
		b := after.stores[n]
		if a.Store != b.Store || a.Domain != b.Domain {
			return errors.New("bound store identity or domain changed")
		}
		if a.WipedSeq != b.WipedSeq || a.SeenSeq != b.SeenSeq || a.SnapshotSeq != b.SnapshotSeq || !equalParityPointer(a.ReleaseID, b.ReleaseID) {
			return fmt.Errorf("store %s seen_seq, snapshot_seq, wiped_seq or release_id changed", a.Store)
		}
		if !equalParityPointer(a.MaterializedActivationSeq, b.MaterializedActivationSeq) {
			return fmt.Errorf("store %s materialized_activation_seq changed", a.Store)
		}
	}
	for id, a := range before.documents {
		b, ok := after.documents[id]
		if !ok || a.doc.Status != b.doc.Status || a.chunks != b.chunks || !bytes.Equal(a.digest.Sum(nil), b.digest.Sum(nil)) {
			return fmt.Errorf("document %s status, chunk IDs, chunk_count or chunks_sha256 changed", id)
		}
	}
	if before.indexHash != after.indexHash {
		return errors.New("identifier index digest changed")
	}
	return nil
}

type parityRecall struct {
	Expected     int     `json:"expected"`
	Found        int     `json:"found"`
	Recall       float64 `json:"recall"`
	LostPartial  int     `json:"lost_partial"`
	LostComplete int     `json:"lost_complete"`
	LostOnError  int     `json:"lost_on_error"`
	Extras       int     `json:"extras"`
}

type parityLatency struct {
	P50 float64 `json:"p50_ms"`
	P95 float64 `json:"p95_ms"`
	P99 float64 `json:"p99_ms"`
	Max float64 `json:"max_ms"`
}

type parityTiming struct {
	ColdMS float64       `json:"cold_ms"`
	Warm   parityLatency `json:"warm"`
	All    parityLatency `json:"all"`
}

type productParityReport struct {
	Status             string                   `json:"status"`
	Failures           []string                 `json:"failures"`
	Products           int                      `json:"products"`
	Reads              int                      `json:"reads"`
	NegativeControls   int                      `json:"negative_controls"`
	NegativeWithExtras int                      `json:"negative_controls_with_extras"`
	Pairs              parityRecall             `json:"pairs"`
	Entries            parityRecall             `json:"entries"`
	Strata             map[string]*parityRecall `json:"strata"`
	Errors             map[string]string        `json:"read_errors"`
	PartialReads       int                      `json:"partial_reads"`
	PartialRate        float64                  `json:"partial_rate"`
	ErrorRate          float64                  `json:"error_rate"`
	RawSaturated       int                      `json:"raw_saturated"`
	EligibleSaturated  int                      `json:"eligible_saturated"`
	DroppedKeys        int                      `json:"dropped_keys"`
	EndToEnd           parityTiming             `json:"end_to_end"`
	IndexLoad          parityTiming             `json:"index_load"`
	SearchMatch        parityTiming             `json:"search_match"`
}

func measureProductMentionParity(ctx context.Context, st parityReadStore, summary parityPinnedSummary, oracle parityOracle) (productParityReport, error) {
	report := productParityReport{Status: "FAIL", Failures: []string{}, Errors: make(map[string]string), Strata: map[string]*parityRecall{
		"separator_or_multi_token": {}, "product_reference": {}, "catalogue_code": {}, "multi_key_product": {},
	}}
	selection := graph.ProductMentionSelection{DocumentIDs: make([]string, len(summary.Documents))}
	for i, doc := range summary.Documents {
		selection.DocumentIDs[i] = doc.ID
	}
	before, err := captureParitySnapshot(ctx, st, selection, oracle)
	if ctx.Err() != nil {
		report.Failures = append(report.Failures, ctx.Err().Error())
		return report, nil
	}
	if errors.Is(err, errParityStoredChunkHash) {
		report.Failures = append(report.Failures, err.Error())
		return report, nil
	}
	switch {
	case err == nil:
		err = checkParityPinned(summary, before)
	case errors.Is(err, graph.ErrNoActiveProductRelease), errors.Is(err, graph.ErrNotFound), errors.Is(err, graph.ErrNotDone):
		// The release or a pinned document changed before the snapshot could be captured.
	default:
		return report, err
	}
	if err != nil {
		report.Failures = append(report.Failures, "pre-read frozen metadata: "+err.Error())
		return report, nil
	}
	// Check pair identities after metadata, since drift can invalidate those too.
	if before.pairErr != nil {
		return report, before.pairErr
	}

	keyCounts := before.index.KeyedProducts()
	report.Products = len(keyCounts)
	expected := make(map[string]map[string]productMentionPair)
	for id, pair := range oracle.pairs {
		if expected[id.product] == nil {
			expected[id.product] = make(map[string]productMentionPair)
		}
		expected[id.product][id.chunk] = pair
	}
	report.NegativeControls = report.Products - len(expected)
	var endToEnd, indexLoad, searchMatch []time.Duration
	for _, id := range slices.Sorted(maps.Keys(keyCounts)) {
		if ctx.Err() != nil {
			break
		}
		start := time.Now()
		out, read, err := st.GetProductWithMentions(ctx, graph.ProductLookup{ID: id})
		endToEnd = append(endToEnd, time.Since(start))
		indexLoad = append(indexLoad, read.IndexLoad)
		searchMatch = append(searchMatch, read.SearchMatch)
		report.Reads++
		if err != nil {
			report.Errors[id] = "product_lookup_failed"
		} else if out.MentionsError != "" {
			report.Errors[id] = out.MentionsError
		}
		var partial bool
		var mentions []graph.ProductChunkMention
		if out.ProductMentionFields != nil {
			partial, mentions = out.MentionsPartial, out.Mentions
		}
		if partial {
			report.PartialReads++
		}
		if read.RawSaturated {
			report.RawSaturated++
		}
		if read.EligibleSaturated {
			report.EligibleSaturated++
		}
		report.DroppedKeys += read.DroppedKeys
		report.compareRead(expected[id], keyCounts[id], before.documents, mentions, partial, report.Errors[id] != "")
		delete(expected, id)
	}
	for id, pairs := range expected {
		report.compareRead(pairs, keyCounts[id], before.documents, nil, false, true)
	}

	report.EndToEnd, report.IndexLoad, report.SearchMatch = parityTimes(endToEnd), parityTimes(indexLoad), parityTimes(searchMatch)
	if report.Reads != 0 {
		report.PartialRate = float64(report.PartialReads) / float64(report.Reads)
		report.ErrorRate = float64(len(report.Errors)) / float64(report.Reads)
	}
	report.checkGates()
	if ctx.Err() == nil {
		after, drift := captureParitySnapshot(ctx, st, selection, parityOracle{})
		if drift == nil {
			drift = checkParityDrift(before, after)
		}
		if drift != nil {
			report.Failures = append(report.Failures, "post-read drift check: "+drift.Error())
		}
	}
	if err := ctx.Err(); err != nil {
		report.Failures = append(report.Failures, err.Error())
	}
	if len(report.Failures) == 0 {
		report.Status = "PASS"
	}
	return report, nil
}

func parityEntry(match productMentionMatch) graph.ProductMentionEntry {
	return graph.ProductMentionEntry{Key: match.Key, Span: match.Span, Feature: match.Feature, ValueFold: match.ValueFold}
}

func (r *productParityReport) compareRead(expected map[string]productMentionPair, keys int, frozen map[string]*parityDocumentState,
	mentions []graph.ProductChunkMention, partial, failed bool) {
	actual := make(map[string]map[graph.ProductMentionEntry]bool)
	for _, mention := range mentions {
		if frozen[mention.DocID] == nil {
			continue
		}
		entries := make(map[graph.ProductMentionEntry]bool, len(mention.Entries))
		for _, entry := range mention.Entries {
			entries[entry] = true
		}
		actual[mention.ChunkID] = entries
	}
	if len(expected) == 0 && len(actual) != 0 {
		r.NegativeWithExtras++
	}
	for chunk, pair := range expected {
		got, found := actual[chunk]
		r.Pairs.count(found, partial, failed)
		for _, match := range pair.Matches {
			entry := parityEntry(match)
			entryFound := got[entry]
			delete(got, entry)
			r.Entries.count(entryFound, partial, failed)
			if stratum := r.Strata[entry.Feature]; stratum != nil {
				stratum.count(entryFound, partial, failed)
			}
			if m := (productobs.CodeMatch{Tokens: match.Tokens, Text: match.Text}); m.SeparatorRun() {
				r.Strata["separator_or_multi_token"].count(entryFound, partial, failed)
			}
			if keys > 1 {
				r.Strata["multi_key_product"].count(entryFound, partial, failed)
			}
		}
	}
	for chunk, entries := range actual {
		if _, exists := expected[chunk]; !exists {
			r.Pairs.Extras++
		}
		r.Entries.Extras += len(entries)
	}
}

func (r *parityRecall) count(found, partial, failed bool) {
	r.Expected++
	switch {
	case found:
		r.Found++
	case failed:
		r.LostOnError++
	case partial:
		r.LostPartial++
	default:
		r.LostComplete++
	}
}

func (r *productParityReport) checkGates() {
	measure := func(name string, count *parityRecall, minCount int) {
		count.Recall = 1
		if count.Expected != 0 {
			count.Recall = float64(count.Found) / float64(count.Expected)
		}
		if count.Expected >= minCount && count.Recall < 0.99 {
			r.Failures = append(r.Failures, fmt.Sprintf("%s recall %.4f below 0.99", name, count.Recall))
		}
	}
	measure("pair", &r.Pairs, 1)
	measure("entry", &r.Entries, 1)
	for _, name := range slices.Sorted(maps.Keys(r.Strata)) {
		measure(name, r.Strata[name], 20)
	}
	if len(r.Errors) != 0 || r.Pairs.Extras != 0 || r.Entries.Extras != 0 {
		r.Failures = append(r.Failures, fmt.Sprintf("read errors=%d, extra pairs=%d, extra entries=%d", len(r.Errors), r.Pairs.Extras, r.Entries.Extras))
	}
	if r.PartialRate > 0.10 {
		r.Failures = append(r.Failures, fmt.Sprintf("partial rate %.4f above 0.10", r.PartialRate))
	}
	if r.EndToEnd.All.P95 > 500 {
		r.Failures = append(r.Failures, fmt.Sprintf("end-to-end p95 %.3f ms above 500 ms", r.EndToEnd.All.P95))
	}
}

func parityTimes(times []time.Duration) parityTiming {
	if len(times) == 0 {
		return parityTiming{}
	}
	millis := func(d time.Duration) float64 { return float64(d) / float64(time.Millisecond) }
	stats := func(values []time.Duration) parityLatency {
		if len(values) == 0 {
			return parityLatency{}
		}
		sorted := slices.Clone(values)
		slices.SortFunc(sorted, cmp.Compare[time.Duration])
		n := len(sorted)
		at := func(p int) float64 { return millis(sorted[(p*n+99)/100-1]) }
		return parityLatency{P50: at(50), P95: at(95), P99: at(99), Max: millis(sorted[n-1])}
	}
	return parityTiming{ColdMS: millis(times[0]), Warm: stats(times[1:]), All: stats(times)}
}
