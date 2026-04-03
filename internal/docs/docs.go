package docs

import (
	"embed"
	"io/fs"
	"net/http"
	"strings"
)

//go:embed all:dist
var content embed.FS

// Handler returns an http.Handler that serves the embedded documentation
// site. The caller must strip the URL prefix before passing requests.
//
// For SPA-style routing (Starlight), requests for paths that don't match
// a static file are served the root index.html so client-side routing
// can handle them.
func Handler() http.Handler {
	sub, _ := fs.Sub(content, "dist")
	fileServer := http.FileServer(http.FS(sub))

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Serve static assets directly.
		path := strings.TrimPrefix(r.URL.Path, "/")
		if path == "" || path == "/" {
			fileServer.ServeHTTP(w, r)
			return
		}

		// Try the exact path first. If not found, try path/index.html
		// (Starlight generates e.g. getting-started/index.html).
		if _, err := fs.Stat(sub, path); err == nil {
			fileServer.ServeHTTP(w, r)
			return
		}

		// Try directory index: path/index.html
		indexPath := strings.TrimSuffix(path, "/") + "/index.html"
		if _, err := fs.Stat(sub, indexPath); err == nil {
			fileServer.ServeHTTP(w, r)
			return
		}

		// Fallback: serve root index.html for SPA routing.
		r.URL.Path = "/"
		fileServer.ServeHTTP(w, r)
	})
}
