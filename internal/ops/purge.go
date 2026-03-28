package ops

import (
	"log/slog"
	"os"
	"path/filepath"

	"github.com/cynkra/blockyard/internal/bundle"
	"github.com/cynkra/blockyard/internal/db"
	"github.com/cynkra/blockyard/internal/server"
)

// PurgeApp permanently removes an app's bundles, files, and database
// rows. The app must already have no running workers. Used by both
// the DeleteApp handler (immediate delete) and the sweeper.
func PurgeApp(srv *server.Server, app *db.AppRow) {
	bundles, err := srv.DB.ListBundlesByApp(app.ID)
	if err != nil {
		slog.Warn("purge: list bundles failed",
			"app_id", app.ID, "error", err)
	}

	for _, b := range bundles {
		paths := bundle.NewBundlePaths(srv.Config.Storage.BundleServerPath, app.ID, b.ID)
		bundle.DeleteFiles(paths)
	}

	// Delete all DB rows atomically (sessions, tags, access, bundles, app).
	if err := srv.DB.PurgeApp(app.ID); err != nil {
		slog.Warn("purge: delete app data failed",
			"app_id", app.ID, "error", err)
	}

	appDir := filepath.Join(srv.Config.Storage.BundleServerPath, app.ID)
	if err := os.RemoveAll(appDir); err != nil {
		slog.Warn("purge: remove app directory failed",
			"app_id", app.ID, "path", appDir, "error", err)
	}

	slog.Info("purged app", "app_id", app.ID, "name", app.Name)
}
