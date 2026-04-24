package server

import (
	"encoding/json"
	"path/filepath"

	"github.com/cynkra/blockyard/internal/backend"
	"github.com/cynkra/blockyard/internal/db"
	"github.com/cynkra/blockyard/internal/manifest"
)

// WorkerEnv builds the backend-agnostic environment variable map for
// worker containers. Always sets BLOCKYARD_API_URL (needed for runtime
// package installs). Includes Vault/OpenBao integration vars when
// configured. Sets SHINY_HOST per backend so bundles don't have to.
// Values from server.worker_env are merged in last; blockyard-managed
// keys win on collision, everything else (e.g. OTEL_*) is passed through.
func WorkerEnv(srv *Server) map[string]string {
	env := make(map[string]string)
	for k, v := range srv.Config.Server.WorkerEnv {
		env[k] = v
	}

	shinyHost := "0.0.0.0"
	if srv.Config.Server.Backend == "process" {
		shinyHost = "127.0.0.1"
	}
	env["BLOCKYARD_API_URL"] = srv.InternalAPIURL()
	env["SHINY_HOST"] = shinyHost

	if srv.Config.Vault != nil {
		env["VAULT_ADDR"] = srv.Config.Vault.Address
		if len(srv.Config.Vault.Services) > 0 {
			svcMap := make(map[string]string, len(srv.Config.Vault.Services))
			for _, svc := range srv.Config.Vault.Services {
				svcMap[svc.ID] = "apikeys/" + svc.ID
			}
			svcJSON, _ := json.Marshal(svcMap)
			env["BLOCKYARD_VAULT_SERVICES"] = string(svcJSON)
		}
	}

	// Board storage discovery (#284): R assembles
	//   {VAULT_ADDR}/v1/{BLOCKYARD_VAULT_DB_MOUNT}/static-creds/{role}
	// at runtime. Role is delivered per-session via the
	// X-Blockyard-Pg-Role header; mount is deployment-level so it
	// ships as env alongside VAULT_ADDR. Unset when the feature is
	// disabled so workers never pick up a stale value after a flip.
	if srv.Config.Database.BoardStorage {
		env["BLOCKYARD_VAULT_DB_MOUNT"] = srv.Config.Database.VaultMount
	}

	return env
}

// injectOTELIdentity adds per-app OpenTelemetry identity attributes
// so signals from different apps/workers are distinguishable in the
// backend. A no-op unless OTEL_EXPORTER_OTLP_ENDPOINT is already in
// env. A user-set OTEL_SERVICE_NAME wins; blockyard resource attrs
// are appended to any user-supplied OTEL_RESOURCE_ATTRIBUTES.
func injectOTELIdentity(env map[string]string, appName, workerID string) {
	if env["OTEL_EXPORTER_OTLP_ENDPOINT"] == "" {
		return
	}
	if _, set := env["OTEL_SERVICE_NAME"]; !set {
		env["OTEL_SERVICE_NAME"] = appName
	}
	blockyardAttrs := "blockyard.app=" + appName + ",blockyard.worker_id=" + workerID
	if existing := env["OTEL_RESOURCE_ATTRIBUTES"]; existing != "" {
		env["OTEL_RESOURCE_ATTRIBUTES"] = existing + "," + blockyardAttrs
	} else {
		env["OTEL_RESOURCE_ATTRIBUTES"] = blockyardAttrs
	}
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

	env := WorkerEnv(srv)
	injectOTELIdentity(env, app.Name, workerID)

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
		Env:     env,
		Runtime: AppRuntime(app, srv.Config.Docker),
	}
}
