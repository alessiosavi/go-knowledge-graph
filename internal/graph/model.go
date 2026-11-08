package graph

import (
	"errors"
	"time"
)

// ErrNotFound reports a missing node or document.
var ErrNotFound = errors.New("graph: not found")

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
	CreatedAt  time.Time
	UpdatedAt  time.Time
	Supersedes string
}

// Chunk is a piece of a document's cleaned text with absolute byte offsets.
// Empty or absent fingerprint lists read back as non-nil empty slices; use
// len() == 0, not == nil, when testing them.
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
