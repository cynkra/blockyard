package server

import (
	"encoding/json"
	"path/filepath"

	"github.com/cynkra/blockyard/internal/backend"
	"github.com/cynkra/blockyard/internal/db"
	"github.com/cynkra/blockyard/internal/manifest"
)

// WorkerEnv builds the environment variable map for worker containers.
// Always sets BLOCKYARD_API_URL (needed for runtime package installs).
// Includes Vault/OpenBao integration vars when configured.
// Sets SHINY_HOST per backend so bundles don't have to.
func WorkerEnv(srv *Server) map[string]string {
	shinyHost := "0.0.0.0"
	if srv.Config.Server.Backend == "process" {
		shinyHost = "127.0.0.1"
	}

	env := map[string]string{
		"BLOCKYARD_API_URL": srv.InternalAPIURL(),
		"SHINY_HOST":        shinyHost,
	}

	if srv.Config.Openbao != nil {
		env["VAULT_ADDR"] = srv.Config.Openbao.Address
		if len(srv.Config.Openbao.Services) > 0 {
			svcMap := make(map[string]string, len(srv.Config.Openbao.Services))
			for _, svc := range srv.Config.Openbao.Services {
				svcMap[svc.ID] = "apikeys/" + svc.ID
			}
			svcJSON, _ := json.Marshal(svcMap)
			env["BLOCKYARD_VAULT_SERVICES"] = string(svcJSON)
		}
	}

	// Board storage: inject PostgREST URL so R apps can discover it.
	if srv.Config.BoardStorage != nil && srv.Config.BoardStorage.PostgrestURL != "" {
		env["POSTGREST_URL"] = srv.Config.BoardStorage.PostgrestURL
	}

	return env
}

// BaseWorkerSpec returns a WorkerSpec with all fields that are common
// across spawn sites (coldstart, transfer, API scale-up). Callers
// fill in site-specific fields like LibDir, TransferDir, TokenDir,
// MemoryLimit, and CPULimit.
func BaseWorkerSpec(srv *Server, app *db.AppRow, workerID, bundleID string) backend.WorkerSpec {
	hostPaths := srv.BundlePaths(app.ID, bundleID)
	rProfile, _ := EnsureRProfile(srv.Config.Storage.BundleServerPath)

	var rVersion string
	if m, err := manifest.Read(filepath.Join(hostPaths.Unpacked, "manifest.json")); err == nil {
		rVersion = m.RVersion
	}

	return backend.WorkerSpec{
		AppID:        app.ID,
		WorkerID:     workerID,
		Image:        AppImage(app, srv.Config.Docker.Image),
		Cmd:          []string{"Rscript", filepath.Join(srv.Config.Storage.BundleWorkerPath, "app.R")},
		BundlePath:   hostPaths.Unpacked,
		LibraryPath:  hostPaths.Library,
		WorkerMount:  srv.Config.Storage.BundleWorkerPath,
		ShinyPort:    srv.Config.Docker.ShinyPort,
		RVersion:     rVersion,
		RProfilePath: rProfile,
		Labels: map[string]string{
			"dev.blockyard/managed":   "true",
			"dev.blockyard/app-id":    app.ID,
			"dev.blockyard/worker-id": workerID,
			"dev.blockyard/role":      "worker",
		},
		Env:     WorkerEnv(srv),
		Runtime: AppRuntime(app, srv.Config.Docker),
	}
}
