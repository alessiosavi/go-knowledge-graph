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

// TestUISitsBehindBearerAuth pins that the UI obeys the same bearer policy
// as the API (mirrors TestAuthAppliesToMountedHandlers).
func TestUISitsBehindBearerAuth(t *testing.T) {
	cfg := config.Config{}
	cfg.Server.APIKey = "sekret"
	h := NewServer(cfg, nil, nil).Handler()

	for _, path := range []string{"/ui/", "/ui/app.js"} {
		rr := do(t, h, http.MethodGet, path, nil, nil)
		if rr.Code != http.StatusUnauthorized {
			t.Errorf("GET %s without token: code = %d, want 401", path, rr.Code)
		}
		rr = do(t, h, http.MethodGet, path, nil, map[string]string{"Authorization": "Bearer sekret"})
		if rr.Code != http.StatusOK {
			t.Errorf("GET %s with token: code = %d, want 200", path, rr.Code)
		}
	}
}
