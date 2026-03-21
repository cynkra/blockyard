package proxy

import (
	_ "embed"
	"html/template"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/cynkra/blockyard/internal/db"
	"github.com/cynkra/blockyard/internal/server"
)

//go:embed loading.html
var loadingHTML string

var loadingTmpl = template.Must(template.New("loading").Parse(loadingHTML))

type loadingData struct {
	AppName   string
	ReadyURL  template.JSStr
	AppURL    template.JSStr
	TimeoutMs int64
}

// serveLoadingPage renders the cold-start loading page for browser
// requests when no healthy worker is available.
func serveLoadingPage(w http.ResponseWriter, app *db.AppRow, appName string, srv *server.Server) {
	appPath := "/app/" + appName + "/"
	readyPath := appPath + "__blockyard/ready"
	timeout := srv.Config.Proxy.WorkerStartTimeout.Duration

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusOK)

	// Add 10s buffer to the client-side timeout so the loading page
	// doesn't race the server-side spawn.
	clientTimeout := timeout + 10*time.Second

	if err := loadingTmpl.Execute(w, loadingData{
		AppName:   displayName(app),
		ReadyURL:  template.JSStr(readyPath),
		AppURL:    template.JSStr(appPath),
		TimeoutMs: clientTimeout.Milliseconds(),
	}); err != nil {
		slog.Warn("loading page: template execute failed", "error", err)
	}
}

// displayName returns the app's title if set, otherwise its name.
func displayName(app *db.AppRow) string {
	if app.Title != nil && *app.Title != "" {
		return *app.Title
	}
	return app.Name
}

// isBrowserRequest returns true if the request's Accept header
// indicates a browser expecting HTML content.
func isBrowserRequest(r *http.Request) bool {
	accept := r.Header.Get("Accept")
	return strings.Contains(accept, "text/html")
}

