// Package quality computes versioned, read-only graph quality metrics over
// done documents and their derived data.
package quality

import (
	"context"
	"fmt"
	"io"
	"os"

	"github.com/alessiosavi/go-knowledge-graph/internal/config"
	"github.com/alessiosavi/go-knowledge-graph/internal/hygiene"

	"github.com/alessiosavi/go-knowledge-graph/internal/domain"
	"github.com/alessiosavi/go-knowledge-graph/internal/graph"
)

const MetricsVersion = 2

// HygienePolicy describes the policy used for both noise and private samples.
const HygienePolicy = "built-in preset plus configured stoplist overlay; label = entity.type"

type Options struct {
	Domain        string
	Seed          *domain.Seed
	HygieneSample int
	SampleSeed    uint64
}

// Report contains aggregates only. Undefined metrics encode as null; Sample is
// deliberately excluded from JSON and must be written only to the requested file.
type Report struct {
	Metrics               int              `json:"metrics"`
	Documents             int              `json:"documents"`
	Chunks                int              `json:"chunks"`
	Entities              int              `json:"entities"`
	Mentions              int              `json:"mentions"`
	IsolatedShare         *float64         `json:"isolated_share"`
	SingletonShare        *float64         `json:"singleton_share"`
	SingleDocumentShare   *float64         `json:"single_document_share"`
	MentionGini           *float64         `json:"mention_gini"`
	Top1PctMentionShare   *float64         `json:"top1pct_mention_share"`
	CrossDomainEntities   *int             `json:"cross_domain_entities"`
	DomainCoverage        *float64         `json:"domain_coverage"`
	FactComponents        int              `json:"fact_components"`
	GiantShareOfFactNodes *float64         `json:"giant_share_of_fact_nodes"`
	GiantShareOfEntities  *float64         `json:"giant_share_of_entities"`
	Facts                 FactCounts       `json:"facts"`
	LowConfidenceShare    *float64         `json:"low_confidence_share"`
	ConfidenceLT04Share   *float64         `json:"confidence_lt_0_4_share"`
	Support               SupportMetrics   `json:"support"`
	RelationTypes         int              `json:"relation_types"`
	RelationEntropyBits   *float64         `json:"relation_entropy_bits"`
	MaxEntropyBits        *float64         `json:"max_entropy_bits"`
	MultiTypePairs        int              `json:"multi_type_pairs"`
	InversePairs          int              `json:"inverse_pairs"`
	SelfLoops             int              `json:"self_loops"`
	DuplicateGroups       int              `json:"duplicate_groups"`
	DuplicateEntities     int              `json:"duplicate_entities"`
	CrossTypeGroups       int              `json:"cross_type_groups"`
	NoiseRuleShare        *float64         `json:"noise_rule_share"`
	CoMention             CoMentionMetrics `json:"co_mention"`
	HygienePolicy         string           `json:"hygiene_policy"`
	Sample                HygieneSample    `json:"-"`
}

type FactCounts struct {
	Typed              int  `json:"typed"`
	CandidatesUnjudged *int `json:"candidates_unjudged"`
	CandidatesUnclear  *int `json:"candidates_unclear"`
	Rejected           int  `json:"rejected"`
}

type SupportMetrics struct {
	Triples            int      `json:"triples"`
	OneChunkShare      *float64 `json:"one_chunk_share"`
	MultiDocumentShare *float64 `json:"multi_document_share"`
}

type CoMentionMetrics struct {
	Nodes        int      `json:"nodes"`
	Edges        int      `json:"edges"`
	Weight1Share *float64 `json:"weight1_share"`
	GiantShare   *float64 `json:"giant_share"`
}

// HygieneSample is private: it contains original entity names.
type HygieneSample struct {
	Metrics          int          `json:"metrics"`
	HygienePolicy    string       `json:"hygiene_policy"`
	SampleSeed       uint64       `json:"sample_seed"`
	Requested        int          `json:"requested_per_kind"`
	AvailableDropped int          `json:"available_dropped"`
	AvailableRenamed int          `json:"available_renamed"`
	Dropped          []SampleName `json:"dropped"`
	Renamed          []SampleName `json:"renamed"`
}

type SampleName struct {
	Key        string `json:"key"`
	Type       string `json:"type"`
	Original   string `json:"original"`
	Normalized string `json:"normalized"`
	Rule       string `json:"rule"`
}

type document struct{ id, source, domain string }
type chunk struct{ id, docID string }
type entity struct{ key, name, typ string }
type mention struct{ entity, chunk string }
type fact struct {
	head, tail, rel, chunk, doc string
	confidence                  float64
	low                         bool
}
type dataset struct {
	version                     int
	documents                   []document
	chunks                      []chunk
	entities                    []entity
	mentions                    []mention
	facts                       []fact
	unjudged, unclear, rejected int
}

func Compute(ctx context.Context, st *graph.Store, opt Options) (Report, error) {
	report, _, err := computeReport(ctx, st, opt, nil)
	return report, err
}

// ComputeWithDuplicates computes the usual report and writes the private JSONL
// census to out. The returned summary contains only aggregate counts.
func ComputeWithDuplicates(ctx context.Context, st *graph.Store, opt Options, out io.Writer) (Report, DuplicateSummary, error) {
	return computeReport(ctx, st, opt, out)
}

func computeReport(ctx context.Context, st *graph.Store, opt Options, duplicates io.Writer) (Report, DuplicateSummary, error) {
	var summary DuplicateSummary
	if opt.HygieneSample < 0 {
		return Report{}, summary, fmt.Errorf("quality: hygiene sample must be nonnegative")
	}
	if opt.Domain != "" && !domain.ValidDomain(opt.Domain) {
		return Report{}, summary, fmt.Errorf("quality: invalid domain slug")
	}
	// Match the CLI's config lookup while preserving the pinned Options API.
	path := os.Getenv("GKG_CONFIG")
	if path == "" {
		if _, err := os.Stat("config.yaml"); err == nil {
			path = "config.yaml"
		}
	}
	cfg, err := config.Load(path)
	if err != nil {
		return Report{}, summary, err
	}
	preset, err := hygiene.LoadPreset(cfg.Labels.PresetsDir, "", cfg.Ingest.StoplistFile)
	if err != nil {
		return Report{}, summary, err
	}
	data, err := load(ctx, st, opt)
	if err != nil {
		return Report{}, summary, err
	}
	if err := ctx.Err(); err != nil {
		return Report{}, summary, err
	}
	var report Report
	if duplicates == nil {
		report = compute(data, opt, preset)
	} else {
		var groups map[string][]duplicateGroup
		report, groups = computeWithCensus(data, opt, preset, true)
		summary, err = writeDuplicates(ctx, st, duplicates, groups)
		if err != nil {
			return Report{}, DuplicateSummary{}, err
		}
	}
	if err := ctx.Err(); err != nil {
		return Report{}, DuplicateSummary{}, err
	}
	return report, summary, nil
}
