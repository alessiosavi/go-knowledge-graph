package graph

import (
	"errors"
	"time"

	"github.com/alessiosavi/go-knowledge-graph/internal/textclean"
)

// ErrNotFound reports a missing node or document.
var ErrNotFound = errors.New("graph: not found")

// ErrNotDone reports a rebuild attempt against a document that is not done.
var ErrNotDone = errors.New("graph: document is not done")

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
	Domain      string
	ID          string
	SourceKind  string
	Source      string
	FinalURL    string
	Title       string
	TitleSource string
	Hash        string
	Status      Status
	Error       string
	Labels      string
	CreatedAt   time.Time
	UpdatedAt   time.Time
	Supersedes  string

	RawDocumentDate    string // caller-supplied value in timeLayout, empty when absent
	DocumentDate       *time.Time
	DocumentDateSource *string // "caller" or "ingest"

	ProductRefs        []string
	SpoolPayloadDigest string
}

// Chunk is a piece of a document's cleaned text with absolute byte offsets.
// ChunkNeighbourhood includes the owning document's effective date and origin.
// An absent fingerprint property reads back as a nil slice and a stored
// empty list as a non-nil empty slice; use len() == 0, not == nil, when
// testing them.
type Chunk struct {
	Domain           string
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

	DocumentDate       *time.Time
	DocumentDateSource *string
}

// DocumentInput is the identity and content handle of a submitted document.
type DocumentInput struct {
	// Domain applies on version creation or when product refs pin an unassigned version.
	Domain      string
	Kind        string
	Source      string
	FinalURL    string
	Title       string
	TitleSource string
	Hash        string
	Labels      string
	Force       bool // reset an existing live version without changing its version order

	DocumentDate         string   // normalized caller date, empty when omitted
	ProductRefs          []string // nil means omitted; an empty list clears refs
	SpoolPayloadDigest   string
	productRefsValidated bool

	TextCleanVersion int // Zero when the input has no cleaning metadata.
	TextClean        textclean.Stats
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
	// JudgeInput is the canonical judge request. Candidates require it.
	// Typed edges store it only when StoreJudgeInput is enabled.
	JudgeInput []byte
	// JudgeGen identifies this write of a typed edge under StoreJudgeInput.
	JudgeGen string
}

// EntityRef identifies an entity node.
type EntityRef struct {
	Domain string
	ID     string
	Key    string
	Name   string
	Type   string
}

// Provenance identifies a selector that resolved through the merge ledger.
type Provenance struct {
	Requested   string
	ResolvedVia string
}

// EntityCandidate is a name lookup hit from the exact or fold tier.
type EntityCandidate struct {
	EntityRef
	Match string
}

const (
	EntityMatchExact = "exact"
	EntityMatchFold  = "fold"
)

// CoMention is an entity sharing chunks with the viewed entity.
type CoMention struct {
	EntityRef
	SharedChunks int
}

// ViewOptions controls fact visibility. By default, typed edges are visible
// if they meet the assert floor or the judge confirmed them. IncludeLowConfidence
// adds all other typed edges. IncludeUnverified adds candidates at any confidence.
// ConfirmedOnly takes precedence and returns only judge-confirmed typed edges.
// An empty Domain selects all domains for the start entity.
// DateWindow restricts fact evidence by effective document date.
type ViewOptions struct {
	Domain               string
	IncludeLowConfidence bool
	IncludeUnverified    bool
	ConfirmedOnly        bool
	DateWindow           DateWindow
}

// HiddenCounts counts distinct (head_key, rel, tail_key) facts in each state
// touching the viewed entity (the start entity for traversal), independently
// of visibility options.
type HiddenCounts struct {
	Unclear  int `json:"unclear"`
	Unjudged int `json:"unjudged"`
	Rejected int `json:"rejected"`
}

// FactView is one fact edge as seen from the queried entity: Outgoing
// reports whether the queried entity is the edge's head, and the excerpt is
// anchored on the queried entity's own mention.
type FactView struct {
	Rel           string
	Other         EntityRef
	Confidence    float64
	Judge         string
	Unverified    bool
	LowConfidence bool
	Outgoing      bool
	DocID         string
	ChunkID       string
	Excerpt       string

	DocumentDate       *time.Time
	DocumentDateSource *string
}

// EntityView is an entity with aliases, mention count, fact evidence and co-mentions.
type EntityView struct {
	EntityRef
	Requested    string `json:"requested,omitempty"`
	ResolvedVia  string `json:"resolved_via,omitempty"`
	Aliases      []string
	MentionCount int
	Facts        []FactView
	Together     []CoMention
	Hidden       HiddenCounts
}
