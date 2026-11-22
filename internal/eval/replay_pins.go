package eval

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"github.com/alessiosavi/go-knowledge-graph/internal/argerr"
	"github.com/alessiosavi/go-knowledge-graph/internal/config"
	"github.com/alessiosavi/go-knowledge-graph/internal/extract"
	"github.com/alessiosavi/go-knowledge-graph/internal/ingest"
)

type replayPins struct {
	Source, BackupSHA256, StoplistSHA256 string
	SchemaVersion                        int
	WriteMode                            replayWriteMode
	StoreFloor, AssertFloor              float64
	SidecarModel, SidecarDigest          string
	Presets                              map[string]string
}

func readReplayPins(path string) (*replayPins, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, argerr.Errorf("read pins: %w", err)
	}
	var p replayPins
	var presets []json.RawMessage
	writeMode := pinFields{
		"hygiene": &p.WriteMode.Hygiene, "domains": &p.WriteMode.Domains,
		"facts_as_candidates": &p.WriteMode.FactsAsCandidates,
	}
	sidecar := pinFields{"model": &p.SidecarModel, "digest": &p.SidecarDigest}
	fields := pinFields{
		"source": &p.Source, "backup_tar_sha256": &p.BackupSHA256, "schema_version": &p.SchemaVersion,
		"write_mode": &writeMode, "store_floor": &p.StoreFloor, "assert_floor": &p.AssertFloor,
		"stoplist_sha256": &p.StoplistSHA256, "sidecar": &sidecar, "presets": &presets,
	}
	if err := json.Unmarshal(data, &fields); err != nil {
		return nil, argerr.Errorf("pins: %w", err)
	}
	switch {
	case p.Source == "":
		return nil, argerr.New("pins: source must be non-empty")
	case p.SidecarModel == "":
		return nil, argerr.New("pins: sidecar.model must be non-empty")
	case !validPinSHA256(p.BackupSHA256):
		return nil, argerr.New("pins: backup_tar_sha256 must be 64 lowercase hexadecimal characters")
	case !validPinSHA256(p.StoplistSHA256):
		return nil, argerr.New("pins: stoplist_sha256 must be 64 lowercase hexadecimal characters")
	case p.StoreFloor < 0 || p.StoreFloor > p.AssertFloor || p.AssertFloor > 1:
		return nil, argerr.New("pins: confidence floors must satisfy 0 <= store_floor <= assert_floor <= 1")
	case len(presets) == 0:
		return nil, argerr.New("pins: presets must be non-empty")
	}
	if p.SidecarDigest != "" {
		digest, ok := strings.CutPrefix(p.SidecarDigest, "sha256:")
		if !ok || !validPinSHA256(digest) {
			return nil, argerr.New("pins: sidecar.digest must be empty or sha256: followed by 64 lowercase hexadecimal characters")
		}
	}
	p.Presets = make(map[string]string, len(presets))
	for i, row := range presets {
		var name, digest string
		fields := pinFields{"name": &name, "sha256": &digest}
		if err := json.Unmarshal(row, &fields); err != nil {
			return nil, argerr.Errorf("pins presets[%d]: %w", i, err)
		}
		if !validPinSHA256(digest) {
			return nil, argerr.Errorf("pins: presets[%d].sha256 must be 64 lowercase hexadecimal characters", i)
		}
		if _, exists := p.Presets[name]; exists {
			return nil, argerr.Errorf("pins presets: duplicate preset %q", name)
		}
		p.Presets[name] = digest
	}
	return &p, nil
}

func validPinSHA256(value string) bool {
	if len(value) != hex.EncodedLen(sha256.Size) || value != strings.ToLower(value) {
		return false
	}
	_, err := hex.DecodeString(value)
	return err == nil
}

// pinFields decodes a closed JSON object with case-sensitive required keys.
type pinFields map[string]any

func (fields pinFields) UnmarshalJSON(data []byte) error {
	dec := json.NewDecoder(bytes.NewReader(data))
	if token, err := dec.Token(); err != nil || token != json.Delim('{') {
		return fmt.Errorf("expected an object")
	}
	seen := make(map[string]bool, len(fields))
	for dec.More() {
		token, err := dec.Token()
		if err != nil {
			return err
		}
		key := token.(string)
		dst, ok := fields[key]
		if !ok {
			return fmt.Errorf("unknown key %q", key)
		}
		if seen[key] {
			return fmt.Errorf("duplicate key %q", key)
		}
		var value json.RawMessage
		if err := dec.Decode(&value); err != nil {
			return fmt.Errorf("%s: %w", key, err)
		}
		if bytes.Equal(value, []byte("null")) {
			return fmt.Errorf("%s: null value", key)
		}
		if err := json.Unmarshal(value, dst); err != nil {
			return fmt.Errorf("%s: %w", key, err)
		}
		seen[key] = true
	}
	for key := range fields {
		if !seen[key] {
			return fmt.Errorf("missing key %q", key)
		}
	}
	return nil
}

func (p *replayPins) checkConfig(cfg config.Config, presetName, imageDigest string) error {
	if _, ok := p.Presets[presetName]; !ok {
		return argerr.Errorf("pins: preset %q is missing from presets", presetName)
	}
	if model := extract.ResolveSidecarModel(cfg.Extractor.Sidecar.Model); p.SidecarModel != model {
		return argerr.Errorf("pins: sidecar.model is %q, configured model is %q", p.SidecarModel, model)
	}
	if imageDigest != "unknown" && p.SidecarDigest != imageDigest {
		return argerr.Errorf("pins: sidecar.digest is %q, configured image digest is %q", p.SidecarDigest, imageDigest)
	}
	return nil
}

func (p *replayPins) checkPreset(name string, preset ingest.CheckedPreset) error {
	if p.Presets[name] != preset.SHA256 {
		return argerr.Errorf("pins: preset %q sha256 does not match the loaded preset", name)
	}
	if p.StoplistSHA256 != preset.StoplistSHA256 {
		return argerr.New("pins: stoplist_sha256 does not match the loaded stoplist")
	}
	return nil
}

func (p *replayPins) checkSidecarHealth(model string, healthErr error) error {
	if healthErr != nil {
		return argerr.Errorf("pins: sidecar health check failed: %w", healthErr)
	}
	if model != p.SidecarModel {
		return argerr.Errorf("pins: sidecar.model is %q, health reports %q", p.SidecarModel, model)
	}
	return nil
}
