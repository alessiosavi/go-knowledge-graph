package graph

import (
	"fmt"
	"regexp"
	"time"

	"github.com/alessiosavi/go-knowledge-graph/internal/argerr"
	"github.com/neo4j/neo4j-go-driver/v6/neo4j"
)

// time.Parse accepts some inputs outside RFC 3339, including a single-digit
// hour, a comma before fractional seconds, and out-of-range offset components.
var documentTimestampFormat = regexp.MustCompile(`^[0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9]{2}:[0-9]{2}:[0-9]{2}(\.[0-9]+)?(Z|[+-]([01][0-9]|2[0-3]):[0-5][0-9])$`)

// ParseDocumentDate normalizes an optional caller date to the storage format.
// now supplies the server clock for the future-date check.
func ParseDocumentDate(value string, now func() time.Time) (string, error) {
	if value == "" {
		return "", nil
	}
	layout := time.DateOnly
	if documentTimestampFormat.MatchString(value) {
		layout = time.RFC3339
	}
	date, err := time.Parse(layout, value)
	if err != nil {
		return "", argerr.New("document_date must be YYYY-MM-DD or an RFC 3339 timestamp with an offset")
	}
	date = date.UTC()
	if date.Year() < 1900 {
		return "", argerr.New("document_date must be in 1900 or later (UTC)")
	}
	if date.After(now().Add(24 * time.Hour)) {
		return "", argerr.New("document_date must be no more than 24 hours in the future")
	}
	return date.Format(timeLayout), nil
}

// documentDateCypher renders the effective date and its origin for a
// Document variable, which can be null after an optional match.
func documentDateCypher(node string) (effective, origin string) {
	effective = fmt.Sprintf("coalesce(%[1]s.document_date, %[1]s.version_at, %[1]s.created_at)", node)
	origin = fmt.Sprintf("CASE WHEN %[1]s.document_date IS NOT NULL THEN 'caller' WHEN %[1]s IS NOT NULL THEN 'ingest' END", node)
	return effective, origin
}

func documentDateColumns(node string) string {
	effective, origin := documentDateCypher(node)
	return effective + " AS document_date, " + origin + " AS document_date_source"
}

// recordDocumentDate decodes documentDateColumns. The origin is reported only
// with a date, so a Document without any usable date omits both fields.
func recordDocumentDate(r *neo4j.Record) (date *time.Time, source *string) {
	parsed := parseTime(getString(r, "document_date")).UTC()
	if parsed.IsZero() {
		return nil, nil
	}
	if origin := getString(r, "document_date_source"); origin != "" {
		source = &origin
	}
	return &parsed, source
}
