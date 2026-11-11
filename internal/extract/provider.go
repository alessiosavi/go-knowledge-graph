// Package extract defines the extraction provider contract and the shipped
// providers (mock now; openai and sidecar follow). The contract is frozen:
// providers report mentions with byte offsets into the submitted chunk text
// and relation candidates indexing into the same chunk's mentions.
package extract

import "context"

// Label is an extraction label with its per-call threshold and local identity scope.
type Label struct {
	Name      string  `yaml:"name" json:"name"`
	Threshold float64 `yaml:"threshold" json:"threshold"`
	Scope     string  `yaml:"scope" json:"-"`
}

// RequestChunk is one chunk submitted for extraction. The JSON tags are
// part of the frozen sidecar wire contract ({"id", "text"}).
type RequestChunk struct {
	ID   string `json:"id"`
	Text string `json:"text"`
}

// Request is one extraction call: the chunks plus the labels to look for.
type Request struct {
	Chunks    []RequestChunk
	Entities  []Label
	Relations []Label
}

// Mention is one entity occurrence inside a chunk.
type Mention struct {
	Label string
	Text  string
	Start int
	End   int
	Score float64
}

// Candidate is one relation candidate; Head and Tail index into the same
// chunk result's Mentions.
type Candidate struct {
	Label string
	Head  int
	Tail  int
	Score float64
}

// ChunkResult is the provider's answer for one chunk.
type ChunkResult struct {
	ChunkID    string
	Mentions   []Mention
	Candidates []Candidate
}

// Provider extracts entities and relations from chunks.
type Provider interface {
	// Name identifies the provider kind (mock, openai, sidecar).
	Name() string
	// Version identifies the provider configuration, stored on every fact.
	Version() string
	// Extract processes the request; results carry one entry per submitted
	// chunk (providers may batch internally).
	Extract(ctx context.Context, req Request) ([]ChunkResult, error)
}
