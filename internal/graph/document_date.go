package graph

import (
	"fmt"
	"regexp"
	"strings"
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
	date, _, err := parseSourceDate(value, "document_date")
	if err != nil {
		return "", err
	}
	if date.After(now().Add(24 * time.Hour)) {
		return "", argerr.New("document_date must be no more than 24 hours in the future")
	}
	return date.Format(timeLayout), nil
}

func parseSourceDate(value, field string) (time.Time, bool, error) {
	layout := time.DateOnly
	if documentTimestampFormat.MatchString(value) {
		layout = time.RFC3339
	}
	date, err := time.Parse(layout, value)
	if err != nil {
		return time.Time{}, false, argerr.Errorf("%s must be YYYY-MM-DD or an RFC 3339 timestamp with an offset", field)
	}
	date = date.UTC()
	if date.Year() < 1900 {
		return time.Time{}, false, argerr.Errorf("%s must be in 1900 or later (UTC)", field)
	}
	return date, layout == time.DateOnly, nil
}

// DateWindow is an inclusive interval over the effective document date.
// A zero value omits both bounds.
type DateWindow struct {
	since, until       time.Time
	hasSince, hasUntil bool
}

// ParseDateWindow parses optional UTC-normalized dates or RFC 3339 bounds.
// A date-only until includes its entire UTC day.
func ParseDateWindow(since, until string) (DateWindow, error) {
	w := DateWindow{hasSince: since != "", hasUntil: until != ""}
	var err error
	if w.hasSince {
		w.since, _, err = parseSourceDate(since, "since")
		if err != nil {
			return DateWindow{}, err
		}
	}
	if w.hasUntil {
		var dateOnly bool
		w.until, dateOnly, err = parseSourceDate(until, "until")
		if err != nil {
			return DateWindow{}, err
		}
		if dateOnly {
			w.until = w.until.Add(24*time.Hour - time.Nanosecond)
		}
	}
	if w.hasSince && w.hasUntil && w.since.After(w.until) {
		return DateWindow{}, argerr.New("since must be at or before until")
	}
	return w, nil
}

func (w DateWindow) dateFilters(node string, params map[string]any) []string {
	effective, _ := documentDateCypher(node)
	var filters []string
	if w.hasSince {
		params["since"] = dateWindowKey(w.since)
		filters = append(filters, effective+" >= $since")
	}
	if w.hasUntil {
		params["until"] = dateWindowKey(w.until)
		filters = append(filters, effective+" <= $until")
	}
	return filters
}

func dateWindowKey(date time.Time) string {
	if date.Year() > 9999 {
		// An offset can roll into UTC year 10000. Use a key that sorts
		// after every fixed-width stored timestamp.
		return "9999-12-31T23:59:59.999999999Z~"
	}
	return date.Format(timeLayout)
}

// evidenceJoin filters evidence by its owning Document within the evidence read's
// snapshot. The empty window keeps the existing query text byte-for-byte.
func (w DateWindow) evidenceJoin(evidence string, params map[string]any) string {
	filters := w.dateFilters("wd", params)
	if len(filters) == 0 {
		return ""
	}
	return "\nMATCH (wd:Document {id: " + evidence + ".doc_id})\nWHERE " + strings.Join(filters, " AND ")
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
