package eval

import (
	"cmp"
	"context"
	"crypto/rand"
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
	AttemptID          string                `json:"attempt_id,omitempty"`
	NoBorrow           bool                  `json:"no_borrow,omitempty"`
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
	ingest.ReplayAssessment
	raw extract.ChunkResult
	err error
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

type ReplayOptions struct {
	MentionsPath string
	NoBorrow     bool
}

var newReplayAttemptID = func() (string, error) {
	var nonce [16]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(nonce[:]), nil
}

// Replay predicts relations from an intact, frozen sample. It reads the
// graph's schema marker and runs extraction on the configured sidecar.
func Replay(ctx context.Context, cfg config.Config, input, output, presetName, gkgVersion string, stderr io.Writer) (ReplayCounts, error) {
	return ReplayWithOptions(ctx, cfg, input, output, presetName, gkgVersion, ReplayOptions{}, stderr)
}

// ReplayWithOptions can also write validated mentions and extract duplicates locally.
func ReplayWithOptions(ctx context.Context, cfg config.Config, input, output, presetName, gkgVersion string, opts ReplayOptions, stderr io.Writer) (ReplayCounts, error) {
	paths := []string{output}
	if opts.MentionsPath != "" {
		paths = append(paths, opts.MentionsPath)
	}
	var counts ReplayCounts
	err := writeArtifacts(paths, func(encoders []*json.Encoder) ([]any, error) {
		var mentions *replayMentions
		if len(encoders) == 2 {
			mentions = &replayMentions{enc: encoders[1]}
		}
		var err error
		counts, err = replay(ctx, cfg, input, presetName, gkgVersion, opts.NoBorrow, encoders[0], mentions, stderr)
		if err != nil {
			return nil, err
		}
		trailers := []any{counts}
		if mentions != nil {
			trailers = append(trailers, mentions.trailerCounts())
		}
		return trailers, nil
	})
	return counts, err
}

func replay(ctx context.Context, cfg config.Config, input, presetName, gkgVersion string, noBorrow bool, enc *json.Encoder, mentions *replayMentions, stderr io.Writer) (ReplayCounts, error) {
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
	header := replayHeader{
		Type: "header", InputSHA256: frozen.SHA256, Preset: presetName, PresetSHA256: preset.SHA256,
		LabelMismatches: mismatches,
		WriteMode:       replayWriteMode{Hygiene: mode.Hygiene, Domains: mode.Domains, FactsAsCandidates: mode.RequireConfirmation},
		Hygiene:         preset.Hygiene, StoreFloor: cfg.Ingest.StoreFloor, AssertFloor: cfg.Ingest.AssertFloor,
		SidecarModel: model, SidecarImage: image, SidecarImageDigest: digest,
		ExtractorVersion: extractorVersion, GKGVersion: gkgVersion, NoBorrow: noBorrow,
	}
	if mentions != nil {
		mentions.counts.ChunksRequested = counts.Chunks
	}
	if err := writeReplayHeaders(enc, mentions, header, preset); err != nil {
		return counts, err
	}

	// Assess roots before writing result rows so a duplicate can refer to a
	// chunk in a later document. Keep the stored order for the mentions file.
	assessments := make(map[string]replayAssessment)
	duplicateLinks := make(map[string]string, counts.Chunks)
	chunksBySequence := make([][]sampleChunk, len(frozen.Documents))
	compareSeq := func(a, b sampleChunk) int { return cmp.Compare(a.Seq, b.Seq) }
	for i, d := range frozen.Documents {
		chunks := d.Chunks
		if !slices.IsSortedFunc(chunks, compareSeq) {
			if mentions != nil {
				chunks = slices.Clone(chunks)
			}
			slices.SortStableFunc(chunks, compareSeq)
		}
		chunksBySequence[i] = chunks
		req := extract.Request{
			Chunks: make([]extract.RequestChunk, 0, len(chunks)), Entities: preset.Entities, Relations: preset.Relations,
		}
		for _, c := range chunks {
			duplicateLinks[c.ChunkID] = c.DuplicateOf
			if noBorrow || c.DuplicateOf == "" {
				req.Chunks = append(req.Chunks, extract.RequestChunk{ID: c.ChunkID, Text: c.Text})
			}
		}
		if healthErr != nil {
			continue
		}
		results, err := provider.ExtractReplay(ctx, req)
		if err != nil {
			return counts, err
		}
		doc := graph.Document{ID: d.DocID, Domain: frozen.Header.Domain, Title: d.Title}
		for i, raw := range results {
			c := req.Chunks[i]
			assessment := replayAssessment{err: raw.Err}
			if raw.Err == nil {
				chunk := graph.Chunk{ID: c.ID, DocID: d.DocID, Text: c.Text}
				assessment.ReplayAssessment = ingest.AssessReplay(cfg.Ingest, preset, doc, chunk, raw.ChunkResult, extractorVersion, mode)
				if mentions != nil {
					assessment.raw = raw.ChunkResult
				} else {
					assessment.Mentions = nil
				}
			}
			assessments[c.ID] = assessment
		}
	}
	failed := make(map[string]error)
	borrowedRoots := make(map[string]string)
	for i, d := range frozen.Documents {
		labelMismatch := d.Labels != nil && *d.Labels != presetName
		for _, c := range chunksBySequence[i] {
			if err := ctx.Err(); err != nil {
				return counts, err
			}
			res := replayResult{Type: "result", ChunkID: c.ChunkID, DocID: d.DocID, Labels: d.Labels, LabelMismatch: labelMismatch}
			extractedID := c.ChunkID
			var sourceChunkID string
			if c.DuplicateOf != "" {
				var rootID string
				rootID, res.DuplicateRoot = replayRoot(c.ChunkID, duplicateLinks)
				if !noBorrow {
					extractedID, sourceChunkID = rootID, rootID
					if mentions != nil && rootID != "" {
						borrowedRoots[c.ChunkID] = rootID
					}
				}
			} else if c.DuplicateRoot == duplicateRootCycle {
				res.DuplicateRoot = duplicateRootCycle
			}
			switch res.DuplicateRoot {
			case duplicateRootOutsideSample:
				counts.DuplicateRootsOutsideSample++
			case duplicateRootCycle:
				counts.DuplicateRootCycles++
			}
			assessment := assessments[extractedID]
			if healthErr != nil {
				assessment.err = healthErr
			}
			switch {
			case assessment.err != nil:
				res.Status, res.Error = "error", assessment.err.Error()
				counts.Errors++
				if mentions != nil {
					failed[c.ChunkID] = assessment.err
				}
			case len(assessment.Occurrences) == 0:
				res.Status = "empty"
				counts.Empty++
			default:
				res.Status = "ok"
				counts.OK++
			}
			if err := enc.Encode(res); err != nil {
				return counts, err
			}
			for _, decision := range assessment.Occurrences {
				decision.StoredWinner = (noBorrow || c.DuplicateOf == "") && decision.StoredWinner
				if err := enc.Encode(replayOccurrence{
					Type: "occurrence", ChunkID: c.ChunkID, DocID: d.DocID, Title: d.Title, SourceChunkID: sourceChunkID, OccurrenceDecision: decision,
				}); err != nil {
					return counts, err
				}
				counts.Occurrences++
				if decision.StoredWinner {
					counts.StoredWinners++
				}
			}
		}
	}
	if mentions != nil {
		for _, d := range frozen.Documents {
			for _, c := range d.Chunks {
				if err := ctx.Err(); err != nil {
					return counts, err
				}
				// Borrowed prediction evidence has offsets in the root chunk.
				// Only local extraction can supply mentions for this chunk.
				assessment := assessments[c.ChunkID]
				assessment.err = failed[c.ChunkID]
				if err := mentions.writeChunk(d, c, assessment, borrowedRoots[c.ChunkID]); err != nil {
					return counts, err
				}
			}
		}
	}
	return counts, ctx.Err()
}

func writeReplayHeaders(enc *json.Encoder, mentions *replayMentions, header replayHeader, preset ingest.CheckedPreset) error {
	if mentions != nil {
		attemptID, err := newReplayAttemptID()
		if err != nil {
			return fmt.Errorf("generate replay attempt ID: %w", err)
		}
		header.AttemptID = attemptID
		if err := mentions.writeHeader(header, preset); err != nil {
			return err
		}
	}
	return enc.Encode(header)
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
