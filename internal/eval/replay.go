package eval

import (
	"cmp"
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"slices"
	"strings"

	"github.com/alessiosavi/go-knowledge-graph/internal/config"
	"github.com/alessiosavi/go-knowledge-graph/internal/extract"
	"github.com/alessiosavi/go-knowledge-graph/internal/graph"
	"github.com/alessiosavi/go-knowledge-graph/internal/hygiene"
	"github.com/alessiosavi/go-knowledge-graph/internal/ingest"
)

type replayHeader struct {
	Type               string                `json:"type"`
	InputSHA256        string                `json:"input_sha256"`
	Preset             string                `json:"preset"`
	PresetSHA256       string                `json:"preset_sha256"`
	LabelMismatches    []replayLabelMismatch `json:"label_mismatches"`
	Hygiene            hygiene.Preset        `json:"hygiene"`
	WriteMode          replayWriteMode       `json:"write_mode"`
	StoreFloor         float64               `json:"store_floor"`
	AssertFloor        float64               `json:"assert_floor"`
	SidecarModel       string                `json:"sidecar_model,omitempty"`
	SidecarImage       string                `json:"sidecar_image"`
	SidecarImageDigest string                `json:"sidecar_image_digest"`
	ExtractorVersion   string                `json:"extractor_version"`
	GKGVersion         string                `json:"gkg_version"`
}

type replayWriteMode struct {
	Hygiene           bool `json:"hygiene"`
	Domains           bool `json:"domains"`
	FactsAsCandidates bool `json:"facts_as_candidates"`
}

type replayLabelMismatch struct {
	DocID  string `json:"doc_id"`
	Labels string `json:"labels"`
}

type replayResult struct {
	Type          string  `json:"type"`
	ChunkID       string  `json:"chunk_id"`
	DocID         string  `json:"doc_id"`
	Labels        *string `json:"labels"`
	LabelMismatch bool    `json:"label_mismatch"`
	DuplicateRoot string  `json:"duplicate_root,omitempty"`
	Status        string  `json:"status"`
	Error         string  `json:"error"`
}

type replayOccurrence struct {
	Type          string `json:"type"`
	ChunkID       string `json:"chunk_id"`
	DocID         string `json:"doc_id"`
	Title         string `json:"document_title"`
	SourceChunkID string `json:"source_chunk_id,omitempty"` // source of a borrowed occurrence's offsets and evidence
	ingest.OccurrenceDecision
}

type replayAssessment struct {
	occurrences []ingest.OccurrenceDecision
	err         error
}

type ReplayCounts struct {
	Chunks                      int `json:"chunks"`
	OK                          int `json:"ok"`
	Empty                       int `json:"empty"`
	Errors                      int `json:"errors"`
	Occurrences                 int `json:"occurrences"`
	StoredWinners               int `json:"stored_winners"`
	DuplicateRootsOutsideSample int `json:"duplicate_roots_outside_sample"`
	DuplicateRootCycles         int `json:"duplicate_root_cycles"`
}

const (
	duplicateRootOutsideSample = "outside_sample"
	duplicateRootCycle         = "cycle"
)

// Replay predicts relations from an intact, frozen sample. It reads the
// graph's schema marker and runs extraction on the configured sidecar.
func Replay(ctx context.Context, cfg config.Config, input, output, presetName, gkgVersion string, stderr io.Writer) (ReplayCounts, error) {
	frozen, err := readSample(input)
	if err != nil {
		return ReplayCounts{}, fmt.Errorf("read sample: %w", err)
	}
	preset, err := ingest.LoadCheckedPreset(cfg.Labels.PresetsDir, presetName, cfg.Ingest.StoplistFile)
	if err != nil {
		return ReplayCounts{}, err
	}
	mode, err := readReplayWriteMode(ctx, cfg)
	if err != nil {
		return ReplayCounts{}, err
	}

	provider := extract.NewReplaySidecar(cfg)
	extractorVersion := provider.Version()
	counts := ReplayCounts{Chunks: frozen.Header.Counts.Chunks}
	err = writeArtifact(output, func(enc *json.Encoder) (any, error) {
		image := cfg.Extractor.Sidecar.Image
		digest := replaySidecarImageDigest(image)
		if digest == "unknown" {
			fmt.Fprintln(stderr, "gkg eval: warning: sidecar image digest is unknown")
		}
		model := ""
		var healthErr error
		if counts.Chunks > 0 {
			model, healthErr = provider.HealthModel(ctx)
		}
		mismatches := make([]replayLabelMismatch, 0)
		for _, d := range frozen.Documents {
			if d.Labels != nil && *d.Labels != presetName {
				mismatches = append(mismatches, replayLabelMismatch{DocID: d.DocID, Labels: *d.Labels})
			}
		}
		if err := enc.Encode(replayHeader{
			Type: "header", InputSHA256: frozen.SHA256, Preset: presetName, PresetSHA256: preset.SHA256,
			LabelMismatches: mismatches,
			WriteMode:       replayWriteMode{Hygiene: mode.Hygiene, Domains: mode.Domains, FactsAsCandidates: mode.RequireConfirmation},
			Hygiene:         preset.Hygiene, StoreFloor: cfg.Ingest.StoreFloor, AssertFloor: cfg.Ingest.AssertFloor,
			SidecarModel: model, SidecarImage: image, SidecarImageDigest: digest,
			ExtractorVersion: extractorVersion, GKGVersion: gkgVersion,
		}); err != nil {
			return nil, err
		}

		// Assess roots before writing result rows so a duplicate can refer to a
		// chunk in a later document while preserving the sample's result order.
		assessments := make(map[string]replayAssessment)
		duplicateLinks := make(map[string]string, counts.Chunks)
		for _, d := range frozen.Documents {
			slices.SortStableFunc(d.Chunks, func(a, b sampleChunk) int { return cmp.Compare(a.Seq, b.Seq) })
			req := extract.Request{
				Chunks: make([]extract.RequestChunk, 0, len(d.Chunks)), Entities: preset.Entities, Relations: preset.Relations,
			}
			for _, c := range d.Chunks {
				duplicateLinks[c.ChunkID] = c.DuplicateOf
				if c.DuplicateOf == "" {
					req.Chunks = append(req.Chunks, extract.RequestChunk{ID: c.ChunkID, Text: c.Text})
				}
			}
			if healthErr != nil {
				continue
			}
			results, err := provider.ExtractReplay(ctx, req)
			if err != nil {
				return nil, err
			}
			doc := graph.Document{ID: d.DocID, Domain: frozen.Header.Domain, Title: d.Title}
			for i, raw := range results {
				c := req.Chunks[i]
				assessment := replayAssessment{err: raw.Err}
				if raw.Err == nil && len(raw.Candidates) > 0 {
					chunk := graph.Chunk{ID: c.ID, DocID: d.DocID, Text: c.Text}
					assessment.occurrences = ingest.AssessReplay(cfg.Ingest, preset, doc, chunk, raw.ChunkResult, extractorVersion, mode)
				}
				assessments[c.ID] = assessment
			}
		}
		for _, d := range frozen.Documents {
			labelMismatch := d.Labels != nil && *d.Labels != presetName
			for _, c := range d.Chunks {
				if err := ctx.Err(); err != nil {
					return nil, err
				}
				res := replayResult{Type: "result", ChunkID: c.ChunkID, DocID: d.DocID, Labels: d.Labels, LabelMismatch: labelMismatch}
				rootID := c.ChunkID
				var sourceChunkID string
				if c.DuplicateOf != "" {
					rootID, res.DuplicateRoot = replayRoot(c.ChunkID, duplicateLinks)
					sourceChunkID = rootID
				} else if c.DuplicateRoot == duplicateRootCycle {
					res.DuplicateRoot = duplicateRootCycle
				}
				switch res.DuplicateRoot {
				case duplicateRootOutsideSample:
					counts.DuplicateRootsOutsideSample++
				case duplicateRootCycle:
					counts.DuplicateRootCycles++
				}
				assessment := assessments[rootID]
				if healthErr != nil {
					assessment.err = healthErr
				}
				switch {
				case assessment.err != nil:
					res.Status, res.Error = "error", assessment.err.Error()
					counts.Errors++
				case len(assessment.occurrences) == 0:
					res.Status = "empty"
					counts.Empty++
				default:
					res.Status = "ok"
					counts.OK++
				}
				if err := enc.Encode(res); err != nil {
					return nil, err
				}
				for _, decision := range assessment.occurrences {
					decision.StoredWinner = c.DuplicateOf == "" && decision.StoredWinner
					if err := enc.Encode(replayOccurrence{
						Type: "occurrence", ChunkID: c.ChunkID, DocID: d.DocID, Title: d.Title, SourceChunkID: sourceChunkID, OccurrenceDecision: decision,
					}); err != nil {
						return nil, err
					}
					counts.Occurrences++
					if decision.StoredWinner {
						counts.StoredWinners++
					}
				}
			}
		}
		return counts, ctx.Err()
	})
	return counts, err
}

func replayRoot(id string, duplicateLinks map[string]string) (rootID, issue string) {
	seen := make(map[string]bool)
	for {
		if seen[id] {
			return "", duplicateRootCycle
		}
		next, sampled := duplicateLinks[id]
		if !sampled {
			return "", duplicateRootOutsideSample
		}
		if next == "" {
			return id, ""
		}
		seen[id] = true
		id = next
	}
}

func replaySidecarImageDigest(image string) string {
	image = strings.TrimSpace(image)
	if _, digest, pinned := strings.Cut(image, "@"); pinned {
		image = digest
	}
	if value, ok := strings.CutPrefix(image, "sha256:"); ok && len(value) == 64 {
		if _, err := hex.DecodeString(value); err == nil {
			return "sha256:" + strings.ToLower(value)
		}
	}
	return "unknown"
}

func readReplayWriteMode(ctx context.Context, cfg config.Config) (ingest.WriteMode, error) {
	st, err := graph.Open(ctx, cfg)
	if err != nil {
		return ingest.WriteMode{}, err
	}
	defer func() { _ = st.Close(context.Background()) }()
	schema, err := st.CheckExtractionSchema(ctx)
	if err != nil {
		return ingest.WriteMode{}, err
	}
	if schema != graph.ExtractionSchemaVersion {
		return ingest.WriteMode{}, fmt.Errorf(
			"replay requires extraction schema v%d (graph is v%d); migrate the graph first",
			graph.ExtractionSchemaVersion, schema,
		)
	}
	return ingest.WriteModeForSchema(schema, cfg.Facts.RequireConfirmation), nil
}
