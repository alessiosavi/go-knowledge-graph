package webui

import (
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	"golang.org/x/net/html"
)

func TestPresetPickerMatchesShippedPresets(t *testing.T) {
	rr := get(t, "/")
	doc, err := html.Parse(strings.NewReader(rr.Body.String()))
	if err != nil {
		t.Fatal(err)
	}
	attr := func(n *html.Node, key string) string {
		for _, a := range n.Attr {
			if a.Key == key {
				return a.Val
			}
		}
		return ""
	}
	var picker *html.Node
	var foundInput bool
	var walk func(*html.Node)
	walk = func(n *html.Node) {
		if n.Type == html.ElementNode {
			switch attr(n, "id") {
			case "ingest-labels":
				foundInput = true
				if n.Data != "input" || attr(n, "type") != "text" || attr(n, "list") != "ingest-presets" {
					t.Error("labels must remain a text input linked to ingest-presets")
				}
				if got := attr(n, "placeholder"); got != "preset (default: general)" {
					t.Errorf("placeholder = %q", got)
				}
			case "ingest-presets":
				if n.Data == "datalist" {
					picker = n
				}
			}
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
	}
	walk(doc)
	if !foundInput {
		t.Error("ingest-labels input missing")
	}
	if picker == nil {
		t.Fatal("ingest-presets datalist missing")
	}
	var got []string
	for c := picker.FirstChild; c != nil; c = c.NextSibling {
		if c.Type == html.ElementNode && c.Data == "option" {
			got = append(got, attr(c, "value"))
		}
	}
	if len(got) == 0 || got[0] != "general" {
		t.Fatalf("picker options = %v, want general first", got)
	}
	files, err := filepath.Glob("../../labels/*.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var want []string
	for _, file := range files {
		want = append(want, strings.TrimSuffix(filepath.Base(file), ".yaml"))
	}
	slices.Sort(got)
	if !slices.Equal(got, want) {
		t.Errorf("picker options = %v, shipped presets = %v", got, want)
	}
}

func TestUIAuthJS(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		if os.Getenv("GKG_REQUIRE_NODE") == "1" {
			t.Fatal("node is required by GKG_REQUIRE_NODE=1")
		}
		t.Skip("node is not installed")
	}
	// Go runs package tests from the package directory, alongside jstest/.
	cmd := exec.Command(node, "--test", "jstest/")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("UI auth JavaScript tests: %v\n%s", err, out)
	}
}

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
	if body := rr.Body.String(); !strings.Contains(body, `id="ingest-file"`) {
		t.Errorf("GET /: the body misses the file-upload input marker")
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
