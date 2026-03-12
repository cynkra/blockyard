package db

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/google/uuid"
	_ "modernc.org/sqlite"
)

const schema = `
CREATE TABLE IF NOT EXISTS apps (
    id                      TEXT PRIMARY KEY,
    name                    TEXT NOT NULL UNIQUE,
    owner                   TEXT NOT NULL DEFAULT 'admin',
    access_type             TEXT NOT NULL DEFAULT 'acl' CHECK (access_type IN ('acl', 'public')),
    active_bundle           TEXT REFERENCES bundles(id),
    max_workers_per_app     INTEGER,
    max_sessions_per_worker INTEGER DEFAULT 1,
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

CREATE TABLE IF NOT EXISTS app_access (
    app_id      TEXT NOT NULL REFERENCES apps(id) ON DELETE CASCADE,
    principal   TEXT NOT NULL,
    kind        TEXT NOT NULL CHECK (kind IN ('user', 'group')),
    role        TEXT NOT NULL CHECK (role IN ('viewer', 'collaborator')),
    granted_by  TEXT NOT NULL,
    granted_at  TEXT NOT NULL,
    PRIMARY KEY (app_id, principal, kind)
);

CREATE TABLE IF NOT EXISTS role_mappings (
    group_name  TEXT NOT NULL,
    role        TEXT NOT NULL CHECK (role IN ('admin', 'publisher', 'viewer')),
    PRIMARY KEY (group_name)
);
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

	// SQLite does not benefit from multiple concurrent connections and
	// in-memory databases are per-connection, so pin to a single conn.
	sqlDB.SetMaxOpenConns(1)

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
	Owner                string
	AccessType           string
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

func (db *DB) CreateApp(name, owner string) (*AppRow, error) {
	id := uuid.New().String()
	now := time.Now().UTC().Format(time.RFC3339)

	_, err := db.Exec(
		`INSERT INTO apps (id, name, owner, max_sessions_per_worker, created_at, updated_at)
		 VALUES (?, ?, ?, 1, ?, ?)`,
		id, name, owner, now, now,
	)
	if err != nil {
		return nil, fmt.Errorf("insert app: %w", err)
	}

	return db.GetApp(id)
}

const appColumns = `id, name, owner, access_type, active_bundle, max_workers_per_app,
		max_sessions_per_worker, memory_limit, cpu_limit, created_at, updated_at`

func (db *DB) GetApp(id string) (*AppRow, error) {
	row := db.QueryRow(`SELECT `+appColumns+` FROM apps WHERE id = ?`, id)
	return scanApp(row)
}

func (db *DB) GetAppByName(name string) (*AppRow, error) {
	row := db.QueryRow(`SELECT `+appColumns+` FROM apps WHERE name = ?`, name)
	return scanApp(row)
}

func (db *DB) ListApps() ([]AppRow, error) {
	rows, err := db.Query(`SELECT ` + appColumns + ` FROM apps ORDER BY created_at DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanApps(rows)
}

// ListAccessibleApps returns apps the caller can see: owned apps + apps
// with an ACL grant matching the caller's sub or any of their groups +
// public apps.
func (db *DB) ListAccessibleApps(sub string, groups []string) ([]AppRow, error) {
	args := []any{sub, sub} // owner check + direct user grant

	groupClause := "SELECT 1 WHERE 0" // no groups -> never matches
	if len(groups) > 0 {
		placeholders := make([]string, len(groups))
		for i, g := range groups {
			placeholders[i] = "?"
			args = append(args, g)
		}
		groupClause = strings.Join(placeholders, ", ")
	}

	query := fmt.Sprintf(
		`SELECT DISTINCT a.%s
		 FROM apps a
		 LEFT JOIN app_access aa ON a.id = aa.app_id
		 WHERE a.access_type = 'public'
		    OR a.owner = ?
		    OR (aa.kind = 'user'  AND aa.principal = ?)
		    OR (aa.kind = 'group' AND aa.principal IN (%s))
		 ORDER BY a.created_at DESC`,
		strings.ReplaceAll(appColumns, "\n\t\t", " "),
		groupClause,
	)

	// Reorder args: the query uses owner=? then principal=? then group IN(?)
	// which matches our args order: sub, sub, groups...
	rows, err := db.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanApps(rows)
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

// AppUpdate holds optional fields for updating an app's configuration.
type AppUpdate struct {
	MaxWorkersPerApp     *int
	MaxSessionsPerWorker *int
	MemoryLimit          *string
	CPULimit             *float64
	AccessType           *string
}

// UpdateApp applies partial updates to an app's configuration.
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
	if u.AccessType != nil {
		app.AccessType = *u.AccessType
	}

	now := time.Now().UTC().Format(time.RFC3339)
	_, err = db.Exec(
		`UPDATE apps SET
			max_workers_per_app = ?,
			max_sessions_per_worker = ?,
			memory_limit = ?,
			cpu_limit = ?,
			access_type = ?,
			updated_at = ?
		WHERE id = ?`,
		app.MaxWorkersPerApp, app.MaxSessionsPerWorker,
		app.MemoryLimit, app.CPULimit,
		app.AccessType,
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
	err := row.Scan(&app.ID, &app.Name, &app.Owner, &app.AccessType,
		&app.ActiveBundle, &app.MaxWorkersPerApp, &app.MaxSessionsPerWorker,
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

func scanApps(rows *sql.Rows) ([]AppRow, error) {
	var apps []AppRow
	for rows.Next() {
		var app AppRow
		if err := rows.Scan(&app.ID, &app.Name, &app.Owner, &app.AccessType,
			&app.ActiveBundle, &app.MaxWorkersPerApp, &app.MaxSessionsPerWorker,
			&app.MemoryLimit, &app.CPULimit,
			&app.CreatedAt, &app.UpdatedAt); err != nil {
			return nil, err
		}
		apps = append(apps, app)
	}
	return apps, rows.Err()
}

// --- Role mappings ---

// RoleMappingRow represents a row from the role_mappings table.
type RoleMappingRow struct {
	GroupName string
	Role      string
}

func (db *DB) ListRoleMappings() ([]RoleMappingRow, error) {
	rows, err := db.Query("SELECT group_name, role FROM role_mappings")
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var mappings []RoleMappingRow
	for rows.Next() {
		var m RoleMappingRow
		if err := rows.Scan(&m.GroupName, &m.Role); err != nil {
			return nil, err
		}
		mappings = append(mappings, m)
	}
	return mappings, rows.Err()
}

func (db *DB) UpsertRoleMapping(groupName, role string) error {
	_, err := db.Exec(
		`INSERT INTO role_mappings (group_name, role) VALUES (?, ?)
		 ON CONFLICT (group_name) DO UPDATE SET role = excluded.role`,
		groupName, role,
	)
	return err
}

func (db *DB) DeleteRoleMapping(groupName string) (bool, error) {
	result, err := db.Exec(
		"DELETE FROM role_mappings WHERE group_name = ?", groupName,
	)
	if err != nil {
		return false, err
	}
	n, _ := result.RowsAffected()
	return n > 0, nil
}

// --- App access (ACL) ---

// AppAccessRow represents a row from the app_access table.
type AppAccessRow struct {
	AppID     string
	Principal string
	Kind      string
	Role      string
	GrantedBy string
	GrantedAt string
}

func (db *DB) ListAppAccess(appID string) ([]AppAccessRow, error) {
	rows, err := db.Query(
		"SELECT app_id, principal, kind, role, granted_by, granted_at FROM app_access WHERE app_id = ?",
		appID,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var grants []AppAccessRow
	for rows.Next() {
		var g AppAccessRow
		if err := rows.Scan(&g.AppID, &g.Principal, &g.Kind, &g.Role, &g.GrantedBy, &g.GrantedAt); err != nil {
			return nil, err
		}
		grants = append(grants, g)
	}
	return grants, rows.Err()
}

func (db *DB) GrantAppAccess(appID, principal, kind, role, grantedBy string) error {
	now := time.Now().UTC().Format(time.RFC3339)
	_, err := db.Exec(
		`INSERT INTO app_access (app_id, principal, kind, role, granted_by, granted_at)
		 VALUES (?, ?, ?, ?, ?, ?)
		 ON CONFLICT (app_id, principal, kind)
		 DO UPDATE SET role = excluded.role,
		               granted_by = excluded.granted_by,
		               granted_at = excluded.granted_at`,
		appID, principal, kind, role, grantedBy, now,
	)
	return err
}

func (db *DB) RevokeAppAccess(appID, principal, kind string) (bool, error) {
	result, err := db.Exec(
		"DELETE FROM app_access WHERE app_id = ? AND principal = ? AND kind = ?",
		appID, principal, kind,
	)
	if err != nil {
		return false, err
	}
	n, _ := result.RowsAffected()
	return n > 0, nil
}
