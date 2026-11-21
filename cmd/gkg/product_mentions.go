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
		(domainSet && !graph.ValidDomain(selection.Domain)) || (prefixSet && !domainSet) {
		fs.Usage()
		return 2
	}

	return withProductStore(fs.Name(), stderr, func(ctx context.Context, st *graph.Store) error {
		return writeProductMentionScan(ctx, st, selection, *outPath, *summaryPath, stderr)
	})
}

func writeProductMentionScan(ctx context.Context, st *graph.Store, selection graph.ProductMentionSelection,
	outPath, summaryPath string, stderr io.Writer) error {
	if summaryPath != "" {
		if err := checkProductMentionOutputs(outPath, summaryPath); err != nil {
			return err
		}
	}
	out, err := stageProductOutput("-out", outPath, nil)
	if err != nil {
		return err
	}
	defer os.Remove(out.Name())
	defer out.Close()

	var summaryFile *os.File
	if summaryPath != "" {
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
	if len(summary.Documents) == 0 {
		fmt.Fprintln(stderr, "gkg products mentions-scan: warning: no documents selected")
	}
	return nil
}

func checkProductMentionOutputs(outPath, summaryPath string) error {
	outAbs, err := filepath.Abs(outPath)
	if err != nil {
		return fmt.Errorf("resolve -out: %w", err)
	}
	summaryAbs, err := filepath.Abs(summaryPath)
	if err != nil {
		return fmt.Errorf("resolve -summary: %w", err)
	}
	if strings.EqualFold(outAbs, summaryAbs) {
		return errors.New("-out and -summary must name different files")
	}
	if !strings.EqualFold(filepath.Base(outAbs), filepath.Base(summaryAbs)) {
		return nil
	}
	// Different paths to the parent can still resolve to the same directory.
	outDir, err := os.Stat(filepath.Dir(outAbs))
	if err != nil {
		return err
	}
	summaryDir, err := os.Stat(filepath.Dir(summaryAbs))
	if err != nil {
		return err
	}
	if os.SameFile(outDir, summaryDir) {
		return errors.New("-out and -summary must name different files")
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

type productMentionSummary struct {
	MatcherRevision string                          `json:"matcher_revision"`
	Release         string                          `json:"release"`
	ActivationSeq   int64                           `json:"activation_seq"`
	Stores          []graph.ProductMentionStore     `json:"stores"`
	Documents       []productMentionDocument        `json:"documents"`
	Counts          productMentionCounts            `json:"counts"`
	Ambiguous       []graph.ProductMentionAmbiguity `json:"ambiguous"`
	Divergence      graph.ProductMentionDivergence  `json:"divergence"`
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
	var index *graph.ProductMentionIndex
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
		index = graph.BuildProductMentionIndex(input.Identifiers)
		summary.Counts.IdentifierValues = index.Stats.IdentifierValues
		summary.Counts.EligibleKeys = index.Stats.EligibleKeys
		summary.Counts.IneligibleValues = index.Stats.IneligibleValues
		summary.Counts.AmbiguousKeys = len(index.Stats.Ambiguous)
		summary.Ambiguous, summary.Divergence = index.Stats.Ambiguous, index.Stats.Divergence
		for i := range summary.Stores {
			store := &summary.Stores[i]
			stats := index.Stats.ByStore[store.Store]
			store.IdentifierProducts, store.EligibleProducts = stats.IdentifierProducts, stats.EligibleProducts
		}
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
		pairs := matchProductMentionPairs(state.doc, chunk, index)
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

func matchProductMentionPairs(doc graph.Document, chunk graph.Chunk, index *graph.ProductMentionIndex) map[string]*productMentionPair {
	matches := index.Match(chunk.Text)
	if len(matches) == 0 {
		return nil
	}
	pairs := make(map[string]*productMentionPair)
	for _, match := range matches {
		product := match.Product
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
		for _, evidence := range match.Evidence {
			pair.Matches = append(pair.Matches, productMentionMatch{
				Key: match.Key, Feature: evidence.Feature, ValueFold: evidence.ValueFold,
				Span: match.Span, Text: match.Text, Tokens: match.Tokens,
			})
			pair.Features = append(pair.Features, evidence.Feature)
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
