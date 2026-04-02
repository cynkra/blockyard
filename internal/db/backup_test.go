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
