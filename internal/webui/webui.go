// Package webui serves gkg's embedded web UI: a dependency-free, read-only
// viewer over the existing REST endpoints (search, entity cards with
// evidence, documents, one-hop traversal) plus the explicit ingest box.
// The UI ships inside the binary and works fully offline.
package webui

import (
	"embed"
	"io/fs"
	"net/http"
)

// assets holds the static UI; the whole directory ships in the binary.
//
//go:embed assets
var assets embed.FS

// Handler serves the embedded UI rooted at "/", so the caller mounts it
// under a prefix (internal/api strips "/ui/"). The root path serves
// index.html; unknown assets answer 404. The returned handler is stateless
// and safe for concurrent use.
func Handler() http.Handler {
	sub, err := fs.Sub(assets, "assets")
	if err != nil {
		// The subtree is a compile-time constant: unreachable.
		panic(err)
	}
	return http.FileServer(http.FS(sub))
}
