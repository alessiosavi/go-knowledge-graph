package main

import (
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"hash"
	"io"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"unicode/utf8"

	"github.com/alessiosavi/go-knowledge-graph/internal/graph"
	"github.com/alessiosavi/go-knowledge-graph/internal/productobs"
)

const productsMentionsScanUsage = "usage: gkg products mentions-scan (-doc ID [-doc ID …] | -domain D [-source-prefix P]) -out FILE.jsonl [-summary FILE.json]"

func runProductMentionsScan(args []string, stderr io.Writer) int {
	fs := productFlags("products mentions-scan", productsMentionsScanUsage, stderr)
	var selection graph.ProductMentionSelection
	fs.Func("doc", "done document ID (repeatable)", func(id string) error {
		if strings.TrimSpace(id) == "" {
			return errors.New("expected a non-empty document ID")
		}
		selection.DocumentIDs = append(selection.DocumentIDs, id)
		return nil
	})
	fs.StringVar(&selection.Domain, "domain", "", "domain of done documents")
	fs.StringVar(&selection.SourcePrefix, "source-prefix", "", "literal source prefix within the domain")
	outPath := fs.String("out", "", "write private pair JSONL to this file")
	summaryPath := fs.String("summary", "", "write a private JSON summary to this file")
	if err := fs.Parse(args); err != nil {
		return productFlagExit(err)
	}
	var domainSet, prefixSet bool
	fs.Visit(func(f *flag.Flag) {
		domainSet = domainSet || f.Name == "domain"
		prefixSet = prefixSet || f.Name == "source-prefix"
	})
	if fs.NArg() != 0 || *outPath == "" || (len(selection.DocumentIDs) > 0) == domainSet ||
		(domainSet && strings.TrimSpace(selection.Domain) == "") || (prefixSet && !domainSet) {
		fs.Usage()
		return 2
	}

	return withProductStore(fs.Name(), stderr, func(ctx context.Context, st *graph.Store) error {
		return writeProductMentionScan(ctx, st, selection, *outPath, *summaryPath)
	})
}

func writeProductMentionScan(ctx context.Context, st *graph.Store, selection graph.ProductMentionSelection, outPath, summaryPath string) error {
	out, err := stageProductOutput("-out", outPath, nil)
	if err != nil {
		return err
	}
	defer os.Remove(out.Name())
	defer out.Close()

	var summaryFile *os.File
	if summaryPath != "" {
		if filepath.Base(outPath) == filepath.Base(summaryPath) {
			outDir, err := os.Stat(filepath.Dir(outPath))
			if err != nil {
				return err
			}
			summaryDir, err := os.Stat(filepath.Dir(summaryPath))
			if err != nil {
				return err
			}
			if os.SameFile(outDir, summaryDir) {
				return errors.New("-out and -summary must name different files")
			}
		}
		summaryFile, err = stageProductOutput("-summary", summaryPath, nil)
		if err != nil {
			return err
		}
		defer os.Remove(summaryFile.Name())
		defer summaryFile.Close()
	}

	summary, err := scanProductMentions(ctx, st, selection, out)
	if err != nil {
		return err
	}
	if summaryFile != nil {
		if err := json.NewEncoder(summaryFile).Encode(summary); err != nil {
			return fmt.Errorf("write summary: %w", err)
		}
	}
	if err := finishProductOutput(out, outPath); err != nil {
		return fmt.Errorf("-out: %w", err)
	}
	if summaryFile != nil {
		if err := finishProductOutput(summaryFile, summaryPath); err != nil {
			return fmt.Errorf("-summary: %w", err)
		}
	}
	return nil
}

type productMentionMatch struct {
	Key       string `json:"key"`
	Feature   string `json:"feature"`
	ValueFold string `json:"value_fold"`
	Span      [2]int `json:"span"`
	Text      string `json:"text"`
	Tokens    int    `json:"tokens"`
}

type productMentionPair struct {
	ID            string                `json:"id"`
	ProductID     string                `json:"product_id"`
	Store         string                `json:"store"`
	ProductDomain string                `json:"product_domain"`
	Title         string                `json:"title"`
	DocID         string                `json:"doc_id"`
	DocSource     string                `json:"doc_source"`
	DocTitle      string                `json:"doc_title"`
	ChunkID       string                `json:"chunk_id"`
	ChunkSeq      int                   `json:"chunk_seq"`
	Matches       []productMentionMatch `json:"matches"`
	SeparatorRun  bool                  `json:"separator_run"`
	Features      []string              `json:"features"`
	Excerpt       string                `json:"excerpt"`
}

type productMentionDocument struct {
	ID           string `json:"id"`
	Source       string `json:"source"`
	Hash         string `json:"hash"`
	ChunkCount   int    `json:"chunk_count"`
	ChunksSHA256 string `json:"chunks_sha256"`
}

type productMentionCounts struct {
	IdentifierValues  int `json:"identifier_values"`
	EligibleKeys      int `json:"eligible_keys"`
	IneligibleValues  int `json:"ineligible_values"`
	AmbiguousKeys     int `json:"ambiguous_keys"`
	Pairs             int `json:"pairs"`
	ChunksWithPairs   int `json:"chunks_with_pairs"`
	ProductsWithPairs int `json:"products_with_pairs"`
}

type productMentionIdentity struct {
	ID    string `json:"id"`
	Store string `json:"store"`
}

type productMentionAmbiguity struct {
	Key      string                   `json:"key"`
	Products []productMentionIdentity `json:"products"`
}

type productMentionDivergenceExample struct {
	productMentionIdentity
	Key       string `json:"key"`
	Feature   string `json:"feature"`
	ValueFold string `json:"value_fold"`
}

type productMentionDivergence struct {
	Count    int                               `json:"count"`
	Examples []productMentionDivergenceExample `json:"examples"`
}

type productMentionSummary struct {
	MatcherRevision string                      `json:"matcher_revision"`
	Release         string                      `json:"release"`
	ActivationSeq   int64                       `json:"activation_seq"`
	Stores          []graph.ProductMentionStore `json:"stores"`
	Documents       []productMentionDocument    `json:"documents"`
	Counts          productMentionCounts        `json:"counts"`
	Ambiguous       []productMentionAmbiguity   `json:"ambiguous"`
	Divergence      productMentionDivergence    `json:"divergence"`
}

type productMentionEvidence struct {
	feature   string
	valueFold string
}

type productMentionKey struct {
	product  graph.Product
	evidence []productMentionEvidence
}

func indexProductMentionKeys(observations []graph.ProductIdentifierObservation, summary *productMentionSummary) map[string]*productMentionKey {
	byKey := make(map[string]map[string]*productMentionKey)
	summary.Ambiguous = []productMentionAmbiguity{}
	summary.Divergence.Examples = []productMentionDivergenceExample{}
	addValue := func(observation graph.ProductIdentifierObservation, value string) {
		summary.Counts.IdentifierValues++
		key := productobs.CodeKey(value)
		product := observation.Product
		if key != value {
			summary.Divergence.Count++
			if len(summary.Divergence.Examples) < 20 {
				summary.Divergence.Examples = append(summary.Divergence.Examples, productMentionDivergenceExample{
					productMentionIdentity: productMentionIdentity{ID: product.ID, Store: product.Store},
					Key:                    key, Feature: observation.Feature, ValueFold: value,
				})
			}
		}
		if !productobs.EligibleCodeKey(key) {
			summary.Counts.IneligibleValues++
			return
		}
		products := byKey[key]
		if products == nil {
			products = make(map[string]*productMentionKey)
			byKey[key] = products
		}
		entry := products[product.ID]
		if entry == nil {
			entry = &productMentionKey{product: product}
			products[product.ID] = entry
		}
		entry.evidence = append(entry.evidence, productMentionEvidence{feature: observation.Feature, valueFold: value})
	}
	for _, observation := range observations {
		if observation.ValueFold != nil {
			addValue(observation, *observation.ValueFold)
		}
		for _, item := range observation.ItemsFold {
			addValue(observation, item)
		}
	}

	summary.Counts.EligibleKeys = len(byKey)
	keys := make(map[string]*productMentionKey, len(byKey))
	for key, products := range byKey {
		if len(products) == 1 {
			for _, entry := range products {
				slices.SortFunc(entry.evidence, func(a, b productMentionEvidence) int {
					return cmp.Or(cmp.Compare(a.feature, b.feature), cmp.Compare(a.valueFold, b.valueFold))
				})
				entry.evidence = slices.Compact(entry.evidence)
				keys[key] = entry
			}
			continue
		}
		ambiguous := productMentionAmbiguity{Key: key, Products: make([]productMentionIdentity, 0, len(products))}
		for _, id := range slices.Sorted(maps.Keys(products)) {
			ambiguous.Products = append(ambiguous.Products, productMentionIdentity{ID: id, Store: products[id].product.Store})
		}
		summary.Ambiguous = append(summary.Ambiguous, ambiguous)
	}
	slices.SortFunc(summary.Ambiguous, func(a, b productMentionAmbiguity) int { return cmp.Compare(a.Key, b.Key) })
	summary.Counts.AmbiguousKeys = len(summary.Ambiguous)
	return keys
}

type productMentionDocState struct {
	doc     graph.Document
	summary *productMentionDocument
	digest  hash.Hash
}

func scanProductMentions(ctx context.Context, st *graph.Store, selection graph.ProductMentionSelection, out io.Writer) (productMentionSummary, error) {
	summary := productMentionSummary{MatcherRevision: productobs.CodeMatcherRevision}
	docs := make(map[string]productMentionDocState)
	products := make(map[string]struct{})
	var keys map[string]*productMentionKey
	var pending []*productMentionPair
	var source string
	var seq int
	encoder := json.NewEncoder(out)
	flush := func() error {
		slices.SortFunc(pending, func(a, b *productMentionPair) int {
			return cmp.Or(cmp.Compare(a.ProductID, b.ProductID), cmp.Compare(a.DocID, b.DocID),
				cmp.Compare(a.ChunkID, b.ChunkID), cmp.Compare(a.ID, b.ID))
		})
		for _, pair := range pending {
			if err := encoder.Encode(pair); err != nil {
				return fmt.Errorf("write pair: %w", err)
			}
		}
		clear(pending)
		pending = pending[:0]
		return nil
	}
	begin := func(input graph.ProductMentionInput) error {
		summary.Release, summary.ActivationSeq, summary.Stores = input.Release, input.ActivationSeq, input.Stores
		keys = indexProductMentionKeys(input.Identifiers, &summary)
		summary.Documents = make([]productMentionDocument, len(input.Documents))
		for i, doc := range input.Documents {
			summary.Documents[i] = productMentionDocument{ID: doc.ID, Source: doc.Source, Hash: doc.Hash}
			docs[doc.ID] = productMentionDocState{doc: doc, summary: &summary.Documents[i], digest: sha256.New()}
		}
		return nil
	}
	visit := func(chunk graph.Chunk) error {
		state := docs[chunk.DocID]
		state.summary.ChunkCount++
		fmt.Fprintf(state.digest, "%s\t%s\n", chunk.ID, chunk.Hash)
		if state.doc.Source != source || chunk.Seq != seq {
			if err := flush(); err != nil {
				return err
			}
			source, seq = state.doc.Source, chunk.Seq
		}
		pairs := matchProductMentionPairs(state.doc, chunk, keys)
		if len(pairs) > 0 {
			summary.Counts.ChunksWithPairs++
			summary.Counts.Pairs += len(pairs)
			for id, pair := range pairs {
				products[id] = struct{}{}
				pending = append(pending, pair)
			}
		}
		return nil
	}
	if err := st.ReadProductMentions(ctx, selection, begin, visit); err != nil {
		return productMentionSummary{}, err
	}
	if err := flush(); err != nil {
		return productMentionSummary{}, err
	}
	for _, state := range docs {
		state.summary.ChunksSHA256 = hex.EncodeToString(state.digest.Sum(nil))
	}
	summary.Counts.ProductsWithPairs = len(products)
	return summary, nil
}

func matchProductMentionPairs(doc graph.Document, chunk graph.Chunk, keys map[string]*productMentionKey) map[string]*productMentionPair {
	matches := productobs.MatchCodes(chunk.Text, keys)
	if len(matches) == 0 {
		return nil
	}
	pairs := make(map[string]*productMentionPair)
	for _, match := range matches {
		entry := keys[match.Key]
		product := entry.product
		pair := pairs[product.ID]
		if pair == nil {
			pair = &productMentionPair{
				ID: product.ID, ProductID: product.ProductID, Store: product.Store,
				ProductDomain: product.Domain, Title: product.Title,
				DocID: doc.ID, DocSource: doc.Source, DocTitle: doc.Title,
				ChunkID: chunk.ID, ChunkSeq: chunk.Seq, Excerpt: productMentionExcerpt(chunk.Text, match.Span),
			}
			pairs[product.ID] = pair
		}
		pair.SeparatorRun = pair.SeparatorRun || match.SeparatorRun()
		for _, evidence := range entry.evidence {
			pair.Matches = append(pair.Matches, productMentionMatch{
				Key: match.Key, Feature: evidence.feature, ValueFold: evidence.valueFold,
				Span: match.Span, Text: match.Text, Tokens: match.Tokens,
			})
			pair.Features = append(pair.Features, evidence.feature)
		}
	}
	for _, pair := range pairs {
		slices.Sort(pair.Features)
		pair.Features = slices.Compact(pair.Features)
	}
	return pairs
}

func productMentionExcerpt(text string, span [2]int) string {
	const contextBytes = 200
	start, end := max(0, span[0]-contextBytes), min(len(text), span[1]+contextBytes)
	for start < span[0] && !utf8.RuneStart(text[start]) {
		start++
	}
	for end < len(text) && end > span[1] && !utf8.RuneStart(text[end]) {
		end--
	}
	return text[start:end]
}
