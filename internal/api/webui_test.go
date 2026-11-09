package api

import (
	"net/http"
	"strings"
	"testing"

	"github.com/alessiosavi/go-knowledge-graph/internal/config"
)

// TestUIServesEmbeddedAssets pins the /ui/ mount: the index page, the
// static assets, and a 404 for unknown paths.
func TestUIServesEmbeddedAssets(t *testing.T) {
	h := apiHandler(t, config.Config{}, nil, nil)

	rr := do(t, h, http.MethodGet, "/ui/", nil, nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("GET /ui/: code = %d, want 200", rr.Code)
	}
	if ct := rr.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
		t.Errorf("GET /ui/: Content-Type = %q, want text/html", ct)
	}
	if !strings.Contains(rr.Body.String(), `id="graph"`) {
		t.Errorf("GET /ui/: the body misses the graph canvas marker")
	}

	rr = do(t, h, http.MethodGet, "/ui/app.js", nil, nil)
	if rr.Code != http.StatusOK {
		t.Errorf("GET /ui/app.js: code = %d, want 200", rr.Code)
	}

	rr = do(t, h, http.MethodGet, "/ui/nope.js", nil, nil)
	if rr.Code != http.StatusNotFound {
		t.Errorf("GET /ui/nope.js: code = %d, want 404", rr.Code)
	}
}

// TestUIRedirectsBarePath pins GET /ui → /ui/.
func TestUIRedirectsBarePath(t *testing.T) {
	h := apiHandler(t, config.Config{}, nil, nil)
	rr := do(t, h, http.MethodGet, "/ui", nil, nil)
	if rr.Code != http.StatusMovedPermanently {
		t.Fatalf("GET /ui: code = %d, want 301", rr.Code)
	}
	if loc := rr.Header().Get("Location"); loc != "/ui/" {
		t.Errorf("GET /ui: Location = %q, want /ui/", loc)
	}
}

// TestUIBearerPolicy pins the split the UI needs: the static shell is
// reachable without the key — a browser navigation cannot attach an
// Authorization header, and the page must load to ask for the key it then
// sends on its fetches — while every data path stays guarded.
func TestUIBearerPolicy(t *testing.T) {
	cfg := config.Config{}
	cfg.Server.APIKey = "sekret"
	h := NewServer(cfg, nil, nil).Handler()

	shell := []struct {
		path string
		want int
	}{
		{"/ui", http.StatusMovedPermanently},
		{"/ui/", http.StatusOK},
		{"/ui/index.html", http.StatusMovedPermanently},
		{"/ui/app.css", http.StatusOK},
		{"/ui/app.js", http.StatusOK},
	}
	for _, tc := range shell {
		rr := do(t, h, http.MethodGet, tc.path, nil, nil)
		if rr.Code != tc.want {
			t.Errorf("GET %s without a key: code = %d, want %d (the shell must load so the page can ask for the key)", tc.path, rr.Code, tc.want)
		}
	}

	// Anything else under /ui is not exempt.
	rr := do(t, h, http.MethodGet, "/ui/nope.js", nil, nil)
	if rr.Code != http.StatusUnauthorized {
		t.Errorf("GET /ui/nope.js without a key: code = %d, want 401", rr.Code)
	}
	rr = do(t, h, http.MethodGet, "/ui/nope.js", nil, map[string]string{"Authorization": "Bearer sekret"})
	if rr.Code != http.StatusNotFound {
		t.Errorf("GET /ui/nope.js with a key: code = %d, want 404", rr.Code)
	}

	// Data paths stay behind the key.
	rr = do(t, h, http.MethodGet, "/v1/documents", nil, nil)
	if rr.Code != http.StatusUnauthorized {
		t.Errorf("GET /v1/documents without a key: code = %d, want 401", rr.Code)
	}
	rr = do(t, h, http.MethodGet, "/v1/documents", nil, map[string]string{"Authorization": "Bearer sekret"})
	if rr.Code == http.StatusUnauthorized {
		t.Errorf("GET /v1/documents with a key: got 401, want the request served")
	}
}
