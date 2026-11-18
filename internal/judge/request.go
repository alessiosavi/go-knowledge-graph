package judge

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"unicode/utf8"
)

const (
	profileVersionV2 = "3"
	requestSchemaV2  = "2"
)

// Profile identifies the prompt, model and definition set for a frozen request.
type Profile struct {
	Version           string `json:"version"`
	Model             string `json:"model"`
	PromptDigest      string `json:"prompt_digest"`
	DefinitionsDigest string `json:"definitions_digest"`
	Fingerprint       string `json:"fingerprint"`
}

// Request is a frozen judge user message. JSON encodes RequestBytes as base64
// so a manifest round trip preserves whitespace, escaping and key order.
type Request struct {
	Profile      Profile `json:"profile"`
	RequestBytes []byte  `json:"request_bytes"`
	DocID        string  `json:"doc_id"`
}

// NewV2Profile builds the v2.1 profile using the definitions applicable to
// the run, indexed by preset and then relation.
func NewV2Profile(model string, definitions map[string]map[string]string) Profile {
	canonical := canonicalDefinitions(definitions)
	return Profile{
		Version:           profileVersionV2,
		Model:             model,
		PromptDigest:      fmt.Sprintf("%x", sha256.Sum256([]byte(PromptV2))),
		DefinitionsDigest: fmt.Sprintf("%x", sha256.Sum256(canonical)),
		Fingerprint:       fingerprint(PromptV2, canonical, model, requestSchemaV2),
	}
}

// Fingerprint identifies the prompt, definitions, model and user-message
// schema. JSON orders definition keys by preset and relation. Length prefixes
// distinguish changes that move bytes between successive inputs.
func Fingerprint(prompt string, definitions map[string]map[string]string, model, schemaVersion string) string {
	return fingerprint(prompt, canonicalDefinitions(definitions), model, schemaVersion)
}

func canonicalDefinitions(definitions map[string]map[string]string) []byte {
	if definitions == nil {
		return []byte("{}")
	}
	canonical, _ := json.Marshal(definitions)
	return canonical
}

func fingerprint(prompt string, canonical []byte, model, schemaVersion string) string {
	h := sha256.New()
	for _, part := range [][]byte{[]byte(prompt), canonical, []byte(model), []byte(schemaVersion)} {
		_, _ = h.Write(binary.BigEndian.AppendUint64(nil, uint64(len(part))))
		_, _ = h.Write(part)
	}
	return hex.EncodeToString(h.Sum(nil)[:8])
}

// RequestKeyV2 hashes the exact v2 user message followed by its profile
// fingerprint. The owning document is bookkeeping only.
func RequestKeyV2(requestBytes []byte, fingerprint string) string {
	h := sha256.New()
	_, _ = h.Write(requestBytes)
	_, _ = h.Write([]byte(fingerprint))
	return hex.EncodeToString(h.Sum(nil))
}

func (r Request) validate(model string, definitions map[string]map[string]string) (string, error) {
	if r.Profile.Version != profileVersionV2 {
		return "", fmt.Errorf("judge: unsupported profile version %q", r.Profile.Version)
	}
	if r.Profile.Model != model {
		return "", fmt.Errorf("judge: request model %q does not match configured model %q", r.Profile.Model, model)
	}
	current := NewV2Profile(model, definitions)
	if r.Profile.PromptDigest != current.PromptDigest {
		return "", fmt.Errorf("judge: profile prompt digest does not match the current prompt")
	}
	if r.Profile.DefinitionsDigest != current.DefinitionsDigest {
		return "", fmt.Errorf("judge: profile definitions digest does not match the current definitions")
	}
	if r.Profile.Fingerprint != current.Fingerprint {
		return "", fmt.Errorf("judge: profile fingerprint does not match the current profile")
	}
	if len(r.RequestBytes) == 0 || !utf8.Valid(r.RequestBytes) {
		return "", fmt.Errorf("judge: request bytes must be non-empty UTF-8")
	}
	return PromptV2, nil
}
