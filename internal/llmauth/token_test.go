package llmauth

import (
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"testing/iotest"
)

func TestAttemptRedactorAuthorizationRepresentations(t *testing.T) {
	const token = "fake-token+/="
	const auth = "Bearer " + token
	replace := Redactor(auth)
	for _, input := range []string{
		token,
		"Authorization: " + auth,
		`"Authorization":"` + auth + `"`,
		url.QueryEscape(auth),
		`Authorization: Bearer fake-token+\/=`,
		`Authorization: Bearer fake-token+\u002f=`,
		`Authorization: Bearer fake-token+\u002F=`,
	} {
		if got := replace(input); got != "[redacted]" {
			t.Errorf("replace(%q) = %q", input, got)
		}
	}
}

func TestVertexStatusKeepsPermanentClassWhenBodyIsIncomplete(t *testing.T) {
	for _, tc := range []struct {
		name string
		body io.Reader
	}{
		{"read error", io.MultiReader(strings.NewReader("Bearer fake-partial-"), iotest.ErrReader(io.ErrUnexpectedEOF))},
		{"read limit", strings.NewReader(strings.Repeat("x", 64<<10))},
	} {
		t.Run(tc.name, func(t *testing.T) {
			resp := &http.Response{StatusCode: http.StatusForbidden, Body: io.NopCloser(tc.body)}
			err := VertexStatus(resp, func(string) string {
				t.Error("incomplete body used as a preview")
				return "unexpected preview"
			})
			if Class(err) != PermanentClass || err.Error() != "auth_permanent: Vertex rejected the credential (HTTP 403)" {
				t.Fatalf("error = %v, want permanent auth error with only the HTTP status", err)
			}
		})
	}
}
