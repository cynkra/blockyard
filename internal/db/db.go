package db

import (
	"context"
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
    access_type             TEXT NOT NULL DEFAULT 'acl' CHECK (access_type IN ('acl', 'logged_in', 'public')),
    active_bundle           TEXT REFERENCES bundles(id),
    max_workers_per_app     INTEGER,
    max_sessions_per_worker INTEGER DEFAULT 1,
    memory_limit            TEXT,
    cpu_limit               REAL,
    title                   TEXT,
    description             TEXT,
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
    kind        TEXT NOT NULL CHECK (kind IN ('user')),
    role        TEXT NOT NULL CHECK (role IN ('viewer', 'collaborator')),
    granted_by  TEXT NOT NULL,
    granted_at  TEXT NOT NULL,
    PRIMARY KEY (app_id, principal, kind)
);

CREATE TABLE IF NOT EXISTS users (
    sub        TEXT PRIMARY KEY,
    email      TEXT NOT NULL DEFAULT '',
    name       TEXT NOT NULL DEFAULT '',
    role       TEXT NOT NULL DEFAULT 'viewer',
    active     INTEGER NOT NULL DEFAULT 1,
    last_login TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS personal_access_tokens (
    id           TEXT PRIMARY KEY,
    token_hash   BLOB NOT NULL UNIQUE,
    user_sub     TEXT NOT NULL REFERENCES users(sub),
    name         TEXT NOT NULL,
    created_at   TEXT NOT NULL,
    expires_at   TEXT,
    last_used_at TEXT,
    revoked      INTEGER NOT NULL DEFAULT 0
);

CREATE INDEX IF NOT EXISTS idx_pat_token_hash
    ON personal_access_tokens(token_hash);
CREATE INDEX IF NOT EXISTS idx_pat_user_sub
    ON personal_access_tokens(user_sub);

CREATE TABLE IF NOT EXISTS tags (
    id         TEXT PRIMARY KEY,
    name       TEXT NOT NULL UNIQUE,
    created_at TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS app_tags (
    app_id TEXT NOT NULL REFERENCES apps(id) ON DELETE CASCADE,
    tag_id TEXT NOT NULL REFERENCES tags(id) ON DELETE CASCADE,
    PRIMARY KEY (app_id, tag_id)
);
`

type DB struct {
	*sql.DB
}

// IsUniqueConstraintError reports whether err is a SQLite UNIQUE
// constraint violation.
func IsUniqueConstraintError(err error) bool {
	return err != nil && strings.Contains(err.Error(), "UNIQUE constraint failed")
}

func Open(path string) (*DB, error) {
	if dir := filepath.Dir(path); dir != "." {
		if err := os.MkdirAll(dir, 0o700); err != nil {
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
	Title                *string
	Description          *string
	CreatedAt            string
	UpdatedAt            string
}

type BundleRow struct {
	ID         string `json:"id"`
	AppID      string `json:"app_id"`
	Status     string `json:"status"`
	UploadedAt string `json:"uploaded_at"`
}

// Ping verifies the database connection is alive.
func (db *DB) Ping(ctx context.Context) error {
	_, err := db.ExecContext(ctx, "SELECT 1")
	return err
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
		max_sessions_per_worker, memory_limit, cpu_limit, title, description, created_at, updated_at`

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
// with a user ACL grant + logged_in apps + public apps.
func (db *DB) ListAccessibleApps(sub string) ([]AppRow, error) {
	query := fmt.Sprintf(
		`SELECT DISTINCT a.%s
		 FROM apps a
		 LEFT JOIN app_access aa ON a.id = aa.app_id
		 WHERE a.access_type IN ('public', 'logged_in')
		    OR a.owner = ?
		    OR (aa.kind = 'user' AND aa.principal = ?)
		 ORDER BY a.created_at DESC`,
		strings.ReplaceAll(appColumns, "\n\t\t", " "),
	)

	rows, err := db.Query(query, sub, sub)
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
	Title                *string
	Description          *string
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
	if u.Title != nil {
		app.Title = u.Title
	}
	if u.Description != nil {
		app.Description = u.Description
	}

	now := time.Now().UTC().Format(time.RFC3339)
	_, err = db.Exec(
		`UPDATE apps SET
			max_workers_per_app = ?,
			max_sessions_per_worker = ?,
			memory_limit = ?,
			cpu_limit = ?,
			access_type = ?,
			title = ?,
			description = ?,
			updated_at = ?
		WHERE id = ?`,
		app.MaxWorkersPerApp, app.MaxSessionsPerWorker,
		app.MemoryLimit, app.CPULimit,
		app.AccessType,
		app.Title, app.Description,
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
		&app.Title, &app.Description,
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
			&app.Title, &app.Description,
			&app.CreatedAt, &app.UpdatedAt); err != nil {
			return nil, err
		}
		apps = append(apps, app)
	}
	return apps, rows.Err()
}

// --- Users ---

// UserRow represents a row from the users table.
type UserRow struct {
	Sub       string `json:"sub"`
	Email     string `json:"email"`
	Name      string `json:"name"`
	Role      string `json:"role"`
	Active    bool   `json:"active"`
	LastLogin string `json:"last_login"`
}

// UpsertUser creates or updates a user record on OIDC login.
// On conflict (existing user), updates email, name, and last_login
// but preserves role and active status set by admins.
func (db *DB) UpsertUser(sub, email, name string) (*UserRow, error) {
	now := time.Now().UTC().Format(time.RFC3339)
	_, err := db.Exec(
		`INSERT INTO users (sub, email, name, last_login)
		 VALUES (?, ?, ?, ?)
		 ON CONFLICT (sub) DO UPDATE SET
		     email = excluded.email,
		     name = excluded.name,
		     last_login = excluded.last_login`,
		sub, email, name, now,
	)
	if err != nil {
		return nil, fmt.Errorf("upsert user: %w", err)
	}
	return db.GetUser(sub)
}

// UpsertUserWithRole creates a user with a specific role (used for initial_admin).
// If the user already exists, only updates email, name, and last_login.
func (db *DB) UpsertUserWithRole(sub, email, name, role string) (*UserRow, error) {
	now := time.Now().UTC().Format(time.RFC3339)
	_, err := db.Exec(
		`INSERT INTO users (sub, email, name, role, last_login)
		 VALUES (?, ?, ?, ?, ?)
		 ON CONFLICT (sub) DO UPDATE SET
		     email = excluded.email,
		     name = excluded.name,
		     last_login = excluded.last_login`,
		sub, email, name, role, now,
	)
	if err != nil {
		return nil, fmt.Errorf("upsert user with role: %w", err)
	}
	return db.GetUser(sub)
}

func (db *DB) GetUser(sub string) (*UserRow, error) {
	var u UserRow
	err := db.QueryRow(
		"SELECT sub, email, name, role, active, last_login FROM users WHERE sub = ?", sub,
	).Scan(&u.Sub, &u.Email, &u.Name, &u.Role, &u.Active, &u.LastLogin)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &u, nil
}

func (db *DB) ListUsers() ([]UserRow, error) {
	rows, err := db.Query(
		"SELECT sub, email, name, role, active, last_login FROM users ORDER BY last_login DESC",
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var users []UserRow
	for rows.Next() {
		var u UserRow
		if err := rows.Scan(&u.Sub, &u.Email, &u.Name, &u.Role, &u.Active, &u.LastLogin); err != nil {
			return nil, err
		}
		users = append(users, u)
	}
	return users, rows.Err()
}

// UserUpdate holds optional fields for updating a user.
type UserUpdate struct {
	Role   *string
	Active *bool
}

func (db *DB) UpdateUser(sub string, u UserUpdate) (*UserRow, error) {
	user, err := db.GetUser(sub)
	if err != nil {
		return nil, err
	}
	if user == nil {
		return nil, nil
	}

	if u.Role != nil {
		user.Role = *u.Role
	}
	if u.Active != nil {
		user.Active = *u.Active
	}

	_, err = db.Exec(
		"UPDATE users SET role = ?, active = ? WHERE sub = ?",
		user.Role, user.Active, sub,
	)
	if err != nil {
		return nil, fmt.Errorf("update user: %w", err)
	}

	return db.GetUser(sub)
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

// --- Tags ---

// TagRow represents a row from the tags table.
type TagRow struct {
	ID        string
	Name      string
	CreatedAt string
}

func (db *DB) CreateTag(name string) (*TagRow, error) {
	id := uuid.New().String()
	now := time.Now().UTC().Format(time.RFC3339)
	_, err := db.Exec(
		"INSERT INTO tags (id, name, created_at) VALUES (?, ?, ?)",
		id, name, now,
	)
	if err != nil {
		return nil, err
	}
	return &TagRow{ID: id, Name: name, CreatedAt: now}, nil
}

func (db *DB) GetTag(id string) (*TagRow, error) {
	var t TagRow
	err := db.QueryRow("SELECT id, name, created_at FROM tags WHERE id = ?", id).
		Scan(&t.ID, &t.Name, &t.CreatedAt)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &t, nil
}

func (db *DB) ListTags() ([]TagRow, error) {
	rows, err := db.Query("SELECT id, name, created_at FROM tags ORDER BY name")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var tags []TagRow
	for rows.Next() {
		var t TagRow
		if err := rows.Scan(&t.ID, &t.Name, &t.CreatedAt); err != nil {
			return nil, err
		}
		tags = append(tags, t)
	}
	return tags, rows.Err()
}

func (db *DB) DeleteTag(id string) (bool, error) {
	result, err := db.Exec("DELETE FROM tags WHERE id = ?", id)
	if err != nil {
		return false, err
	}
	n, _ := result.RowsAffected()
	return n > 0, nil
}

func (db *DB) AddAppTag(appID, tagID string) error {
	_, err := db.Exec(
		"INSERT OR IGNORE INTO app_tags (app_id, tag_id) VALUES (?, ?)",
		appID, tagID,
	)
	return err
}

func (db *DB) RemoveAppTag(appID, tagID string) (bool, error) {
	result, err := db.Exec(
		"DELETE FROM app_tags WHERE app_id = ? AND tag_id = ?",
		appID, tagID,
	)
	if err != nil {
		return false, err
	}
	n, _ := result.RowsAffected()
	return n > 0, nil
}

func (db *DB) ListAppTags(appID string) ([]TagRow, error) {
	rows, err := db.Query(
		`SELECT t.id, t.name, t.created_at
		 FROM tags t
		 JOIN app_tags at ON t.id = at.tag_id
		 WHERE at.app_id = ?
		 ORDER BY t.name`,
		appID,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var tags []TagRow
	for rows.Next() {
		var t TagRow
		if err := rows.Scan(&t.ID, &t.Name, &t.CreatedAt); err != nil {
			return nil, err
		}
		tags = append(tags, t)
	}
	return tags, rows.Err()
}

// --- Catalog ---

// CatalogParams holds query parameters for the catalog listing.
type CatalogParams struct {
	CallerSub  string
	CallerRole string // "admin", "publisher", "viewer", or ""
	Tag        string
	Search     string
	Page       int
	PerPage    int
}

// ListCatalog returns apps visible to the caller with access control,
// tag filtering, search, and pagination.
func (db *DB) ListCatalog(params CatalogParams) ([]AppRow, int, error) {
	var conditions []string
	var args []any

	// Access control filter
	if params.CallerRole == "admin" {
		// Admin sees everything — no filter
	} else if params.CallerSub != "" {
		accessFilter := `(
			apps.owner = ?
			OR apps.access_type IN ('public', 'logged_in')
			OR EXISTS (
				SELECT 1 FROM app_access
				WHERE app_access.app_id = apps.id
				AND app_access.kind = 'user'
				AND app_access.principal = ?
			)
		)`

		conditions = append(conditions, accessFilter)
		args = append(args, params.CallerSub, params.CallerSub)
	} else {
		// Unauthenticated — public apps only
		conditions = append(conditions, "apps.access_type = 'public'")
	}

	// Tag filter
	if params.Tag != "" {
		conditions = append(conditions,
			`EXISTS (
				SELECT 1 FROM app_tags
				JOIN tags ON tags.id = app_tags.tag_id
				WHERE app_tags.app_id = apps.id AND tags.name = ?
			)`)
		args = append(args, params.Tag)
	}

	// Search filter
	if params.Search != "" {
		conditions = append(conditions,
			"(apps.name LIKE ? OR apps.title LIKE ? OR apps.description LIKE ?)")
		like := "%" + params.Search + "%"
		args = append(args, like, like, like)
	}

	where := ""
	if len(conditions) > 0 {
		where = "WHERE " + strings.Join(conditions, " AND ")
	}

	// Count total
	var total int
	countQuery := "SELECT COUNT(*) FROM apps " + where
	if err := db.QueryRow(countQuery, args...).Scan(&total); err != nil {
		return nil, 0, err
	}

	// Fetch page
	query := fmt.Sprintf(
		`SELECT %s FROM apps %s ORDER BY apps.updated_at DESC LIMIT ? OFFSET ?`,
		appColumns, where,
	)
	pageArgs := append(append([]any{}, args...), params.PerPage, (params.Page-1)*params.PerPage)

	rows, err := db.Query(query, pageArgs...)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	apps, err := scanApps(rows)
	if err != nil {
		return nil, 0, err
	}

	return apps, total, nil
}

// --- Personal Access Tokens ---

// PATRow represents a row from the personal_access_tokens table.
type PATRow struct {
	ID         string  `json:"id"`
	UserSub    string  `json:"user_sub,omitempty"`
	Name       string  `json:"name"`
	CreatedAt  string  `json:"created_at"`
	ExpiresAt  *string `json:"expires_at"`
	LastUsedAt *string `json:"last_used_at"`
	Revoked    bool    `json:"revoked"`
}

// PATLookupResult is the result of looking up a PAT by hash,
// joined with the owning user.
type PATLookupResult struct {
	PAT  PATRow
	User UserRow
}

func (db *DB) CreatePAT(id string, tokenHash []byte, userSub, name string, expiresAt *string) (*PATRow, error) {
	now := time.Now().UTC().Format(time.RFC3339)
	_, err := db.Exec(
		`INSERT INTO personal_access_tokens (id, token_hash, user_sub, name, created_at, expires_at)
		 VALUES (?, ?, ?, ?, ?, ?)`,
		id, tokenHash, userSub, name, now, expiresAt,
	)
	if err != nil {
		return nil, fmt.Errorf("create PAT: %w", err)
	}
	return &PATRow{
		ID:        id,
		UserSub:   userSub,
		Name:      name,
		CreatedAt: now,
		ExpiresAt: expiresAt,
	}, nil
}

// LookupPATByHash looks up a PAT by its SHA-256 hash, joined with the
// owning user. Returns nil if not found.
func (db *DB) LookupPATByHash(tokenHash []byte) (*PATLookupResult, error) {
	var pat PATRow
	var user UserRow
	err := db.QueryRow(
		`SELECT p.id, p.user_sub, p.name, p.created_at, p.expires_at, p.last_used_at, p.revoked,
		        u.sub, u.email, u.name, u.role, u.active, u.last_login
		 FROM personal_access_tokens p
		 JOIN users u ON p.user_sub = u.sub
		 WHERE p.token_hash = ?`,
		tokenHash,
	).Scan(&pat.ID, &pat.UserSub, &pat.Name, &pat.CreatedAt, &pat.ExpiresAt, &pat.LastUsedAt, &pat.Revoked,
		&user.Sub, &user.Email, &user.Name, &user.Role, &user.Active, &user.LastLogin)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &PATLookupResult{PAT: pat, User: user}, nil
}

func (db *DB) ListPATsByUser(userSub string) ([]PATRow, error) {
	rows, err := db.Query(
		`SELECT id, name, created_at, expires_at, last_used_at, revoked
		 FROM personal_access_tokens
		 WHERE user_sub = ?
		 ORDER BY created_at DESC`,
		userSub,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var pats []PATRow
	for rows.Next() {
		var p PATRow
		if err := rows.Scan(&p.ID, &p.Name, &p.CreatedAt, &p.ExpiresAt, &p.LastUsedAt, &p.Revoked); err != nil {
			return nil, err
		}
		pats = append(pats, p)
	}
	return pats, rows.Err()
}

func (db *DB) RevokePAT(id, userSub string) (bool, error) {
	result, err := db.Exec(
		"UPDATE personal_access_tokens SET revoked = 1 WHERE id = ? AND user_sub = ?",
		id, userSub,
	)
	if err != nil {
		return false, err
	}
	n, _ := result.RowsAffected()
	return n > 0, nil
}

func (db *DB) RevokeAllPATs(userSub string) (int64, error) {
	result, err := db.Exec(
		"UPDATE personal_access_tokens SET revoked = 1 WHERE user_sub = ? AND revoked = 0",
		userSub,
	)
	if err != nil {
		return 0, err
	}
	return result.RowsAffected()
}

func (db *DB) UpdatePATLastUsed(ctx context.Context, id string) {
	now := time.Now().UTC().Format(time.RFC3339)
	_, _ = db.ExecContext(ctx,
		"UPDATE personal_access_tokens SET last_used_at = ? WHERE id = ?",
		now, id,
	)
}
