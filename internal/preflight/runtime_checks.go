package preflight

import (
	"context"
	"fmt"
	"time"

	"golang.org/x/sys/unix"
)

const runtimeCheckTimeout = 5 * time.Second

// runDynamicChecks executes all re-runnable checks.
func runDynamicChecks(ctx context.Context, deps RuntimeDeps) *Report {
	r := &Report{RanAt: time.Now().UTC()}

	// Health probes (same subsystems as readyz).
	r.Add(checkDatabase(ctx, deps))
	r.Add(checkBackend(ctx, deps))
	if deps.RedisPing != nil {
		r.Add(checkRedis(ctx, deps))
	}
	if deps.IDPCheck != nil {
		r.Add(checkIDP(ctx, deps))
	}
	if deps.VaultCheck != nil {
		r.Add(checkVault(ctx, deps))
	}
	if deps.VaultTokenOK != nil {
		r.Add(checkVaultToken(deps))
	}

	// Runtime checks.
	if deps.StorePath != "" {
		r.Add(checkDiskSpace(deps.StorePath))
	}
	if deps.UpdateAvailable != nil {
		r.Add(checkUpdateAvailable(deps))
	}

	return r
}

// --- Health probes ---

func checkDatabase(ctx context.Context, deps RuntimeDeps) Result {
	const name = "database"
	const category = "runtime"

	if deps.DBPing == nil {
		return Result{Name: name, Severity: SeverityOK, Message: "database check not available", Category: category}
	}

	ctx, cancel := context.WithTimeout(ctx, runtimeCheckTimeout)
	defer cancel()

	if err := deps.DBPing(ctx); err != nil {
		return Result{
			Name:     name,
			Severity: SeverityError,
			Message:  fmt.Sprintf("database ping failed: %v", err),
			Category: category,
		}
	}
	return Result{
		Name:     name,
		Severity: SeverityOK,
		Message:  "database is reachable",
		Category: category,
	}
}

func checkBackend(ctx context.Context, deps RuntimeDeps) Result {
	const name = "backend"
	const category = "runtime"

	if deps.BackendPing == nil {
		return Result{Name: name, Severity: SeverityOK, Message: "backend check not available", Category: category}
	}

	ctx, cancel := context.WithTimeout(ctx, runtimeCheckTimeout)
	defer cancel()

	if err := deps.BackendPing(ctx); err != nil {
		return Result{
			Name:     name,
			Severity: SeverityError,
			Message:  fmt.Sprintf("backend unreachable: %v", err),
			Category: category,
		}
	}
	return Result{
		Name:     name,
		Severity: SeverityOK,
		Message:  "backend is responsive",
		Category: category,
	}
}

func checkRedis(ctx context.Context, deps RuntimeDeps) Result {
	const name = "redis"
	const category = "runtime"

	ctx, cancel := context.WithTimeout(ctx, runtimeCheckTimeout)
	defer cancel()

	if err := deps.RedisPing(ctx); err != nil {
		return Result{
			Name:     name,
			Severity: SeverityError,
			Message:  fmt.Sprintf("Redis ping failed: %v", err),
			Category: category,
		}
	}
	return Result{
		Name:     name,
		Severity: SeverityOK,
		Message:  "Redis is reachable",
		Category: category,
	}
}

func checkIDP(ctx context.Context, deps RuntimeDeps) Result {
	const name = "idp"
	const category = "runtime"

	ctx, cancel := context.WithTimeout(ctx, runtimeCheckTimeout)
	defer cancel()

	if err := deps.IDPCheck(ctx); err != nil {
		return Result{
			Name:     name,
			Severity: SeverityError,
			Message:  fmt.Sprintf("IdP discovery endpoint unreachable: %v", err),
			Category: category,
		}
	}
	return Result{
		Name:     name,
		Severity: SeverityOK,
		Message:  "IdP discovery endpoint is reachable",
		Category: category,
	}
}

func checkVault(ctx context.Context, deps RuntimeDeps) Result {
	const name = "openbao"
	const category = "runtime"

	ctx, cancel := context.WithTimeout(ctx, runtimeCheckTimeout)
	defer cancel()

	if err := deps.VaultCheck(ctx); err != nil {
		return Result{
			Name:     name,
			Severity: SeverityError,
			Message:  fmt.Sprintf("OpenBao health check failed: %v", err),
			Category: category,
		}
	}
	return Result{
		Name:     name,
		Severity: SeverityOK,
		Message:  "OpenBao is healthy",
		Category: category,
	}
}

func checkVaultToken(deps RuntimeDeps) Result {
	const name = "vault_token"
	const category = "runtime"

	if deps.VaultTokenOK() {
		return Result{
			Name:     name,
			Severity: SeverityOK,
			Message:  "vault token is valid",
			Category: category,
		}
	}
	return Result{
		Name:     name,
		Severity: SeverityError,
		Message:  "vault token renewal has failed; secrets operations may be broken",
		Category: category,
	}
}

// --- Runtime checks ---

func checkDiskSpace(storePath string) Result {
	const name = "disk_space"
	const category = "runtime"

	var stat unix.Statfs_t
	if err := unix.Statfs(storePath, &stat); err != nil {
		return Result{
			Name:     name,
			Severity: SeverityError,
			Message:  fmt.Sprintf("could not stat pkg-store volume: %v", err),
			Category: category,
		}
	}

	bsize := stat.Bsize
	if bsize <= 0 {
		bsize = 1
	}
	total := stat.Blocks * uint64(bsize) //nolint:gosec // G115: Bsize is always positive on real filesystems
	avail := stat.Bavail * uint64(bsize) //nolint:gosec // G115: Bsize is always positive on real filesystems

	if total == 0 {
		return Result{
			Name:     name,
			Severity: SeverityOK,
			Message:  "could not determine disk capacity",
			Category: category,
		}
	}

	pctFree := float64(avail) / float64(total) * 100

	switch {
	case pctFree < 3:
		return Result{
			Name:     name,
			Severity: SeverityError,
			Message:  fmt.Sprintf("pkg-store volume has %.1f%% free space (critical)", pctFree),
			Category: category,
		}
	case pctFree < 10:
		return Result{
			Name:     name,
			Severity: SeverityWarning,
			Message:  fmt.Sprintf("pkg-store volume has %.1f%% free space", pctFree),
			Category: category,
		}
	default:
		return Result{
			Name:     name,
			Severity: SeverityOK,
			Message:  fmt.Sprintf("pkg-store volume has %.1f%% free space", pctFree),
			Category: category,
		}
	}
}

func checkUpdateAvailable(deps RuntimeDeps) Result {
	const name = "update_available"
	const category = "runtime"

	latest := deps.UpdateAvailable()
	if latest == "" {
		return Result{
			Name:     name,
			Severity: SeverityOK,
			Message:  "running " + deps.ServerVersion,
			Category: category,
		}
	}

	return Result{
		Name:     name,
		Severity: SeverityInfo,
		Message:  fmt.Sprintf("running %s; version %s is available", deps.ServerVersion, latest),
		Category: category,
	}
}
