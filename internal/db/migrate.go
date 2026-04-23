package db

import (
	"fmt"
	"io/fs"
	"strings"

	"github.com/golang-migrate/migrate/v4"
)

// MigrationVersion returns the current migration version and dirty flag.
func (db *DB) MigrationVersion() (uint, bool, error) {
	m, cleanup, err := db.newMigrator()
	if err != nil {
		return 0, false, fmt.Errorf("migration version: %w", err)
	}
	defer cleanup()
	ver, dirty, err := m.Version()
	if err != nil {
		return 0, false, fmt.Errorf("migration version: %w", err)
	}
	return ver, dirty, nil
}

// MigrateDown runs down migrations to the target version.
func (db *DB) MigrateDown(targetVersion uint) error {
	m, cleanup, err := db.newMigrator()
	if err != nil {
		return fmt.Errorf("migrate down: %w", err)
	}
	defer cleanup()

	currentVer, _, err := m.Version()
	if err != nil {
		return fmt.Errorf("migrate down: read version: %w", err)
	}

	// Step down one version at a time until we reach the target.
	for currentVer > targetVersion {
		if err := m.Steps(-1); err != nil && err != migrate.ErrNoChange {
			return fmt.Errorf("migrate down from %d: %w", currentVer, err)
		}
		currentVer, _, err = m.Version()
		if err != nil {
			return fmt.Errorf("migrate down: read version after step: %w", err)
		}
	}
	return nil
}

// CheckDownMigrationSafety verifies that all down migrations between
// fromVersion and toVersion are reversible. Returns an error describing
// the first irreversible migration found.
func (db *DB) CheckDownMigrationSafety(toVersion, fromVersion uint) error {
	for v := fromVersion; v > toVersion; v-- {
		content := db.readDownMigration(v)
		if strings.Contains(content, "-- irreversible:") {
			return fmt.Errorf("migration %03d is irreversible", v)
		}
	}
	return nil
}

// readDownMigration reads the content of a down migration file from the
// embedded filesystem. Returns empty string if the file doesn't exist.
func (db *DB) readDownMigration(version uint) string {
	var fsys fs.FS
	var err error

	switch db.dialect {
	case DialectSQLite:
		fsys, err = fs.Sub(sqliteMigrations, "migrations/sqlite")
	case DialectPostgres:
		fsys, err = fs.Sub(postgresMigrations, "migrations/postgres")
	}
	if err != nil {
		return ""
	}

	filename := fmt.Sprintf("%03d_*.down.sql", version)
	matches, err := fs.Glob(fsys, filename)
	if err != nil || len(matches) == 0 {
		return ""
	}

	data, err := fs.ReadFile(fsys, matches[0])
	if err != nil {
		return ""
	}
	return string(data)
}
