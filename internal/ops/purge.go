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

	if err := srv.DB.ClearActiveBundle(app.ID); err != nil {
		slog.Warn("purge: clear active bundle failed",
			"app_id", app.ID, "error", err)
	}

	for _, b := range bundles {
		if _, err := srv.DB.DeleteBundle(b.ID); err != nil {
			slog.Warn("purge: delete bundle row failed",
				"bundle_id", b.ID, "app_id", app.ID, "error", err)
		}
	}

	if err := srv.DB.HardDeleteApp(app.ID); err != nil {
		slog.Warn("purge: delete app row failed",
			"app_id", app.ID, "error", err)
	}

	appDir := filepath.Join(srv.Config.Storage.BundleServerPath, app.ID)
	if err := os.RemoveAll(appDir); err != nil {
		slog.Warn("purge: remove app directory failed",
			"app_id", app.ID, "path", appDir, "error", err)
	}

	slog.Info("purged app", "app_id", app.ID, "name", app.Name)
}
