package db

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"time"
)

// Backup creates a point-in-time backup of the database.
//
// SQLite: VACUUM INTO to {path}.backup.{timestamp} — an atomic,
// consistent snapshot safe for live databases under concurrent access.
// PostgreSQL: pg_dump --format=custom to {dbname}.backup.{timestamp}.
//
// Returns the path to the backup file.
func (db *DB) Backup(ctx context.Context) (string, error) {
	switch db.dialect {
	case DialectSQLite:
		return db.backupSQLite(ctx)
	case DialectPostgres:
		return db.backupPostgres(ctx)
	default:
		return "", fmt.Errorf("backup: unsupported dialect")
	}
}

func (db *DB) backupSQLite(ctx context.Context) (string, error) {
	if db.tempPath != "" {
		return "", fmt.Errorf("backup: cannot back up in-memory database")
	}

	var seq int
	var name, path string
	if err := db.QueryRowContext(ctx,
		"PRAGMA database_list").Scan(&seq, &name, &path); err != nil {
		return "", fmt.Errorf("backup: resolve database path: %w", err)
	}

	ts := time.Now().UTC().Format("20060102T150405Z")
	dest := path + ".backup." + ts

	// VACUUM INTO creates an atomic, consistent snapshot of the database.
	// Unlike a raw file copy, it is safe while the server is concurrently
	// reading and writing — SQLite handles the locking internally.
	if _, err := db.ExecContext(ctx, "VACUUM INTO ?", dest); err != nil {
		return "", fmt.Errorf("backup: vacuum into: %w", err)
	}

	return dest, nil
}

// BackupMeta records the state at the time of backup so rollback
// knows what to restore.
type BackupMeta struct {
	BackupPath       string `json:"backup_path"`
	ImageTag         string `json:"image_tag"`
	MigrationVersion uint   `json:"migration_version"`
	CreatedAt        string `json:"created_at"`
}

// ErrNoBackup is returned when no backup metadata file is found.
var ErrNoBackup = errors.New("no backup metadata found")

// BackupWithMeta creates a database backup and writes a metadata
// sidecar. Returns the metadata on success.
func (db *DB) BackupWithMeta(ctx context.Context, imageTag string) (*BackupMeta, error) {
	backupPath, err := db.Backup(ctx)
	if err != nil {
		return nil, err
	}

	ver, _, err := db.MigrationVersion()
	if err != nil {
		os.Remove(backupPath)
		return nil, fmt.Errorf("backup: read migration version: %w", err)
	}

	meta := &BackupMeta{
		BackupPath:       backupPath,
		ImageTag:         imageTag,
		MigrationVersion: ver,
		CreatedAt:        time.Now().UTC().Format(time.RFC3339),
	}

	metaPath := backupPath + ".meta.json"
	data, _ := json.MarshalIndent(meta, "", "  ")
	if err := os.WriteFile(metaPath, data, 0o600); err != nil {
		os.Remove(backupPath)
		return nil, fmt.Errorf("backup: write metadata: %w", err)
	}

	return meta, nil
}

// LatestBackupMeta finds the most recent backup metadata file in the
// database directory. Returns ErrNoBackup if none exists.
func LatestBackupMeta(dbPath string) (*BackupMeta, error) {
	dir := filepath.Dir(dbPath)
	pattern := filepath.Join(dir, "*.meta.json")
	matches, _ := filepath.Glob(pattern)
	if len(matches) == 0 {
		return nil, ErrNoBackup
	}
	sort.Strings(matches) // timestamp in filename → lexicographic = chronological
	return readBackupMeta(matches[len(matches)-1])
}

func readBackupMeta(path string) (*BackupMeta, error) {
	data, err := os.ReadFile(path) //nolint:gosec // G304: reading our own backup metadata file
	if err != nil {
		return nil, fmt.Errorf("read backup metadata: %w", err)
	}
	var meta BackupMeta
	if err := json.Unmarshal(data, &meta); err != nil {
		return nil, fmt.Errorf("parse backup metadata: %w", err)
	}
	return &meta, nil
}

func (db *DB) backupPostgres(ctx context.Context) (string, error) {
	var dbname string
	if err := db.QueryRowContext(ctx,
		"SELECT current_database()").Scan(&dbname); err != nil {
		return "", fmt.Errorf("backup: resolve database name: %w", err)
	}

	ts := time.Now().UTC().Format("20060102T150405Z")
	dest := filepath.Join(".", dbname+".backup."+ts)

	// Use the stored connection URL so pg_dump inherits the full DSN
	// including credentials. The connURL field is set in openPostgres().
	cmd := exec.CommandContext(ctx, "pg_dump", //nolint:gosec // connURL is from our own config, not user input
		"--format=custom", "--dbname="+db.connURL, "-f", dest)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		os.Remove(dest)
		return "", fmt.Errorf("backup: pg_dump: %w: %s", err, stderr.String())
	}

	return dest, nil
}
