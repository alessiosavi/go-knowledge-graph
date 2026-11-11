package graph

import (
	"errors"
	"time"
)

// ErrNotFound reports a missing node or document.
var ErrNotFound = errors.New("graph: not found")

// ErrSuperseded reports a write against a document that has been superseded.
var ErrSuperseded = errors.New("graph: document is superseded")

// Status is a document's pipeline state.
type Status string

// Document statuses, in pipeline order.
const (
	StatusReceived   Status = "received"
	StatusFetched    Status = "fetched"
	StatusChunked    Status = "chunked"
	StatusExtracted  Status = "extracted"
	StatusMerged     Status = "merged"
	StatusVerifying  Status = "verifying"
	StatusDone       Status = "done"
	StatusFailed     Status = "failed"
	StatusSuperseded Status = "superseded"
)

// timeLayout is the fixed-width UTC timestamp format stored on nodes. It is
// deliberately not RFC3339Nano: that format trims trailing zeros, which
// breaks lexicographic ordering of timestamps in Cypher ORDER BY.
const timeLayout = "2006-01-02T15:04:05.000000000Z"

// Document is one content version of one source.
type Document struct {
	ID         string
	SourceKind string
	Source     string
	FinalURL   string
	Title      string
	Hash       string
	Status     Status
	Error      string
	Labels     string
	CreatedAt  time.Time
	UpdatedAt  time.Time
	Supersedes string
}

// Chunk is a piece of a document's cleaned text with absolute byte offsets.
// An absent fingerprint property reads back as a nil slice and a stored
// empty list as a non-nil empty slice; use len() == 0, not == nil, when
// testing them.
type Chunk struct {
	ID               string
	DocID            string
	Seq              int
	Text             string
	Start            int
	End              int
	Hash             string
	ExactKey         string
	MinhashSig       []int64
	BandKeys         []int64
	SignatureVersion int
	DuplicateOf      string
}

// DocumentInput is the identity and content handle of a submitted document.
type DocumentInput struct {
	Kind     string
	Source   string
	FinalURL string
	Title    string
	Hash     string
	Labels   string
	Force    bool // reset an existing live version without changing its version order
}

// UpsertResult tells the pipeline what the upsert did. Noop means the same
// source with the same content hash already exists; PreviousID names the
// live document this version replaces (supersede runs after the new
// document has merged).
type UpsertResult struct {
	ID         string
	Noop       bool
	PreviousID string
}

// Span is a mention's byte range into the chunk text.
type Span struct {
	Start int
	End   int
	Text  string
}

// MentionSpan is an entity mention's byte range and extracted type.
type MentionSpan struct {
	Start int
	End   int
	Type  string
}

// EntityUpsert is one entity to merge into the graph with its chunk mentions.
type EntityUpsert struct {
	Key      string
	Name     string
	Type     string // seeds a new entity; typed mentions determine its displayed type
	Aliases  []string
	Mentions []MentionSpan
}

// FactUpsert is one extracted relation with its mention evidence.
type FactUpsert struct {
	HeadKey       string
	TailKey       string
	Rel           string
	Confidence    float64
	HeadMention   Span
	TailMention   Span
	LowConfidence bool
}

// EntityRef identifies an entity node.
type EntityRef struct {
	ID   string
	Key  string
	Name string
	Type string
}

// FactView is one fact edge as seen from the queried entity: Outgoing
// reports whether the queried entity is the edge's head, and the excerpt is
// anchored on the queried entity's own mention.
type FactView struct {
	Rel           string
	Other         EntityRef
	Confidence    float64
	Judge         string
	LowConfidence bool
	Outgoing      bool
	DocID         string
	ChunkID       string
	Excerpt       string
}

// EntityView is an entity with its aliases, mention count and fact evidence.
type EntityView struct {
	EntityRef
	Aliases      []string
	MentionCount int
	Facts        []FactView
}
