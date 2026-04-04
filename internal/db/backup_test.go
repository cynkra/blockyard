package db

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cynkra/blockyard/internal/config"
)

func TestBackupSQLite(t *testing.T) {
	// Create a real file-backed database (not :memory:).
	dir := t.TempDir()
	path := filepath.Join(dir, "test.db")
	db, err := Open(config.DatabaseConfig{Driver: "sqlite", Path: path})
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	// Insert some data so backup is non-trivial.
	_, err = db.CreateApp("test-app", "admin")
	if err != nil {
		t.Fatal(err)
	}

	dest, err := db.Backup(context.Background())
	if err != nil {
		t.Fatal(err)
	}

	// Verify backup file exists and is non-empty.
	info, err := os.Stat(dest)
	if err != nil {
		t.Fatalf("backup file not found: %v", err)
	}
	if info.Size() == 0 {
		t.Error("backup file is empty")
	}

	// Verify backup is a valid SQLite database.
	backupDB, err := Open(config.DatabaseConfig{Driver: "sqlite", Path: dest})
	if err != nil {
		t.Fatalf("cannot open backup: %v", err)
	}
	defer backupDB.Close()

	app, err := backupDB.GetAppByName("test-app")
	if err != nil || app == nil {
		t.Error("backup does not contain expected data")
	}
}

func TestBackupSQLiteMemoryFails(t *testing.T) {
	db := testDB(t) // in-memory
	_, err := db.Backup(context.Background())
	if err == nil {
		t.Error("expected error backing up in-memory database")
	}
}

func TestBackupWithMeta(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "test.db")
	db, err := Open(config.DatabaseConfig{Driver: "sqlite", Path: path})
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	db.CreateApp("test-app", "admin")

	meta, err := db.BackupWithMeta(context.Background(), "v1.0.0")
	if err != nil {
		t.Fatal(err)
	}
	if meta.ImageTag != "v1.0.0" {
		t.Errorf("ImageTag = %q, want %q", meta.ImageTag, "v1.0.0")
	}
	if meta.MigrationVersion == 0 {
		t.Error("expected non-zero migration version")
	}
	if meta.BackupPath == "" {
		t.Error("expected non-empty backup path")
	}

	// Verify metadata file was written.
	metaPath := meta.BackupPath + ".meta.json"
	if _, err := os.Stat(metaPath); err != nil {
		t.Fatalf("metadata file not found: %v", err)
	}
}

func TestLatestBackupMeta(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "test.db")
	db, err := Open(config.DatabaseConfig{Driver: "sqlite", Path: path})
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	// No backups yet.
	_, err = LatestBackupMeta(path)
	if err != ErrNoBackup {
		t.Errorf("expected ErrNoBackup, got %v", err)
	}

	// Create a backup.
	meta, err := db.BackupWithMeta(context.Background(), "v1.0.0")
	if err != nil {
		t.Fatal(err)
	}

	// Now should find it.
	latest, err := LatestBackupMeta(path)
	if err != nil {
		t.Fatal(err)
	}
	if latest.ImageTag != meta.ImageTag {
		t.Errorf("ImageTag = %q, want %q", latest.ImageTag, meta.ImageTag)
	}
}

func TestReadBackupMeta(t *testing.T) {
	dir := t.TempDir()

	// Valid metadata.
	validJSON := `{"backup_path":"/tmp/test.db.backup","image_tag":"v1.0.0","migration_version":5}`
	metaPath := filepath.Join(dir, "test.meta.json")
	os.WriteFile(metaPath, []byte(validJSON), 0o600)

	meta, err := readBackupMeta(metaPath)
	if err != nil {
		t.Fatal(err)
	}
	if meta.ImageTag != "v1.0.0" {
		t.Errorf("ImageTag = %q", meta.ImageTag)
	}
	if meta.MigrationVersion != 5 {
		t.Errorf("MigrationVersion = %d", meta.MigrationVersion)
	}

	// Invalid JSON.
	badPath := filepath.Join(dir, "bad.meta.json")
	os.WriteFile(badPath, []byte("not json"), 0o600)
	_, err = readBackupMeta(badPath)
	if err == nil {
		t.Error("expected error for invalid JSON")
	}

	// Missing file.
	_, err = readBackupMeta(filepath.Join(dir, "missing.meta.json"))
	if err == nil {
		t.Error("expected error for missing file")
	}
}

func TestBackupPostgres(t *testing.T) {
	if pgBaseURL == "" {
		t.Skip("BLOCKYARD_TEST_POSTGRES_URL not set")
	}

	// Check pg_dump is available.
	if _, err := exec.LookPath("pg_dump"); err != nil {
		t.Skip("pg_dump not available")
	}

	db := testPostgresDB(t)

	_, err := db.CreateApp("test-app", "admin")
	if err != nil {
		t.Fatal(err)
	}

	dest, err := db.Backup(context.Background())
	if err != nil {
		// pg_dump fails on version mismatch (e.g., pg_dump 15 vs server 17).
		if strings.Contains(err.Error(), "version mismatch") {
			t.Skip("pg_dump version mismatch with server")
		}
		t.Fatal(err)
	}
	defer os.Remove(dest)

	info, err := os.Stat(dest)
	if err != nil {
		t.Fatalf("backup file not found: %v", err)
	}
	if info.Size() == 0 {
		t.Error("backup file is empty")
	}
}
