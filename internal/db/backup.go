package db

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
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
	cmd := exec.CommandContext(ctx, "pg_dump",
		"--format=custom", "--dbname="+db.connURL, "-f", dest)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		os.Remove(dest)
		return "", fmt.Errorf("backup: pg_dump: %w: %s", err, stderr.String())
	}

	return dest, nil
}
