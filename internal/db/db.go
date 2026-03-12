package db

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/google/uuid"
	_ "modernc.org/sqlite"
)

const schema = `
CREATE TABLE IF NOT EXISTS apps (
    id                      TEXT PRIMARY KEY,
    name                    TEXT NOT NULL UNIQUE,
    active_bundle           TEXT REFERENCES bundles(id),
    max_workers_per_app     INTEGER,
    max_sessions_per_worker INTEGER NOT NULL DEFAULT 1,
    memory_limit            TEXT,
    cpu_limit               REAL,
    created_at              TEXT NOT NULL,
    updated_at              TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS bundles (
    id          TEXT PRIMARY KEY,
    app_id      TEXT NOT NULL REFERENCES apps(id),
    status      TEXT NOT NULL DEFAULT 'pending',
    uploaded_at TEXT NOT NULL
);

CREATE INDEX IF NOT EXISTS idx_bundles_app_id ON bundles(app_id);
`

type DB struct {
	*sql.DB
}

func Open(path string) (*DB, error) {
	if dir := filepath.Dir(path); dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, fmt.Errorf("create db directory: %w", err)
		}
	}

	sqlDB, err := sql.Open("sqlite", path+"?_pragma=foreign_keys(1)")
	if err != nil {
		return nil, fmt.Errorf("open database: %w", err)
	}

	if err := sqlDB.Ping(); err != nil {
		sqlDB.Close()
		return nil, fmt.Errorf("ping database: %w", err)
	}

	if _, err := sqlDB.Exec(schema); err != nil {
		sqlDB.Close()
		return nil, fmt.Errorf("create schema: %w", err)
	}

	return &DB{sqlDB}, nil
}

type AppRow struct {
	ID                   string
	Name                 string
	ActiveBundle         *string
	MaxWorkersPerApp     *int
	MaxSessionsPerWorker int
	MemoryLimit          *string
	CPULimit             *float64
	CreatedAt            string
	UpdatedAt            string
}

type BundleRow struct {
	ID         string `json:"id"`
	AppID      string `json:"app_id"`
	Status     string `json:"status"`
	UploadedAt string `json:"uploaded_at"`
}

func (db *DB) CreateApp(name string) (*AppRow, error) {
	id := uuid.New().String()
	now := time.Now().UTC().Format(time.RFC3339)

	_, err := db.Exec(
		`INSERT INTO apps (id, name, max_sessions_per_worker, created_at, updated_at)
		 VALUES (?, ?, 1, ?, ?)`,
		id, name, now, now,
	)
	if err != nil {
		return nil, fmt.Errorf("insert app: %w", err)
	}

	return db.GetApp(id)
}

func (db *DB) GetApp(id string) (*AppRow, error) {
	row := db.QueryRow(`SELECT id, name, active_bundle, max_workers_per_app,
		max_sessions_per_worker, memory_limit, cpu_limit, created_at, updated_at
		FROM apps WHERE id = ?`, id)
	return scanApp(row)
}

func (db *DB) GetAppByName(name string) (*AppRow, error) {
	row := db.QueryRow(`SELECT id, name, active_bundle, max_workers_per_app,
		max_sessions_per_worker, memory_limit, cpu_limit, created_at, updated_at
		FROM apps WHERE name = ?`, name)
	return scanApp(row)
}

func (db *DB) ListApps() ([]AppRow, error) {
	rows, err := db.Query(`SELECT id, name, active_bundle, max_workers_per_app,
		max_sessions_per_worker, memory_limit, cpu_limit, created_at, updated_at
		FROM apps ORDER BY created_at DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var apps []AppRow
	for rows.Next() {
		var app AppRow
		if err := rows.Scan(&app.ID, &app.Name, &app.ActiveBundle,
			&app.MaxWorkersPerApp, &app.MaxSessionsPerWorker,
			&app.MemoryLimit, &app.CPULimit,
			&app.CreatedAt, &app.UpdatedAt); err != nil {
			return nil, err
		}
		apps = append(apps, app)
	}
	return apps, rows.Err()
}

func (db *DB) DeleteApp(id string) (bool, error) {
	result, err := db.Exec(`DELETE FROM apps WHERE id = ?`, id)
	if err != nil {
		return false, err
	}
	n, _ := result.RowsAffected()
	return n > 0, nil
}

func (db *DB) CreateBundle(id, appID string) (*BundleRow, error) {
	now := time.Now().UTC().Format(time.RFC3339)
	_, err := db.Exec(
		`INSERT INTO bundles (id, app_id, status, uploaded_at)
		 VALUES (?, ?, 'pending', ?)`,
		id, appID, now,
	)
	if err != nil {
		return nil, fmt.Errorf("insert bundle: %w", err)
	}
	return db.GetBundle(id)
}

func (db *DB) GetBundle(id string) (*BundleRow, error) {
	row := db.QueryRow(
		`SELECT id, app_id, status, uploaded_at FROM bundles WHERE id = ?`, id,
	)
	var b BundleRow
	err := row.Scan(&b.ID, &b.AppID, &b.Status, &b.UploadedAt)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &b, nil
}

func (db *DB) ListBundlesByApp(appID string) ([]BundleRow, error) {
	rows, err := db.Query(
		`SELECT id, app_id, status, uploaded_at
		 FROM bundles WHERE app_id = ?
		 ORDER BY uploaded_at DESC`, appID,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	bundles := make([]BundleRow, 0)
	for rows.Next() {
		var b BundleRow
		if err := rows.Scan(&b.ID, &b.AppID, &b.Status, &b.UploadedAt); err != nil {
			return nil, err
		}
		bundles = append(bundles, b)
	}
	return bundles, rows.Err()
}

func (db *DB) UpdateBundleStatus(id, status string) error {
	_, err := db.Exec(
		`UPDATE bundles SET status = ? WHERE id = ?`, status, id,
	)
	return err
}

func (db *DB) SetActiveBundle(appID, bundleID string) error {
	now := time.Now().UTC().Format(time.RFC3339)
	_, err := db.Exec(
		`UPDATE apps SET active_bundle = ?, updated_at = ? WHERE id = ?`,
		bundleID, now, appID,
	)
	return err
}

func (db *DB) DeleteBundle(id string) (bool, error) {
	result, err := db.Exec(`DELETE FROM bundles WHERE id = ?`, id)
	if err != nil {
		return false, err
	}
	n, _ := result.RowsAffected()
	return n > 0, nil
}

// AppUpdate holds optional fields for updating an app's resource limits.
type AppUpdate struct {
	MaxWorkersPerApp     *int
	MaxSessionsPerWorker *int
	MemoryLimit          *string
	CPULimit             *float64
}

// UpdateApp applies partial updates to an app's resource limits.
// Uses fetch-modify-write since updates are rare admin operations.
func (db *DB) UpdateApp(id string, u AppUpdate) (*AppRow, error) {
	app, err := db.GetApp(id)
	if err != nil {
		return nil, err
	}
	if app == nil {
		return nil, fmt.Errorf("app not found")
	}

	if u.MaxWorkersPerApp != nil {
		app.MaxWorkersPerApp = u.MaxWorkersPerApp
	}
	if u.MaxSessionsPerWorker != nil {
		app.MaxSessionsPerWorker = *u.MaxSessionsPerWorker
	}
	if u.MemoryLimit != nil {
		app.MemoryLimit = u.MemoryLimit
	}
	if u.CPULimit != nil {
		app.CPULimit = u.CPULimit
	}

	now := time.Now().UTC().Format(time.RFC3339)
	_, err = db.Exec(
		`UPDATE apps SET
			max_workers_per_app = ?,
			max_sessions_per_worker = ?,
			memory_limit = ?,
			cpu_limit = ?,
			updated_at = ?
		WHERE id = ?`,
		app.MaxWorkersPerApp, app.MaxSessionsPerWorker,
		app.MemoryLimit, app.CPULimit,
		now, id,
	)
	if err != nil {
		return nil, fmt.Errorf("update app: %w", err)
	}

	return db.GetApp(id)
}

// ClearActiveBundle sets active_bundle to NULL for the given app.
func (db *DB) ClearActiveBundle(appID string) error {
	now := time.Now().UTC().Format(time.RFC3339)
	_, err := db.Exec(
		`UPDATE apps SET active_bundle = NULL, updated_at = ? WHERE id = ?`,
		now, appID,
	)
	return err
}

func (db *DB) FailStaleBuilds() (int64, error) {
	result, err := db.Exec(
		`UPDATE bundles SET status = 'failed' WHERE status = 'building'`,
	)
	if err != nil {
		return 0, err
	}
	return result.RowsAffected()
}

func scanApp(row *sql.Row) (*AppRow, error) {
	var app AppRow
	err := row.Scan(&app.ID, &app.Name, &app.ActiveBundle,
		&app.MaxWorkersPerApp, &app.MaxSessionsPerWorker,
		&app.MemoryLimit, &app.CPULimit,
		&app.CreatedAt, &app.UpdatedAt)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &app, nil
}
