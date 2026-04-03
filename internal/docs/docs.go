package docs

import (
	"embed"
	"io/fs"
	"net/http"
)

//go:embed all:dist
var content embed.FS

// Handler returns an http.Handler that serves the embedded documentation
// site (Starlight/Astro static build). The caller must strip the URL
// prefix before passing requests.
func Handler() http.Handler {
	sub, _ := fs.Sub(content, "dist")
	return http.FileServer(http.FS(sub))
}
