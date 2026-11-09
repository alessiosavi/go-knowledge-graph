package webui

import (
	"io/fs"
	"net/http"
	"net/http/httptest"
	"regexp"
	"slices"
	"strings"
	"testing"
)

// get performs one GET against the embedded UI handler, as mounted under
// the /ui/ prefix (internal/api strips the prefix before it gets here).
func get(t *testing.T, target string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, target, nil)
	rr := httptest.NewRecorder()
	Handler().ServeHTTP(rr, req)
	return rr
}

// TestHandlerServesIndex pins that the root path serves the embedded HTML.
func TestHandlerServesIndex(t *testing.T) {
	rr := get(t, "/")
	if rr.Code != http.StatusOK {
		t.Fatalf("GET /: code = %d, want 200", rr.Code)
	}
	if ct := rr.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
		t.Errorf("GET /: Content-Type = %q, want text/html", ct)
	}
	if body := rr.Body.String(); !strings.Contains(body, `id="graph"`) {
		t.Errorf("GET /: the body misses the graph canvas marker")
	}
	if body := rr.Body.String(); !strings.Contains(body, `id="reader"`) {
		t.Errorf("GET /: the body misses the reader panel marker")
	}
}

// TestHandlerServesAssets pins the content types of the static assets.
func TestHandlerServesAssets(t *testing.T) {
	cases := []struct{ path, ctype string }{
		{"/app.css", "text/css"},
		{"/app.js", "text/javascript"},
	}
	for _, tc := range cases {
		rr := get(t, tc.path)
		if rr.Code != http.StatusOK {
			t.Errorf("GET %s: code = %d, want 200", tc.path, rr.Code)
			continue
		}
		if ct := rr.Header().Get("Content-Type"); !strings.HasPrefix(ct, tc.ctype) {
			t.Errorf("GET %s: Content-Type = %q, want %s", tc.path, ct, tc.ctype)
		}
	}
}

// TestHandlerUnknownAssetNotFound pins the 404 for paths that are not part
// of the embedded UI.
func TestHandlerUnknownAssetNotFound(t *testing.T) {
	rr := get(t, "/nope.js")
	if rr.Code != http.StatusNotFound {
		t.Errorf("GET /nope.js: code = %d, want 404", rr.Code)
	}
}

// TestIndexReferencesOnlyLocalAssets pins the offline guarantee: the served
// page must not reference any external URL through src/href attributes.
func TestIndexReferencesOnlyLocalAssets(t *testing.T) {
	rr := get(t, "/")
	if rr.Code != http.StatusOK {
		t.Fatalf("GET /: code = %d, want 200", rr.Code)
	}
	external := regexp.MustCompile(`(?i)(?:src|href)\s*=\s*["'](?:(?:https?:)?//)`)
	if m := external.FindString(rr.Body.String()); m != "" {
		t.Errorf("the served index references an external URL: %q", m)
	}
}

// TestAssetsFileSet pins the shipped file set: every embedded file is
// served without the bearer key (internal/api authExempt lists these same
// paths), so adding an asset means updating that list too.
func TestAssetsFileSet(t *testing.T) {
	entries, err := fs.ReadDir(assets, "assets")
	if err != nil {
		t.Fatalf("read embedded assets: %v", err)
	}
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	want := []string{"app.css", "app.js", "index.html"}
	if !slices.Equal(names, want) {
		t.Errorf("embedded file set = %v, want %v (update internal/api authExempt when adding files)", names, want)
	}
}
