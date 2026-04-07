package server

import (
	"github.com/cynkra/blockyard/internal/config"
	"github.com/cynkra/blockyard/internal/db"
)

// AppImage returns the per-app image override, or the server-wide default.
func AppImage(app *db.AppRow, serverDefault string) string {
	if app.Image != "" {
		return app.Image
	}
	return serverDefault
}

// AppRuntime returns the effective OCI runtime for an app.
// Fallback chain: app.Runtime → config.RuntimeDefaults[accessType] → config.Runtime.
func AppRuntime(app *db.AppRow, cfg config.DockerConfig) string {
	if app.Runtime != "" {
		return app.Runtime
	}
	if rt, ok := cfg.RuntimeDefaults[app.AccessType]; ok && rt != "" {
		return rt
	}
	return cfg.Runtime
}
