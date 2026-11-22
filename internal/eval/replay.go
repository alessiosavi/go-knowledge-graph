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

	"github.com/alessiosavi/go-knowledge-graph/internal/argerr"
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
	Source             string                `json:"source,omitempty"`            // present in every pinned replay
	BackupSHA256       *string               `json:"backup_tar_sha256,omitempty"` // present in every pinned replay
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

type replayJudgeInput struct {
	DocID      string  `json:"doc_id"`
	ChunkID    string  `json:"chunk_id"`
	HeadKey    string  `json:"head_key"`
	Rel        string  `json:"rel"`
	TailKey    string  `json:"tail_key"`
	Confidence float64 `json:"confidence"`
	Payload    []byte  `json:"judge_input_b64"`
	InputHash  string  `json:"input_hash"`
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
	MentionsPath    string
	NoBorrow        bool
	JudgeInputsPath string
	PinsPath        string
}

type replayOutputs struct {
	predictions *json.Encoder
	mentions    *replayMentions
	judgeInputs *json.Encoder
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

// ReplayWithOptions can also write validated mentions or pinned judge inputs.
func ReplayWithOptions(ctx context.Context, cfg config.Config, input, output, presetName, gkgVersion string, opts ReplayOptions, stderr io.Writer) (ReplayCounts, error) {
	if opts.JudgeInputsPath != "" && opts.PinsPath == "" {
		return ReplayCounts{}, argerr.New("-judge-inputs requires -pins FILE")
	}
	write := writeArtifacts
	var pins *replayPins
	if opts.PinsPath != "" {
		var err error
		pins, err = readReplayPins(opts.PinsPath)
		if err != nil {
			return ReplayCounts{}, err
		}
		write = writeStagedArtifacts
	}
	paths := []string{output}
	if opts.MentionsPath != "" {
		paths = append(paths, opts.MentionsPath)
	}
	if opts.JudgeInputsPath != "" {
		paths = append(paths, opts.JudgeInputsPath)
	}
	var counts ReplayCounts
	err := write(paths, func(encoders []*json.Encoder) ([]any, error) {
		outputs := replayOutputs{predictions: encoders[0]}
		if opts.MentionsPath != "" {
			outputs.mentions = &replayMentions{enc: encoders[1]}
		}
		if opts.JudgeInputsPath != "" {
			outputs.judgeInputs = encoders[len(encoders)-1]
		}
		var err error
		counts, err = replay(ctx, cfg, input, presetName, gkgVersion, opts.NoBorrow, pins, outputs, stderr)
		if err != nil {
			return nil, err
		}
		trailers := make([]any, len(encoders))
		trailers[0] = counts
		if outputs.mentions != nil {
			trailers[1] = outputs.mentions.trailerCounts()
		}
		return trailers, nil
	})
	return counts, err
}

func replay(ctx context.Context, cfg config.Config, input, presetName, gkgVersion string, noBorrow bool, pins *replayPins, outputs replayOutputs, stderr io.Writer) (ReplayCounts, error) {
	enc, mentions := outputs.predictions, outputs.mentions
	frozen, err := readSample(input)
	if err != nil {
		return ReplayCounts{}, fmt.Errorf("read sample: %w", err)
	}
	image := cfg.Extractor.Sidecar.Image
	digest := replaySidecarImageDigest(image)
	if pins != nil {
		if err := pins.checkConfig(cfg, presetName, digest); err != nil {
			return ReplayCounts{}, err
		}
	}
	preset, err := ingest.LoadCheckedPreset(cfg.Labels.PresetsDir, presetName, cfg.Ingest.StoplistFile)
	if err != nil {
		return ReplayCounts{}, err
	}
	mode, err := readReplayWriteMode(ctx, cfg, pins)
	if err != nil {
		return ReplayCounts{}, err
	}

	mismatches := make([]replayLabelMismatch, 0)
	for _, d := range frozen.Documents {
		if d.Labels != nil && *d.Labels != presetName {
			if pins != nil {
				return ReplayCounts{}, argerr.Errorf("document %q has labels %q; replay uses %q", d.DocID, *d.Labels, presetName)
			}
			mismatches = append(mismatches, replayLabelMismatch{DocID: d.DocID, Labels: *d.Labels})
		}
	}
	if pins != nil {
		if err := pins.checkPreset(presetName, preset); err != nil {
			return ReplayCounts{}, err
		}
		cfg.Ingest.StoreFloor, cfg.Ingest.AssertFloor = pins.StoreFloor, pins.AssertFloor
	}
	if digest == "unknown" {
		fmt.Fprintln(stderr, "gkg eval: warning: sidecar image digest is unknown")
	}
	if pins != nil {
		digest = pins.SidecarDigest
	}
	provider := extract.NewReplaySidecar(cfg)
	extractorVersion := provider.Version()
	counts := ReplayCounts{Chunks: frozen.Header.Counts.Chunks}
	model := ""
	var healthErr error
	if counts.Chunks > 0 || pins != nil {
		model, healthErr = provider.HealthModel(ctx)
	}
	if pins != nil {
		if err := checkSidecarHealth(model, healthErr); err != nil {
			return counts, err
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
	if pins != nil {
		header.Source, header.BackupSHA256 = pins.Source, &pins.BackupSHA256
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
			if pins != nil && raw.Err != nil {
				return counts, fmt.Errorf("chunk %s: %w", c.ID, raw.Err)
			}
			if raw.Err == nil {
				chunk := graph.Chunk{ID: c.ID, DocID: d.DocID, Text: c.Text}
				export := outputs.judgeInputs != nil && duplicateLinks[c.ID] == ""
				assessment.ReplayAssessment = ingest.AssessReplay(cfg.Ingest, preset, doc, chunk, raw.ChunkResult, extractorVersion, mode, export)
				if export {
					if err := writeReplayJudgeInputs(outputs.judgeInputs, &doc, chunk, assessment.Winners); err != nil {
						return counts, err
					}
					assessment.Winners = nil
				}
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

func writeReplayJudgeInputs(enc *json.Encoder, doc *graph.Document, chunk graph.Chunk, winners []graph.FactUpsert) error {
	for _, fact := range winners {
		payload := ingest.ReplayJudgeInput(doc, chunk, fact)
		if payload == nil {
			return fmt.Errorf("chunk %s: could not encode judge input", chunk.ID)
		}
		if err := enc.Encode(replayJudgeInput{
			DocID: doc.ID, ChunkID: chunk.ID, HeadKey: fact.HeadKey, Rel: graph.SanitizeRel(fact.Rel), TailKey: fact.TailKey,
			Confidence: fact.Confidence, Payload: payload, InputHash: sha256Hex(payload),
		}); err != nil {
			return err
		}
	}
	return nil
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

func readReplayWriteMode(ctx context.Context, cfg config.Config, pins *replayPins) (ingest.WriteMode, error) {
	st, err := graph.Open(ctx, cfg)
	if err != nil {
		return ingest.WriteMode{}, err
	}
	defer func() { _ = st.Close(context.Background()) }()
	schema, err := st.CheckExtractionSchema(ctx)
	if err != nil {
		return ingest.WriteMode{}, err
	}
	if pins != nil && schema != pins.SchemaVersion {
		return ingest.WriteMode{}, argerr.Errorf("pins: schema_version is %d, graph marker is %d", pins.SchemaVersion, schema)
	}
	if schema != graph.ExtractionSchemaVersion {
		return ingest.WriteMode{}, fmt.Errorf(
			"replay requires extraction schema v%d (graph is v%d); migrate the graph first",
			graph.ExtractionSchemaVersion, schema,
		)
	}
	if pins != nil {
		return ingest.WriteMode{Hygiene: pins.WriteMode.Hygiene, Domains: pins.WriteMode.Domains, RequireConfirmation: pins.WriteMode.FactsAsCandidates}, nil
	}
	return ingest.WriteModeForSchema(schema, cfg.Facts.RequireConfirmation), nil
}
