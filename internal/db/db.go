package db

import (
	"context"
	"database/sql"
	"embed"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/golang-migrate/migrate/v4"
	migratedb "github.com/golang-migrate/migrate/v4/database"
	migratepostgres "github.com/golang-migrate/migrate/v4/database/postgres"
	migratesqlite "github.com/golang-migrate/migrate/v4/database/sqlite3"
	"github.com/golang-migrate/migrate/v4/source/iofs"
	"github.com/google/uuid"
	"github.com/jmoiron/sqlx"

	"github.com/cynkra/blockyard/internal/config"

	_ "github.com/jackc/pgx/v5/stdlib"
	_ "modernc.org/sqlite"
)

//go:embed migrations/sqlite/*.sql
var sqliteMigrations embed.FS

//go:embed migrations/postgres/*.sql
var postgresMigrations embed.FS

// Dialect identifies the SQL dialect in use.
type Dialect int

const (
	DialectSQLite Dialect = iota
	DialectPostgres
)

// DB wraps sqlx.DB with dialect awareness.
type DB struct {
	*sqlx.DB
	dialect  Dialect
	tempPath string // non-empty when using a temp file for SQLite :memory:
}

// Open opens a database connection based on the config.
func Open(cfg config.DatabaseConfig) (*DB, error) {
	switch cfg.Driver {
	case "sqlite":
		return openSQLite(cfg.Path)
	case "postgres":
		return openPostgres(cfg.URL)
	default:
		return nil, fmt.Errorf("unsupported database driver: %q", cfg.Driver)
	}
}

func openSQLite(path string) (*DB, error) {
	var tempPath string
	dsn := path + "?_pragma=foreign_keys(1)"
	if path == ":memory:" {
		// Plain ":memory:" gives each driver connection its own database;
		// if the sql pool ever closes and reopens the connection the data
		// is lost. Use a temp file instead — it is deleted on Close().
		f, err := os.CreateTemp("", "blockyard-*.db")
		if err != nil {
			return nil, fmt.Errorf("create temp db: %w", err)
		}
		tempPath = f.Name()
		f.Close()
		dsn = tempPath + "?_pragma=foreign_keys(1)"
	} else if dir := filepath.Dir(path); dir != "." {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return nil, fmt.Errorf("create db directory: %w", err)
		}
	}

	db, err := sqlx.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open sqlite: %w", err)
	}

	// SQLite does not benefit from multiple concurrent connections and
	// in-memory databases are per-connection, so pin to a single conn.
	db.SetMaxOpenConns(1)

	if err := db.Ping(); err != nil {
		db.Close()
		return nil, fmt.Errorf("ping sqlite: %w", err)
	}

	d := &DB{DB: db, dialect: DialectSQLite, tempPath: tempPath}
	if err := d.runMigrations(); err != nil {
		db.Close()
		return nil, err
	}
	return d, nil
}

func openPostgres(url string) (*DB, error) {
	db, err := sqlx.Open("pgx", url)
	if err != nil {
		return nil, fmt.Errorf("open postgres: %w", err)
	}

	// Reasonable pool defaults — tune via connection string parameters
	// if needed (e.g. ?pool_max_conns=20).
	db.SetMaxOpenConns(25)
	db.SetMaxIdleConns(5)
	db.SetConnMaxLifetime(5 * time.Minute)

	if err := db.Ping(); err != nil {
		db.Close()
		return nil, fmt.Errorf("ping postgres: %w", err)
	}

	d := &DB{DB: db, dialect: DialectPostgres, tempPath: ""}
	if err := d.runMigrations(); err != nil {
		db.Close()
		return nil, err
	}
	return d, nil
}

func (db *DB) runMigrations() error {
	var fsys fs.FS
	var err error

	switch db.dialect {
	case DialectSQLite:
		fsys, err = fs.Sub(sqliteMigrations, "migrations/sqlite")
	case DialectPostgres:
		fsys, err = fs.Sub(postgresMigrations, "migrations/postgres")
	}
	if err != nil {
		return fmt.Errorf("migration fs: %w", err)
	}

	source, err := iofs.New(fsys, ".")
	if err != nil {
		return fmt.Errorf("migration source: %w", err)
	}

	driver, err := db.migrateDriver()
	if err != nil {
		return fmt.Errorf("migration driver: %w", err)
	}

	m, err := migrate.NewWithInstance("iofs", source, db.driverName(), driver)
	if err != nil {
		return fmt.Errorf("create migrator: %w", err)
	}

	if err := m.Up(); err != nil && err != migrate.ErrNoChange {
		return fmt.Errorf("run migrations: %w", err)
	}

	return nil
}

func (db *DB) migrateDriver() (migratedb.Driver, error) {
	switch db.dialect {
	case DialectPostgres:
		return migratepostgres.WithInstance(db.DB.DB, &migratepostgres.Config{})
	default:
		return migratesqlite.WithInstance(db.DB.DB, &migratesqlite.Config{
			NoTxWrap: true,
		})
	}
}

func (db *DB) driverName() string {
	switch db.dialect {
	case DialectPostgres:
		return "postgres"
	default:
		return "sqlite3"
	}
}

// rebind rewrites ? placeholders for the active dialect.
func (db *DB) rebind(query string) string {
	return sqlx.Rebind(db.bindType(), query)
}

func (db *DB) bindType() int {
	switch db.dialect {
	case DialectPostgres:
		return sqlx.DOLLAR
	default:
		return sqlx.QUESTION
	}
}

// Close closes the database and removes any temp file created for :memory:.
func (db *DB) Close() error {
	err := db.DB.Close()
	if db.tempPath != "" {
		os.Remove(db.tempPath)
	}
	return err
}

// --- Row types ---

type AppRow struct {
	ID                   string   `db:"id" json:"id"`
	Name                 string   `db:"name" json:"name"`
	Owner                string   `db:"owner" json:"owner"`
	AccessType           string   `db:"access_type" json:"access_type"`
	ActiveBundle         *string  `db:"active_bundle" json:"active_bundle"`
	MaxWorkersPerApp     *int     `db:"max_workers_per_app" json:"max_workers_per_app"`
	MaxSessionsPerWorker int      `db:"max_sessions_per_worker" json:"max_sessions_per_worker"`
	MemoryLimit          *string  `db:"memory_limit" json:"memory_limit"`
	CPULimit             *float64 `db:"cpu_limit" json:"cpu_limit"`
	Title                *string  `db:"title" json:"title"`
	Description          *string  `db:"description" json:"description"`
	CreatedAt            string   `db:"created_at" json:"created_at"`
	UpdatedAt            string   `db:"updated_at" json:"updated_at"`
	DeletedAt            *string  `db:"deleted_at" json:"deleted_at,omitempty"`
	PreWarmedSeats       int      `db:"pre_warmed_seats" json:"pre_warmed_seats"`
}

type BundleRow struct {
	ID         string `db:"id" json:"id"`
	AppID      string `db:"app_id" json:"app_id"`
	Status     string `db:"status" json:"status"`
	UploadedAt string `db:"uploaded_at" json:"uploaded_at"`
}

// Ping verifies the database connection is alive.
func (db *DB) Ping(ctx context.Context) error {
	_, err := db.ExecContext(ctx, "SELECT 1")
	return err
}

// --- Apps ---

func (db *DB) CreateApp(name, owner string) (*AppRow, error) {
	id := uuid.New().String()
	now := time.Now().UTC().Format(time.RFC3339)

	_, err := db.Exec(db.rebind(
		`INSERT INTO apps (id, name, owner, max_sessions_per_worker, created_at, updated_at)
		 VALUES (?, ?, ?, 1, ?, ?)`),
		id, name, owner, now, now,
	)
	if err != nil {
		return nil, fmt.Errorf("insert app: %w", err)
	}

	return db.GetApp(id)
}

func (db *DB) GetApp(id string) (*AppRow, error) {
	var app AppRow
	err := db.DB.Get(&app, db.rebind(
		`SELECT * FROM apps WHERE id = ? AND deleted_at IS NULL`), id)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &app, nil
}

func (db *DB) GetAppByName(name string) (*AppRow, error) {
	var app AppRow
	err := db.DB.Get(&app, db.rebind(
		`SELECT * FROM apps WHERE name = ? AND deleted_at IS NULL`), name)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &app, nil
}

func (db *DB) ListApps() ([]AppRow, error) {
	var apps []AppRow
	err := db.DB.Select(&apps,
		`SELECT * FROM apps WHERE deleted_at IS NULL ORDER BY created_at DESC`)
	if err != nil {
		return nil, err
	}
	return apps, nil
}

// ListAccessibleApps returns apps the caller can see: owned apps + apps
// with a user ACL grant + logged_in apps + public apps.
func (db *DB) ListAccessibleApps(sub string) ([]AppRow, error) {
	query := db.rebind(
		`SELECT DISTINCT a.*
		 FROM apps a
		 LEFT JOIN app_access aa ON a.id = aa.app_id
		 WHERE a.deleted_at IS NULL
		   AND (a.access_type IN ('public', 'logged_in')
		        OR a.owner = ?
		        OR (aa.kind = 'user' AND aa.principal = ?))
		 ORDER BY a.created_at DESC`)

	var apps []AppRow
	err := db.DB.Select(&apps, query, sub, sub)
	if err != nil {
		return nil, err
	}
	return apps, nil
}

// GetAppIncludeDeleted returns an app by ID regardless of soft-delete
// status. Used by the restore endpoint and the sweeper.
func (db *DB) GetAppIncludeDeleted(id string) (*AppRow, error) {
	var app AppRow
	err := db.DB.Get(&app, db.rebind(`SELECT * FROM apps WHERE id = ?`), id)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &app, nil
}

// SoftDeleteApp sets deleted_at on an app.
func (db *DB) SoftDeleteApp(id string) error {
	now := time.Now().UTC().Format(time.RFC3339)
	_, err := db.Exec(db.rebind(
		`UPDATE apps SET deleted_at = ?, updated_at = ? WHERE id = ? AND deleted_at IS NULL`),
		now, now, id,
	)
	return err
}

// RestoreApp clears deleted_at on a soft-deleted app.
func (db *DB) RestoreApp(id string) error {
	now := time.Now().UTC().Format(time.RFC3339)
	_, err := db.Exec(db.rebind(
		`UPDATE apps SET deleted_at = NULL, updated_at = ? WHERE id = ? AND deleted_at IS NOT NULL`),
		now, id,
	)
	return err
}

// HardDeleteApp permanently removes an app row. Used by the sweeper
// after all associated resources (bundles, files) have been cleaned up.
func (db *DB) HardDeleteApp(id string) error {
	_, err := db.Exec(db.rebind(`DELETE FROM apps WHERE id = ?`), id)
	return err
}

// ListDeletedApps returns all soft-deleted apps, newest deletion first.
func (db *DB) ListDeletedApps() ([]AppRow, error) {
	var apps []AppRow
	err := db.DB.Select(&apps,
		`SELECT * FROM apps WHERE deleted_at IS NOT NULL ORDER BY deleted_at DESC`)
	if err != nil {
		return nil, err
	}
	return apps, nil
}

// ListExpiredDeletedApps returns soft-deleted apps whose deleted_at is
// older than the given cutoff time. Used by the sweeper.
func (db *DB) ListExpiredDeletedApps(cutoff string) ([]AppRow, error) {
	var apps []AppRow
	err := db.DB.Select(&apps, db.rebind(
		`SELECT * FROM apps WHERE deleted_at IS NOT NULL AND deleted_at < ?
		 ORDER BY deleted_at ASC`),
		cutoff,
	)
	if err != nil {
		return nil, err
	}
	return apps, nil
}

// --- Bundles ---

func (db *DB) CreateBundle(id, appID string) (*BundleRow, error) {
	now := time.Now().UTC().Format(time.RFC3339)
	_, err := db.Exec(db.rebind(
		`INSERT INTO bundles (id, app_id, status, uploaded_at)
		 VALUES (?, ?, 'pending', ?)`),
		id, appID, now,
	)
	if err != nil {
		return nil, fmt.Errorf("insert bundle: %w", err)
	}
	return db.GetBundle(id)
}

func (db *DB) GetBundle(id string) (*BundleRow, error) {
	var b BundleRow
	err := db.DB.Get(&b, db.rebind(`SELECT * FROM bundles WHERE id = ?`), id)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &b, nil
}

func (db *DB) ListBundlesByApp(appID string) ([]BundleRow, error) {
	var bundles []BundleRow
	err := db.DB.Select(&bundles, db.rebind(
		`SELECT * FROM bundles WHERE app_id = ? ORDER BY uploaded_at DESC`), appID)
	if err != nil {
		return nil, err
	}
	return bundles, nil
}

func (db *DB) UpdateBundleStatus(id, status string) error {
	_, err := db.Exec(db.rebind(
		`UPDATE bundles SET status = ? WHERE id = ?`), status, id)
	return err
}

func (db *DB) SetActiveBundle(appID, bundleID string) error {
	now := time.Now().UTC().Format(time.RFC3339)
	_, err := db.Exec(db.rebind(
		`UPDATE apps SET active_bundle = ?, updated_at = ? WHERE id = ?`),
		bundleID, now, appID,
	)
	return err
}

// ActivateBundle marks a bundle as ready and sets it as the app's active
// bundle in a single transaction.
func (db *DB) ActivateBundle(appID, bundleID string) error {
	tx, err := db.Beginx()
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback()

	if _, err := tx.Exec(db.rebind(`UPDATE bundles SET status = 'ready' WHERE id = ?`), bundleID); err != nil {
		return fmt.Errorf("update bundle status: %w", err)
	}

	now := time.Now().UTC().Format(time.RFC3339)
	if _, err := tx.Exec(db.rebind(`UPDATE apps SET active_bundle = ?, updated_at = ? WHERE id = ?`), bundleID, now, appID); err != nil {
		return fmt.Errorf("set active bundle: %w", err)
	}

	return tx.Commit()
}

func (db *DB) DeleteBundle(id string) (bool, error) {
	result, err := db.Exec(db.rebind(`DELETE FROM bundles WHERE id = ?`), id)
	if err != nil {
		return false, err
	}
	n, _ := result.RowsAffected()
	return n > 0, nil
}

// --- App update ---

// AppUpdate holds optional fields for updating an app's configuration.
type AppUpdate struct {
	MaxWorkersPerApp     *int
	MaxSessionsPerWorker *int
	MemoryLimit          *string
	CPULimit             *float64
	AccessType           *string
	Title                *string
	Description          *string
	PreWarmedSeats       *int
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
	if u.PreWarmedSeats != nil {
		app.PreWarmedSeats = *u.PreWarmedSeats
	}

	now := time.Now().UTC().Format(time.RFC3339)
	_, err = db.Exec(db.rebind(
		`UPDATE apps SET
			max_workers_per_app = ?,
			max_sessions_per_worker = ?,
			memory_limit = ?,
			cpu_limit = ?,
			access_type = ?,
			title = ?,
			description = ?,
			pre_warmed_seats = ?,
			updated_at = ?
		WHERE id = ?`),
		app.MaxWorkersPerApp, app.MaxSessionsPerWorker,
		app.MemoryLimit, app.CPULimit,
		app.AccessType,
		app.Title, app.Description,
		app.PreWarmedSeats,
		now, id,
	)
	if err != nil {
		return nil, fmt.Errorf("update app: %w", err)
	}

	return db.GetApp(id)
}

// ListPreWarmedApps returns all non-deleted apps with pre_warmed_seats > 0.
func (db *DB) ListPreWarmedApps() ([]AppRow, error) {
	var apps []AppRow
	err := db.DB.Select(&apps,
		`SELECT * FROM apps WHERE pre_warmed_seats > 0 AND deleted_at IS NULL`)
	if err != nil {
		return nil, err
	}
	return apps, nil
}

// ClearActiveBundle sets active_bundle to NULL for the given app.
func (db *DB) ClearActiveBundle(appID string) error {
	now := time.Now().UTC().Format(time.RFC3339)
	_, err := db.Exec(db.rebind(
		`UPDATE apps SET active_bundle = NULL, updated_at = ? WHERE id = ?`),
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

// --- Users ---

// UserRow represents a row from the users table.
type UserRow struct {
	Sub       string `db:"sub" json:"sub"`
	Email     string `db:"email" json:"email"`
	Name      string `db:"name" json:"name"`
	Role      string `db:"role" json:"role"`
	Active    bool   `db:"active" json:"active"`
	LastLogin string `db:"last_login" json:"last_login"`
}

// UpsertUser creates or updates a user record on OIDC login.
// On conflict (existing user), updates email, name, and last_login
// but preserves role and active status set by admins.
func (db *DB) UpsertUser(sub, email, name string) (*UserRow, error) {
	now := time.Now().UTC().Format(time.RFC3339)
	_, err := db.Exec(db.rebind(
		`INSERT INTO users (sub, email, name, last_login)
		 VALUES (?, ?, ?, ?)
		 ON CONFLICT (sub) DO UPDATE SET
		     email = excluded.email,
		     name = excluded.name,
		     last_login = excluded.last_login`),
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
	_, err := db.Exec(db.rebind(
		`INSERT INTO users (sub, email, name, role, last_login)
		 VALUES (?, ?, ?, ?, ?)
		 ON CONFLICT (sub) DO UPDATE SET
		     email = excluded.email,
		     name = excluded.name,
		     last_login = excluded.last_login`),
		sub, email, name, role, now,
	)
	if err != nil {
		return nil, fmt.Errorf("upsert user with role: %w", err)
	}
	return db.GetUser(sub)
}

func (db *DB) GetUser(sub string) (*UserRow, error) {
	var u UserRow
	err := db.DB.Get(&u, db.rebind(`SELECT * FROM users WHERE sub = ?`), sub)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &u, nil
}

func (db *DB) ListUsers() ([]UserRow, error) {
	var users []UserRow
	err := db.DB.Select(&users, `SELECT * FROM users ORDER BY last_login DESC`)
	if err != nil {
		return nil, err
	}
	return users, nil
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

	active := 0
	if user.Active {
		active = 1
	}
	_, err = db.Exec(db.rebind(
		"UPDATE users SET role = ?, active = ? WHERE sub = ?"),
		user.Role, active, sub,
	)
	if err != nil {
		return nil, fmt.Errorf("update user: %w", err)
	}

	return db.GetUser(sub)
}

// --- App access (ACL) ---

// AppAccessRow represents a row from the app_access table.
type AppAccessRow struct {
	AppID     string `db:"app_id"`
	Principal string `db:"principal"`
	Kind      string `db:"kind"`
	Role      string `db:"role"`
	GrantedBy string `db:"granted_by"`
	GrantedAt string `db:"granted_at"`
}

func (db *DB) ListAppAccess(appID string) ([]AppAccessRow, error) {
	var grants []AppAccessRow
	err := db.DB.Select(&grants, db.rebind(
		`SELECT * FROM app_access WHERE app_id = ?`), appID)
	if err != nil {
		return nil, err
	}
	return grants, nil
}

func (db *DB) GrantAppAccess(appID, principal, kind, role, grantedBy string) error {
	now := time.Now().UTC().Format(time.RFC3339)
	_, err := db.Exec(db.rebind(
		`INSERT INTO app_access (app_id, principal, kind, role, granted_by, granted_at)
		 VALUES (?, ?, ?, ?, ?, ?)
		 ON CONFLICT (app_id, principal, kind)
		 DO UPDATE SET role = excluded.role,
		               granted_by = excluded.granted_by,
		               granted_at = excluded.granted_at`),
		appID, principal, kind, role, grantedBy, now,
	)
	return err
}

func (db *DB) RevokeAppAccess(appID, principal, kind string) (bool, error) {
	result, err := db.Exec(db.rebind(
		"DELETE FROM app_access WHERE app_id = ? AND principal = ? AND kind = ?"),
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
	ID        string `db:"id"`
	Name      string `db:"name"`
	CreatedAt string `db:"created_at"`
}

func (db *DB) CreateTag(name string) (*TagRow, error) {
	id := uuid.New().String()
	now := time.Now().UTC().Format(time.RFC3339)
	_, err := db.Exec(db.rebind(
		"INSERT INTO tags (id, name, created_at) VALUES (?, ?, ?)"),
		id, name, now,
	)
	if err != nil {
		return nil, err
	}
	return &TagRow{ID: id, Name: name, CreatedAt: now}, nil
}

func (db *DB) GetTag(id string) (*TagRow, error) {
	var t TagRow
	err := db.DB.Get(&t, db.rebind(`SELECT * FROM tags WHERE id = ?`), id)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &t, nil
}

func (db *DB) ListTags() ([]TagRow, error) {
	var tags []TagRow
	err := db.DB.Select(&tags, `SELECT * FROM tags ORDER BY name`)
	if err != nil {
		return nil, err
	}
	return tags, nil
}

func (db *DB) DeleteTag(id string) (bool, error) {
	result, err := db.Exec(db.rebind("DELETE FROM tags WHERE id = ?"), id)
	if err != nil {
		return false, err
	}
	n, _ := result.RowsAffected()
	return n > 0, nil
}

func (db *DB) AddAppTag(appID, tagID string) error {
	_, err := db.Exec(db.rebind(
		"INSERT INTO app_tags (app_id, tag_id) VALUES (?, ?) ON CONFLICT DO NOTHING"),
		appID, tagID,
	)
	return err
}

func (db *DB) RemoveAppTag(appID, tagID string) (bool, error) {
	result, err := db.Exec(db.rebind(
		"DELETE FROM app_tags WHERE app_id = ? AND tag_id = ?"),
		appID, tagID,
	)
	if err != nil {
		return false, err
	}
	n, _ := result.RowsAffected()
	return n > 0, nil
}

func (db *DB) ListAppTags(appID string) ([]TagRow, error) {
	var tags []TagRow
	err := db.DB.Select(&tags, db.rebind(
		`SELECT t.id, t.name, t.created_at
		 FROM tags t
		 JOIN app_tags at ON t.id = at.tag_id
		 WHERE at.app_id = ?
		 ORDER BY t.name`), appID)
	if err != nil {
		return nil, err
	}
	return tags, nil
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
	conditions := []string{"apps.deleted_at IS NULL"}
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

	// Search filter — wrap in LOWER() for cross-dialect case-insensitive matching
	if params.Search != "" {
		conditions = append(conditions,
			"(LOWER(apps.name) LIKE LOWER(?) ESCAPE '\\' OR LOWER(apps.title) LIKE LOWER(?) ESCAPE '\\' OR LOWER(apps.description) LIKE LOWER(?) ESCAPE '\\')")
		like := "%" + escapeLike(params.Search) + "%"
		args = append(args, like, like, like)
	}

	where := ""
	if len(conditions) > 0 {
		where = "WHERE " + strings.Join(conditions, " AND ")
	}

	// Count total
	var total int
	countQuery := db.rebind("SELECT COUNT(*) FROM apps " + where)
	if err := db.DB.Get(&total, countQuery, args...); err != nil {
		return nil, 0, err
	}

	// Fetch page
	query := db.rebind(fmt.Sprintf(
		`SELECT * FROM apps %s ORDER BY apps.updated_at DESC LIMIT ? OFFSET ?`,
		where,
	))
	pageArgs := append(append([]any{}, args...), params.PerPage, (params.Page-1)*params.PerPage)

	var apps []AppRow
	if err := db.DB.Select(&apps, query, pageArgs...); err != nil {
		return nil, 0, err
	}

	return apps, total, nil
}

// escapeLike escapes SQL LIKE metacharacters (%, _, \) in user input
// so they are matched literally when used with ESCAPE '\'.
func escapeLike(s string) string {
	r := strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`)
	return r.Replace(s)
}

// --- Personal Access Tokens ---

// PATRow represents a row from the personal_access_tokens table.
type PATRow struct {
	ID         string  `db:"id" json:"id"`
	UserSub    string  `db:"user_sub" json:"user_sub,omitempty"`
	Name       string  `db:"name" json:"name"`
	CreatedAt  string  `db:"created_at" json:"created_at"`
	ExpiresAt  *string `db:"expires_at" json:"expires_at"`
	LastUsedAt *string `db:"last_used_at" json:"last_used_at"`
	Revoked    bool    `db:"revoked" json:"revoked"`
}

// PATLookupResult is the result of looking up a PAT by hash,
// joined with the owning user.
type PATLookupResult struct {
	PAT  PATRow
	User UserRow
}

func (db *DB) CreatePAT(id string, tokenHash []byte, userSub, name string, expiresAt *string) (*PATRow, error) {
	now := time.Now().UTC().Format(time.RFC3339)
	_, err := db.Exec(db.rebind(
		`INSERT INTO personal_access_tokens (id, token_hash, user_sub, name, created_at, expires_at)
		 VALUES (?, ?, ?, ?, ?, ?)`),
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
	err := db.QueryRow(db.rebind(
		`SELECT p.id, p.user_sub, p.name, p.created_at, p.expires_at, p.last_used_at, p.revoked,
		        u.sub, u.email, u.name, u.role, u.active, u.last_login
		 FROM personal_access_tokens p
		 JOIN users u ON p.user_sub = u.sub
		 WHERE p.token_hash = ?`),
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
	var pats []PATRow
	err := db.DB.Select(&pats, db.rebind(
		`SELECT id, user_sub, name, created_at, expires_at, last_used_at, revoked
		 FROM personal_access_tokens
		 WHERE user_sub = ?
		 ORDER BY created_at DESC`),
		userSub,
	)
	if err != nil {
		return nil, err
	}
	return pats, nil
}

func (db *DB) RevokePAT(id, userSub string) (bool, error) {
	result, err := db.Exec(db.rebind(
		"UPDATE personal_access_tokens SET revoked = 1 WHERE id = ? AND user_sub = ?"),
		id, userSub,
	)
	if err != nil {
		return false, err
	}
	n, _ := result.RowsAffected()
	return n > 0, nil
}

func (db *DB) RevokeAllPATs(userSub string) (int64, error) {
	result, err := db.Exec(db.rebind(
		"UPDATE personal_access_tokens SET revoked = 1 WHERE user_sub = ? AND revoked = 0"),
		userSub,
	)
	if err != nil {
		return 0, err
	}
	return result.RowsAffected()
}

func (db *DB) UpdatePATLastUsed(ctx context.Context, id string) {
	now := time.Now().UTC().Format(time.RFC3339)
	_, _ = db.ExecContext(ctx, db.rebind(
		"UPDATE personal_access_tokens SET last_used_at = ? WHERE id = ?"),
		now, id,
	)
}
