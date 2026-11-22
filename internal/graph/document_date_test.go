package graph

import (
	"errors"
	"testing"
	"time"

	"github.com/alessiosavi/go-knowledge-graph/internal/argerr"
)

func TestParseDocumentDate(t *testing.T) {
	now := func() time.Time { return time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC) }
	for _, tc := range []struct {
		name, input, want string
	}{
		{name: "omitted"},
		{"date_only", "2024-03-01", "2024-03-01T00:00:00.000000000Z"},
		{"offset_to_utc", "2024-03-01T00:30:00.123+01:00", "2024-02-29T23:30:00.123000000Z"},
		{name: "invalid_date", input: "2024-02-30"},
		{name: "missing_offset", input: "2024-03-01T11:30:00"},
		{name: "invalid_offset", input: "2024-03-01T11:30:00+24:00"},
		{name: "invalid_offset_minutes", input: "2024-03-01T11:30:00+00:60"},
		{name: "invalid_rfc3339_hour", input: "2024-03-01T1:30:00Z"},
		{name: "invalid_rfc3339_fraction", input: "2024-03-01T11:30:00,1Z"},
		{name: "before_1900", input: "1899-12-31"},
		{"year_1900", "1900-01-01", "1900-01-01T00:00:00.000000000Z"},
		{"at_future_limit", "2026-03-02T13:00:00+01:00", "2026-03-02T12:00:00.000000000Z"},
		{name: "past_future_limit", input: "2026-03-02T12:00:00.000000001Z"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ParseDocumentDate(tc.input, now)
			if tc.want == "" && tc.input != "" {
				var invalid argerr.Error
				if !errors.As(err, &invalid) || got != "" {
					t.Fatalf("ParseDocumentDate(%q) = %q, %v, want argerr", tc.input, got, err)
				}
				return
			}
			if err != nil || got != tc.want {
				t.Fatalf("ParseDocumentDate(%q) = %q, %v, want %q", tc.input, got, err, tc.want)
			}
		})
	}
}
