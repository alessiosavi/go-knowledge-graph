// Package apidocs serves the embedded, offline Swagger UI and canonical spec.
package apidocs

import (
	"bytes"
	"embed"
	"io/fs"
	"net/http"
	"time"

	"github.com/alessiosavi/go-knowledge-graph/docs"
)

// ContentSecurityPolicy limits documentation resources and requests to this origin.
const ContentSecurityPolicy = "default-src 'none'; script-src 'self'; style-src 'self' 'unsafe-inline'; img-src 'self' data:; connect-src 'self'; font-src 'self'; frame-ancestors 'none'; base-uri 'none'; form-action 'none'"

// Embed only runtime files; license and version records stay alongside them.
//
//go:embed assets/index.html assets/swagger-initializer.js assets/swagger-ui-bundle.js assets/swagger-ui.css
var assets embed.FS

// Handler serves paths rooted at /; strip /docs while retaining the leading
// slash when mounting it. Only the
// named runtime files are served. Directories and unknown paths return 404.
func Handler() http.Handler {
	sub, err := fs.Sub(assets, "assets")
	if err != nil {
		panic(err)
	}
	files := http.FileServer(http.FS(sub))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Security-Policy", ContentSecurityPolicy)
		w.Header().Set("X-Content-Type-Options", "nosniff")
		switch r.URL.Path {
		case "/openapi.yaml":
			w.Header().Set("Content-Type", "application/yaml")
			http.ServeContent(w, r, "openapi.yaml", time.Time{}, bytes.NewReader(docs.OpenAPI))
		case "/", "/index.html", "/swagger-initializer.js", "/swagger-ui-bundle.js", "/swagger-ui.css":
			files.ServeHTTP(w, r)
		default:
			http.NotFound(w, r)
		}
	})
}
