package orchestrator

import (
	"context"
	"errors"
	"fmt"

	"github.com/cynkra/blockyard/internal/db"
	"github.com/cynkra/blockyard/internal/task"
)

// Rollback restores the previous version using backup metadata.
//
//  1. Read latest backup metadata
//  2. Check for irreversible migrations
//  3. Variant-specific prep (Docker: pull old image; process: returns 501 upstream)
//  4. Run down migrations to the recorded version
//  5. Create old instance (passive mode)
//  6. Poll /readyz on old instance
//  7. Drain current server
//  8. Activate old instance
//
// Steps 1–3 are side-effect-free. Step 4 (down-migration) is the
// point of no return: if any subsequent step fails, the running
// server's code no longer matches the database schema. Rather than
// serve broken requests, the server shuts itself down and logs the
// backup path for manual recovery.
//
// Rollback requires a factory that SupportsRollback(). The admin
// handler returns 501 for factories that don't.
func (o *Orchestrator) Rollback(
	ctx context.Context,
	sender task.Sender,
	shutdownFn func(),
) error {
	defer func() { o.activeInstance = nil }()

	if !o.factory.SupportsRollback() {
		return fmt.Errorf("rollback is not supported by the active backend")
	}

	// 1. Find backup metadata.
	dbPath := o.cfg.Database.Path
	if o.cfg.Database.Driver == "postgres" {
		dbPath = "." // pg backups written to cwd
	}
	meta, err := db.LatestBackupMeta(dbPath)
	if errors.Is(err, db.ErrNoBackup) {
		return fmt.Errorf("no backup found — cannot rollback. " +
			"Restore manually from the database backup directory")
	}
	if err != nil {
		return fmt.Errorf("read backup metadata: %w", err)
	}
	sender.Write(fmt.Sprintf("Rolling back to image %s (migration %d)",
		meta.ImageTag, meta.MigrationVersion))

	// 2. Check for irreversible migrations (fail fast before any
	//    side effects).
	currentVer, _, _ := o.db.MigrationVersion()
	if currentVer != meta.MigrationVersion {
		if err := o.db.CheckDownMigrationSafety(
			meta.MigrationVersion, currentVer); err != nil {
			return fmt.Errorf(
				"cannot rollback: %w. Restore manually from backup: %s",
				err, meta.BackupPath)
		}
	}

	// 3. Variant-specific prep: Docker pulls the old image.
	if err := o.factory.PreUpdate(ctx, meta.ImageTag, sender); err != nil {
		return fmt.Errorf("pull old image: %w", err)
	}
	oldRef := imageWithTag(o.factory.CurrentImageBase(ctx), meta.ImageTag)

	// 4. Run down migrations — point of no return.
	migrated := false
	if currentVer != meta.MigrationVersion {
		sender.Write(fmt.Sprintf(
			"Running down migrations: %d → %d ...",
			currentVer, meta.MigrationVersion))
		if err := o.db.MigrateDown(meta.MigrationVersion); err != nil {
			return fmt.Errorf(
				"down migration failed: %w. Restore manually from backup: %s",
				err, meta.BackupPath)
		}
		migrated = true
	}

	// fatal is called when a step after down-migration fails.
	// The running server's code no longer matches the schema —
	// shut down rather than serve broken requests.
	fatal := func(msg string) error {
		sender.Write("FATAL: " + msg)
		sender.Write(fmt.Sprintf(
			"Database is at version %d but server expects %d. "+
				"Restore from backup: %s",
			meta.MigrationVersion, currentVer, meta.BackupPath))
		shutdownFn()
		return fmt.Errorf("rollback failed after migration: %s", msg)
	}

	// 5-6. Create old instance and wait for it to become healthy.
	o.activationToken = generateActivationToken()
	startCtx, cancel := context.WithTimeout(ctx, o.cfg.Proxy.WorkerStartTimeout.Duration)
	defer cancel()
	inst, err := o.factory.CreateInstance(startCtx, oldRef, []string{
		"BLOCKYARD_ACTIVATION_TOKEN=" + o.activationToken,
	}, sender)
	if err != nil {
		if migrated {
			return fatal(fmt.Sprintf("start old container: %v", err))
		}
		return fmt.Errorf("start old container: %w", err)
	}
	o.activeInstance = inst

	if err := o.waitReady(startCtx, inst.Addr()); err != nil {
		inst.Kill(ctx)
		if migrated {
			return fatal(fmt.Sprintf(
				"old container never became ready: %v", err))
		}
		return fmt.Errorf("old container never became ready: %w", err)
	}

	// 7. Drain current server.
	o.drainFn()

	// 8. Activate old container.
	if err := o.activate(ctx, inst.Addr()); err != nil {
		inst.Kill(ctx)
		// Schema is wrong — cannot undrain and resume.
		if migrated {
			return fatal(fmt.Sprintf("activate old container: %v", err))
		}
		o.undrainFn()
		return fmt.Errorf("activate old container: %w", err)
	}

	sender.Write("Rollback complete.")
	return nil
}
