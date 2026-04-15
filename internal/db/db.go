package db

import (
	"context"
	"database/sql"
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
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
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/jmoiron/sqlx"

	"github.com/cynkra/blockyard/internal/config"

	_ "modernc.org/sqlite"
)

//go:embed migrations/sqlite/*.sql
var sqliteMigrations embed.FS

//go:embed migrations/postgres/*.sql
var postgresMigrations embed.FS

//go:embed migrations/released.txt
var releasedFile embed.FS

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
	tempPath string           // non-empty when using a temp file for SQLite :memory:
	connURL  string           // original connection URL, set for PostgreSQL
	pgPool   *pgxpool.Pool    // non-nil when dialect = PostgreSQL; closed separately from *sql.DB
	creds    *pgCredsProvider // non-nil when dialect = PostgreSQL; holds current user/pass and optional rotator
}

// openOptions collects optional Open behavior. Currently just the
// credential rotator used by the Postgres auth-failure fallback.
type openOptions struct {
	rotator CredsRotator
}

// OpenOption configures Open.
type OpenOption func(*openOptions)

// WithCredsRotator installs a CredsRotator that the Postgres pool
// uses to mint fresh credentials on auth failure. When set, both
// the initial connect and mid-run Ping paths fall through to the
// rotator if Postgres returns an auth error (SQLSTATE 28P01 or
// "role ... does not exist"). No-op for SQLite.
func WithCredsRotator(r CredsRotator) OpenOption {
	return func(o *openOptions) { o.rotator = r }
}

// Open opens a database connection based on the config.
func Open(cfg config.DatabaseConfig, opts ...OpenOption) (*DB, error) {
	var options openOptions
	for _, opt := range opts {
		opt(&options)
	}

	switch cfg.Driver {
	case "sqlite":
		return openSQLite(cfg.Path)
	case "postgres":
		return openPostgres(cfg.URL, options)
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

func openPostgres(dsn string, options openOptions) (*DB, error) {
	user, pass, err := parseDSNUser(dsn)
	if err != nil {
		return nil, err
	}
	provider := newPgCredsProvider(user, pass, options.rotator)

	// pgxpool gives us a BeforeConnect hook that fires on every new
	// low-level connection — our hook injects the current creds from
	// the provider, so rotating the provider's creds transparently
	// updates every future connection without a pool rebuild.
	poolCfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		return nil, fmt.Errorf("parse postgres url: %w", err)
	}
	// Pool defaults — override via connection string parameters
	// (e.g. ?pool_max_conns=20) if needed.
	if poolCfg.MaxConns == 0 {
		poolCfg.MaxConns = 25
	}
	if poolCfg.MaxConnLifetime == 0 {
		poolCfg.MaxConnLifetime = 5 * time.Minute
	}
	poolCfg.BeforeConnect = func(_ context.Context, cc *pgx.ConnConfig) error {
		// Only override when the provider holds real creds — otherwise
		// let pgx keep whatever it parsed from the DSN so a URL without
		// userinfo (e.g. one relying on PGPASSFILE / PGUSER env) still
		// connects.
		if u, p := provider.current(); u != "" {
			cc.User = u
			cc.Password = p
		}
		return nil
	}

	pool, err := pgxpool.NewWithConfig(context.Background(), poolCfg)
	if err != nil {
		return nil, fmt.Errorf("open postgres: %w", err)
	}

	sqlDB := stdlib.OpenDBFromPool(pool)
	db := sqlx.NewDb(sqlDB, "pgx")
	d := &DB{
		DB:      db,
		dialect: DialectPostgres,
		connURL: dsn,
		pgPool:  pool,
		creds:   provider,
	}

	// Initial connect: Ping first; on auth failure, rotate creds and
	// retry once. This covers the hot-restart case where the env
	// creds were revoked when the provisioner token died.
	if err := d.pingWithRotate(context.Background()); err != nil {
		db.Close()
		pool.Close()
		return nil, fmt.Errorf("ping postgres: %w", err)
	}

	if err := d.runMigrations(); err != nil {
		db.Close()
		pool.Close()
		return nil, err
	}
	return d, nil
}

// pingWithRotate attempts a Ping. On PostgreSQL auth failure, if a
// CredsRotator is configured, creds are rotated and Ping is retried
// once. On success after rotation, the pool is reset so any prior
// conns opened with stale creds are evicted.
func (d *DB) pingWithRotate(ctx context.Context) error {
	err := d.DB.PingContext(ctx)
	if err == nil {
		return nil
	}
	if !isPostgresAuthError(err) || d.creds == nil || !d.creds.hasRotator() {
		return err
	}
	slog.Warn("postgres auth failed, rotating credentials", "error", err)
	if rErr := d.creds.rotate(ctx); rErr != nil {
		return fmt.Errorf("%w; rotate failed: %v", err, rErr)
	}
	// Reset the pool so any half-open conns using stale creds are
	// discarded; next Acquire triggers BeforeConnect with fresh creds.
	d.pgPool.Reset()
	return d.DB.PingContext(ctx)
}

// isPostgresAuthError reports whether err is a PostgreSQL auth
// failure that should trigger credential rotation. Matches SQLSTATE
// 28P01 (invalid_password) and the common "role ... does not exist"
// variant surfaced when a dynamic role has been revoked.
func isPostgresAuthError(err error) bool {
	if err == nil {
		return false
	}
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		// 28P01 invalid_password is the primary signal; 28000
		// invalid_authorization_specification covers broader auth
		// rejection shapes OpenBao occasionally produces.
		if pgErr.Code == "28P01" || pgErr.Code == "28000" {
			return true
		}
	}
	// When the connect itself fails (role dropped), pgx wraps the
	// server message as a plain error — match by the distinctive
	// substring rather than the SQLSTATE.
	msg := err.Error()
	if strings.Contains(msg, "SQLSTATE 28P01") || strings.Contains(msg, "SQLSTATE 28000") {
		return true
	}
	if strings.Contains(msg, "role ") && strings.Contains(msg, "does not exist") {
		return true
	}
	return false
}

func (db *DB) runMigrations() error {
	m, cleanup, err := db.newMigrator()
	if err != nil {
		return fmt.Errorf("create migrator: %w", err)
	}
	defer cleanup()
	if err := m.Up(); err != nil && err != migrate.ErrNoChange {
		return fmt.Errorf("run migrations: %w", err)
	}
	return nil
}

// newMigrator builds a migrate.Migrate for the active dialect and
// returns a cleanup closure that releases any acquired resources
// (notably, the dedicated *sql.Conn the Postgres driver hijacks for
// advisory locking). Callers MUST defer the cleanup.
func (db *DB) newMigrator() (*migrate.Migrate, func(), error) {
	driver, cleanup, err := db.migrateDriver()
	if err != nil {
		return nil, nil, fmt.Errorf("migration driver: %w", err)
	}

	var fsys fs.FS
	switch db.dialect {
	case DialectSQLite:
		fsys, err = fs.Sub(sqliteMigrations, "migrations/sqlite")
	case DialectPostgres:
		fsys, err = fs.Sub(postgresMigrations, "migrations/postgres")
	}
	if err != nil {
		cleanup()
		return nil, nil, fmt.Errorf("migration fs: %w", err)
	}

	source, err := iofs.New(fsys, ".")
	if err != nil {
		cleanup()
		return nil, nil, fmt.Errorf("migration source: %w", err)
	}

	m, err := migrate.NewWithInstance("iofs", source, db.driverName(), driver)
	if err != nil {
		cleanup()
		return nil, nil, err
	}
	return m, cleanup, nil
}

// migrateDriver returns a migration driver and a cleanup closure that
// must be deferred by the caller. For Postgres, the driver hijacks a
// dedicated *sql.Conn for lock management; the cleanup releases it
// back to the pgxpool so pgxpool.Close() won't block forever at
// shutdown waiting for it. (migratepostgres.WithInstance would hold
// the conn until migrate.Close(), which also closes the *sql.DB —
// unsafe with the shared pool we install.)
func (db *DB) migrateDriver() (migratedb.Driver, func(), error) {
	switch db.dialect {
	case DialectPostgres:
		ctx := context.Background()
		conn, err := db.DB.DB.Conn(ctx)
		if err != nil {
			return nil, nil, err
		}
		drv, err := migratepostgres.WithConnection(ctx, conn, &migratepostgres.Config{})
		if err != nil {
			conn.Close()
			return nil, nil, err
		}
		return drv, func() { _ = conn.Close() }, nil
	default:
		drv, err := migratesqlite.WithInstance(db.DB.DB, &migratesqlite.Config{
			NoTxWrap: true,
		})
		if err != nil {
			return nil, nil, err
		}
		// SQLite's WithInstance does not hijack a conn; nothing to release.
		return drv, func() {}, nil
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
// For PostgreSQL, the underlying pgxpool is closed separately — stdlib.
// OpenDBFromPool docs explicitly note that closing the *sql.DB does not
// close the pool.
func (db *DB) Close() error {
	err := db.DB.Close()
	if db.pgPool != nil {
		db.pgPool.Close()
	}
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
	PreWarmedSessions    int      `db:"pre_warmed_sessions" json:"pre_warmed_sessions"`
	RefreshSchedule      string   `db:"refresh_schedule" json:"refresh_schedule"`
	LastRefreshAt        *string  `db:"last_refresh_at" json:"last_refresh_at,omitempty"`
	Enabled              bool     `db:"enabled" json:"enabled"`
	Image                string   `db:"image" json:"image"`
	Runtime              string   `db:"runtime" json:"runtime"`
}

type BundleRow struct {
	ID         string  `db:"id" json:"id"`
	AppID      string  `db:"app_id" json:"app_id"`
	Status     string  `db:"status" json:"status"`
	UploadedAt string  `db:"uploaded_at" json:"uploaded_at"`
	DeployedBy *string `db:"deployed_by" json:"deployed_by"`
	DeployedAt *string `db:"deployed_at" json:"deployed_at"`
	Pinned     bool    `db:"pinned" json:"pinned"`
}

// SessionRow represents a row from the sessions table.
type SessionRow struct {
	ID        string  `db:"id" json:"id"`
	AppID     string  `db:"app_id" json:"app_id"`
	WorkerID  string  `db:"worker_id" json:"worker_id"`
	UserSub   *string `db:"user_sub" json:"user_sub"`
	StartedAt string  `db:"started_at" json:"started_at"`
	EndedAt   *string `db:"ended_at" json:"ended_at"`
	Status    string  `db:"status" json:"status"`
}

// DeploymentRow represents a bundle deployment joined with app and user info.
type DeploymentRow struct {
	AppID          string  `db:"app_id" json:"app_id"`
	AppName        string  `db:"app_name" json:"app_name"`
	BundleID       string  `db:"bundle_id" json:"bundle_id"`
	DeployedBy     *string `db:"deployed_by" json:"deployed_by"`
	DeployedByName *string `db:"deployed_by_name" json:"deployed_by_name"`
	DeployedAt     *string `db:"deployed_at" json:"deployed_at"`
	Status         string  `db:"status" json:"status"`
}

// CatalogRow extends AppRow with per-app relation and tags for list responses.
type CatalogRow struct {
	AppRow
	Relation string `db:"relation" json:"relation"`
	Tags     string `db:"tags" json:"tags"` // comma-separated tag names
}

// AccessGrantWithName extends AppAccessRow with the user's display name.
type AccessGrantWithName struct {
	AppAccessRow
	DisplayName string `db:"display_name" json:"display_name"`
}

// SessionListOpts holds query parameters for listing sessions.
type SessionListOpts struct {
	UserSub string
	Status  string
	Limit   int
}

// DeploymentListOpts holds query parameters for listing deployments.
type DeploymentListOpts struct {
	CallerSub  string
	CallerRole string
	Search     string
	Status     string
	Sort       string // column key, e.g. "app_name", "deployed_by", "date", "status"
	SortDir    string // "asc" or "desc"
	Page       int
	PerPage    int
}

// Ping verifies the database connection is alive. For PostgreSQL, a
// ping that fails with an auth error (SQLSTATE 28P01 or a role-dropped
// message) triggers a credential rotation if a CredsRotator is
// configured — this lets the health poller self-heal when the dynamic
// role has been revoked or the lease has expired.
func (db *DB) Ping(ctx context.Context) error {
	_, err := db.ExecContext(ctx, "SELECT 1")
	if err == nil {
		return nil
	}
	if db.dialect != DialectPostgres || !isPostgresAuthError(err) {
		return err
	}
	if db.creds == nil || !db.creds.hasRotator() {
		return err
	}
	slog.Warn("postgres ping auth failed, rotating credentials", "error", err)
	if rErr := db.creds.rotate(ctx); rErr != nil {
		return fmt.Errorf("%w; rotate failed: %v", err, rErr)
	}
	db.pgPool.Reset()
	_, err = db.ExecContext(ctx, "SELECT 1")
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

// ListAppsWithRefreshSchedule returns non-deleted apps that have a
// non-empty refresh_schedule. Used by the refresh scheduler.
func (db *DB) ListAppsWithRefreshSchedule() ([]AppRow, error) {
	var apps []AppRow
	err := db.DB.Select(&apps,
		`SELECT * FROM apps WHERE deleted_at IS NULL AND refresh_schedule != '' ORDER BY created_at`)
	if err != nil {
		return nil, err
	}
	return apps, nil
}

// UpdateLastRefresh records the timestamp of the last refresh for an app.
func (db *DB) UpdateLastRefresh(appID string, t time.Time) error {
	_, err := db.Exec(db.rebind(
		`UPDATE apps SET last_refresh_at = ? WHERE id = ?`),
		t.UTC().Format(time.RFC3339), appID)
	return err
}

// --- Bundles ---

func (db *DB) CreateBundle(id, appID, deployedBy string, pinned bool) (*BundleRow, error) {
	now := time.Now().UTC().Format(time.RFC3339)
	pinnedInt := 0
	if pinned {
		pinnedInt = 1
	}
	var deployedByVal *string
	if deployedBy != "" {
		deployedByVal = &deployedBy
	}
	_, err := db.Exec(db.rebind(
		`INSERT INTO bundles (id, app_id, status, uploaded_at, deployed_by, pinned)
		 VALUES (?, ?, 'pending', ?, ?, ?)`),
		id, appID, now, deployedByVal, pinnedInt,
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

// ActivateBundle marks a bundle as ready, sets deployed_at, and sets it as
// the app's active bundle in a single transaction.
func (db *DB) ActivateBundle(appID, bundleID string) error {
	tx, err := db.Beginx()
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback()

	now := time.Now().UTC().Format(time.RFC3339)
	if _, err := tx.Exec(db.rebind(`UPDATE bundles SET status = 'ready', deployed_at = ? WHERE id = ?`), now, bundleID); err != nil {
		return fmt.Errorf("update bundle status: %w", err)
	}

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

// SetBundleDeployed updates the deployed_by and deployed_at fields on a bundle.
// Used during rollbacks to record who triggered the rollback and when.
func (db *DB) SetBundleDeployed(bundleID, deployedBy string) error {
	now := time.Now().UTC().Format(time.RFC3339)
	_, err := db.Exec(db.rebind(
		`UPDATE bundles SET deployed_by = ?, deployed_at = ? WHERE id = ?`),
		deployedBy, now, bundleID,
	)
	return err
}

// --- App update ---

// AppUpdate holds optional fields for updating an app's configuration.
type AppUpdate struct {
	Name                 *string
	MaxWorkersPerApp     *int
	MaxSessionsPerWorker *int
	MemoryLimit          *string
	CPULimit             *float64
	AccessType           *string
	Title                *string
	Description          *string
	PreWarmedSessions    *int
	RefreshSchedule      *string
	Image                *string
	Runtime              *string
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
	if u.PreWarmedSessions != nil {
		app.PreWarmedSessions = *u.PreWarmedSessions
	}
	if u.RefreshSchedule != nil {
		app.RefreshSchedule = *u.RefreshSchedule
	}
	if u.Image != nil {
		app.Image = *u.Image
	}
	if u.Runtime != nil {
		app.Runtime = *u.Runtime
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
			pre_warmed_sessions = ?,
			refresh_schedule = ?,
			image = ?,
			runtime = ?,
			updated_at = ?
		WHERE id = ?`),
		app.MaxWorkersPerApp, app.MaxSessionsPerWorker,
		app.MemoryLimit, app.CPULimit,
		app.AccessType,
		app.Title, app.Description,
		app.PreWarmedSessions,
		app.RefreshSchedule,
		app.Image, app.Runtime,
		now, id,
	)
	if err != nil {
		return nil, fmt.Errorf("update app: %w", err)
	}

	return db.GetApp(id)
}

// DataMountRow represents a row from the app_data_mounts table.
type DataMountRow struct {
	AppID    string `db:"app_id" json:"app_id"`
	Source   string `db:"source" json:"source"`
	Target   string `db:"target" json:"target"`
	ReadOnly bool   `db:"readonly" json:"readonly"`
}

// ListAppDataMounts returns all data mounts for an app.
func (db *DB) ListAppDataMounts(appID string) ([]DataMountRow, error) {
	var mounts []DataMountRow
	err := db.DB.Select(&mounts,
		db.rebind(`SELECT * FROM app_data_mounts WHERE app_id = ?`),
		appID)
	if err != nil {
		return nil, fmt.Errorf("list data mounts: %w", err)
	}
	return mounts, nil
}

// SetAppDataMounts replaces all data mounts for an app.
func (db *DB) SetAppDataMounts(appID string, mounts []DataMountRow) error {
	tx, err := db.DB.Beginx()
	if err != nil {
		return err
	}
	defer tx.Rollback() //nolint:errcheck // rollback after commit is a no-op

	_, err = tx.Exec(db.rebind(
		`DELETE FROM app_data_mounts WHERE app_id = ?`), appID)
	if err != nil {
		return fmt.Errorf("clear data mounts: %w", err)
	}

	for _, m := range mounts {
		_, err = tx.Exec(db.rebind(
			`INSERT INTO app_data_mounts (app_id, source, target, readonly)
			 VALUES (?, ?, ?, ?)`),
			appID, m.Source, m.Target, m.ReadOnly)
		if err != nil {
			return fmt.Errorf("insert data mount: %w", err)
		}
	}

	return tx.Commit()
}

// ListPreWarmedApps returns all non-deleted apps with pre_warmed_sessions > 0.
func (db *DB) ListPreWarmedApps() ([]AppRow, error) {
	var apps []AppRow
	err := db.DB.Select(&apps,
		`SELECT * FROM apps WHERE pre_warmed_sessions > 0 AND deleted_at IS NULL`)
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

// ListUsersOpts holds filter and pagination options for ListUsers.
// Zero value returns all users, newest login first.
type ListUsersOpts struct {
	Search       string // case-insensitive substring match on name, email, sub
	Role         string // "admin", "publisher", "viewer", or "" for any
	ActiveFilter string // "active", "inactive", or "" for any
	Sort         string // "last_login", "name", "email", "role"
	SortDir      string // "asc" or "desc"
	Page         int    // 1-based; ignored if PerPage <= 0
	PerPage      int    // 0 means no limit
}

func (db *DB) ListUsers(opts ListUsersOpts) ([]UserRow, int, error) {
	conditions := []string{}
	var args []any

	if opts.Search != "" {
		conditions = append(conditions,
			"(LOWER(name) LIKE LOWER(?) OR LOWER(email) LIKE LOWER(?) OR LOWER(sub) LIKE LOWER(?))")
		like := "%" + escapeLike(opts.Search) + "%"
		args = append(args, like, like, like)
	}
	if opts.Role != "" {
		conditions = append(conditions, "role = ?")
		args = append(args, opts.Role)
	}
	switch opts.ActiveFilter {
	case "active":
		conditions = append(conditions, "active = 1")
	case "inactive":
		conditions = append(conditions, "active = 0")
	}

	where := ""
	if len(conditions) > 0 {
		where = "WHERE " + strings.Join(conditions, " AND ")
	}

	var total int
	countQuery := db.rebind(fmt.Sprintf("SELECT COUNT(*) FROM users %s", where))
	if err := db.DB.Get(&total, countQuery, args...); err != nil {
		return nil, 0, err
	}

	orderBy := "last_login DESC"
	userSortCols := map[string]string{
		"last_login": "last_login",
		"name":       "name",
		"email":      "email",
		"role":       "role",
	}
	if col, ok := userSortCols[opts.Sort]; ok {
		dir := "ASC"
		if strings.EqualFold(opts.SortDir, "desc") {
			dir = "DESC"
		}
		orderBy = col + " " + dir
	}

	query := fmt.Sprintf("SELECT * FROM users %s ORDER BY %s", where, orderBy)
	if opts.PerPage > 0 {
		page := opts.Page
		if page < 1 {
			page = 1
		}
		offset := (page - 1) * opts.PerPage
		query += " LIMIT ? OFFSET ?"
		args = append(args, opts.PerPage, offset)
	}

	var users []UserRow
	if err := db.DB.Select(&users, db.rebind(query), args...); err != nil {
		return nil, 0, err
	}
	return users, total, nil
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
	Tag        string   // deprecated: use Tags
	Tags       []string // multi-tag filter
	TagMode    string   // "and" (default) or "or"
	Search     string
	Sort       string // column key, e.g. "name", "status", "last_deployed"
	SortDir    string // "asc" or "desc"
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

	// Tag filter — support both legacy single Tag and multi-tag Tags
	tags := params.Tags
	if len(tags) == 0 && params.Tag != "" {
		tags = []string{params.Tag}
	}
	if len(tags) > 0 {
		tagMode := strings.ToLower(params.TagMode)
		if tagMode != "or" {
			tagMode = "and"
		}
		placeholders := make([]string, len(tags))
		for i, t := range tags {
			placeholders[i] = "?"
			args = append(args, t)
		}
		inClause := strings.Join(placeholders, ", ")
		if tagMode == "and" {
			conditions = append(conditions, fmt.Sprintf(
				`apps.id IN (
					SELECT at2.app_id FROM app_tags at2
					JOIN tags t2 ON at2.tag_id = t2.id
					WHERE t2.name IN (%s)
					GROUP BY at2.app_id
					HAVING COUNT(DISTINCT t2.id) = ?
				)`, inClause))
			args = append(args, len(tags))
		} else {
			conditions = append(conditions, fmt.Sprintf(
				`apps.id IN (
					SELECT at2.app_id FROM app_tags at2
					JOIN tags t2 ON at2.tag_id = t2.id
					WHERE t2.name IN (%s)
				)`, inClause))
		}
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

	// Sort
	orderBy := "apps.updated_at DESC"
	catalogSortCols := map[string]string{
		"name":          "apps.name",
		"status":        "apps.enabled",
		"last_deployed": "apps.updated_at",
	}
	if col, ok := catalogSortCols[params.Sort]; ok {
		dir := "ASC"
		if strings.EqualFold(params.SortDir, "desc") {
			dir = "DESC"
		}
		orderBy = col + " " + dir + ", apps.id DESC"
	}

	// Fetch page
	query := db.rebind(fmt.Sprintf(
		`SELECT * FROM apps %s ORDER BY %s LIMIT ? OFFSET ?`,
		where, orderBy,
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

// PATHashExists returns true if a PAT with the given hash exists (any state).
func (db *DB) PATHashExists(tokenHash []byte) bool {
	var count int
	err := db.QueryRow(db.rebind(
		`SELECT COUNT(*) FROM personal_access_tokens WHERE token_hash = ?`), tokenHash).Scan(&count)
	return err == nil && count > 0
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
		 WHERE user_sub = ? AND revoked = 0
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

// --- Sessions ---

// CreateSession inserts a new session record.
func (db *DB) CreateSession(id, appID, workerID, userSub string) error {
	now := time.Now().UTC().Format(time.RFC3339)
	var sub *string
	if userSub != "" {
		sub = &userSub
	}
	_, err := db.Exec(db.rebind(
		`INSERT INTO sessions (id, app_id, worker_id, user_sub, started_at, status)
		 VALUES (?, ?, ?, ?, ?, 'active')`),
		id, appID, workerID, sub, now,
	)
	return err
}

// EndSession marks a session as ended with the given status.
func (db *DB) EndSession(id, status string) error {
	now := time.Now().UTC().Format(time.RFC3339)
	_, err := db.Exec(db.rebind(
		`UPDATE sessions SET status = ?, ended_at = ? WHERE id = ? AND status = 'active'`),
		status, now, id,
	)
	return err
}

// CrashWorkerSessions marks all active sessions for a worker as crashed.
func (db *DB) CrashWorkerSessions(workerID string) error {
	now := time.Now().UTC().Format(time.RFC3339)
	_, err := db.Exec(db.rebind(
		`UPDATE sessions SET status = 'crashed', ended_at = ? WHERE worker_id = ? AND status = 'active'`),
		now, workerID,
	)
	return err
}

// EndWorkerSessions marks all active sessions for a worker as ended.
func (db *DB) EndWorkerSessions(workerID string) error {
	now := time.Now().UTC().Format(time.RFC3339)
	_, err := db.Exec(db.rebind(
		`UPDATE sessions SET status = 'ended', ended_at = ? WHERE worker_id = ? AND status = 'active'`),
		now, workerID,
	)
	return err
}

// EndAppSessions marks all active sessions for an app as ended.
func (db *DB) EndAppSessions(appID string) error {
	now := time.Now().UTC().Format(time.RFC3339)
	_, err := db.Exec(db.rebind(
		`UPDATE sessions SET status = 'ended', ended_at = ? WHERE app_id = ? AND status = 'active'`),
		now, appID,
	)
	return err
}

// ListSessions returns sessions for an app, most recent first.
func (db *DB) ListSessions(appID string, opts SessionListOpts) ([]SessionRow, error) {
	conditions := []string{"app_id = ?"}
	args := []any{appID}

	if opts.UserSub != "" {
		conditions = append(conditions, "user_sub = ?")
		args = append(args, opts.UserSub)
	}
	if opts.Status != "" {
		conditions = append(conditions, "status = ?")
		args = append(args, opts.Status)
	}

	limit := opts.Limit
	if limit <= 0 || limit > 200 {
		limit = 50
	}

	query := db.rebind(fmt.Sprintf(
		`SELECT * FROM sessions WHERE %s ORDER BY started_at DESC LIMIT ?`,
		strings.Join(conditions, " AND "),
	))
	args = append(args, limit)

	var sessions []SessionRow
	if err := db.DB.Select(&sessions, query, args...); err != nil {
		return nil, err
	}
	return sessions, nil
}

// GetSession returns a single session by ID.
func (db *DB) GetSession(id string) (*SessionRow, error) {
	var s SessionRow
	err := db.DB.Get(&s, db.rebind(`SELECT * FROM sessions WHERE id = ?`), id)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &s, nil
}

// --- Activity metrics (derived from sessions) ---

// CountSessions returns total session count for an app.
func (db *DB) CountSessions(appID string) (int, error) {
	var n int
	err := db.DB.Get(&n, db.rebind(`SELECT COUNT(*) FROM sessions WHERE app_id = ?`), appID)
	return n, err
}

// CountRecentSessions returns session count since the given time.
func (db *DB) CountRecentSessions(appID string, since time.Time) (int, error) {
	var n int
	err := db.DB.Get(&n, db.rebind(
		`SELECT COUNT(*) FROM sessions WHERE app_id = ? AND started_at >= ?`),
		appID, since.UTC().Format(time.RFC3339))
	return n, err
}

// CountUniqueVisitors returns distinct user_sub count for an app.
func (db *DB) CountUniqueVisitors(appID string) (int, error) {
	var n int
	err := db.DB.Get(&n, db.rebind(
		`SELECT COUNT(DISTINCT user_sub) FROM sessions WHERE app_id = ? AND user_sub IS NOT NULL`),
		appID)
	return n, err
}

// --- Enable/Disable ---

// SetAppEnabled sets the enabled flag on an app.
func (db *DB) SetAppEnabled(appID string, enabled bool) error {
	val := 0
	if enabled {
		val = 1
	}
	now := time.Now().UTC().Format(time.RFC3339)
	_, err := db.Exec(db.rebind(
		`UPDATE apps SET enabled = ?, updated_at = ? WHERE id = ?`),
		val, now, appID,
	)
	return err
}

// --- Hard delete (purge) ---

// PurgeApp permanently removes an app and all associated data in a
// single transaction. The caller must verify the app is soft-deleted.
func (db *DB) PurgeApp(appID string) error {
	tx, err := db.Beginx()
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback()

	// Order matters for foreign key constraints.
	// Clear active_bundle reference first.
	if _, err := tx.Exec(db.rebind(`UPDATE apps SET active_bundle = NULL WHERE id = ?`), appID); err != nil {
		return fmt.Errorf("clear active bundle: %w", err)
	}
	if _, err := tx.Exec(db.rebind(`DELETE FROM sessions WHERE app_id = ?`), appID); err != nil {
		return fmt.Errorf("delete sessions: %w", err)
	}
	if _, err := tx.Exec(db.rebind(`DELETE FROM app_tags WHERE app_id = ?`), appID); err != nil {
		return fmt.Errorf("delete app tags: %w", err)
	}
	if _, err := tx.Exec(db.rebind(`DELETE FROM app_access WHERE app_id = ?`), appID); err != nil {
		return fmt.Errorf("delete access grants: %w", err)
	}
	if _, err := tx.Exec(db.rebind(`DELETE FROM bundles WHERE app_id = ?`), appID); err != nil {
		return fmt.Errorf("delete bundles: %w", err)
	}
	if _, err := tx.Exec(db.rebind(`DELETE FROM apps WHERE id = ?`), appID); err != nil {
		return fmt.Errorf("delete app: %w", err)
	}

	return tx.Commit()
}

// --- Deployments ---

// ListDeployments returns a cross-app deployment listing with pagination.
// Results are filtered to apps where the caller has collaborator+ access.
func (db *DB) ListDeployments(opts DeploymentListOpts) ([]DeploymentRow, int, error) {
	conditions := []string{"b.deployed_at IS NOT NULL", "apps.deleted_at IS NULL"}
	var args []any

	// Access control
	if opts.CallerRole != "admin" && opts.CallerSub != "" {
		accessFilter := `(
			apps.owner = ?
			OR EXISTS (
				SELECT 1 FROM app_access
				WHERE app_access.app_id = apps.id
				AND app_access.kind = 'user'
				AND app_access.principal = ?
				AND app_access.role = 'collaborator'
			)
		)`
		conditions = append(conditions, accessFilter)
		args = append(args, opts.CallerSub, opts.CallerSub)
	}

	if opts.Search != "" {
		conditions = append(conditions, "LOWER(apps.name) LIKE LOWER(?)")
		args = append(args, "%"+escapeLike(opts.Search)+"%")
	}
	if opts.Status != "" {
		conditions = append(conditions, "b.status = ?")
		args = append(args, opts.Status)
	}

	where := "WHERE " + strings.Join(conditions, " AND ")

	// Count total
	var total int
	countQuery := db.rebind(fmt.Sprintf(
		`SELECT COUNT(*) FROM bundles b JOIN apps ON b.app_id = apps.id %s`, where))
	if err := db.DB.Get(&total, countQuery, args...); err != nil {
		return nil, 0, err
	}

	perPage := opts.PerPage
	if perPage <= 0 || perPage > 100 {
		perPage = 25
	}
	page := opts.Page
	if page < 1 {
		page = 1
	}

	// Sort
	orderBy := "b.deployed_at DESC"
	deploySortCols := map[string]string{
		"app_name":    "apps.name",
		"deployed_by": "u.name",
		"date":        "b.deployed_at",
		"status":      "b.status",
	}
	if col, ok := deploySortCols[opts.Sort]; ok {
		dir := "ASC"
		if strings.EqualFold(opts.SortDir, "desc") {
			dir = "DESC"
		}
		orderBy = col + " " + dir + ", b.id DESC"
	}

	query := db.rebind(fmt.Sprintf(
		`SELECT b.app_id, apps.name AS app_name, b.id AS bundle_id,
		        b.deployed_by, u.name AS deployed_by_name,
		        b.deployed_at, b.status
		 FROM bundles b
		 JOIN apps ON b.app_id = apps.id
		 LEFT JOIN users u ON b.deployed_by = u.sub
		 %s
		 ORDER BY %s
		 LIMIT ? OFFSET ?`, where, orderBy))
	pageArgs := append(append([]any{}, args...), perPage, (page-1)*perPage)

	var rows []DeploymentRow
	if err := db.DB.Select(&rows, query, pageArgs...); err != nil {
		return nil, 0, err
	}
	return rows, total, nil
}

// --- Consolidated app listing ---

// ListCatalogWithRelation returns apps visible to the caller with per-app
// relation and tags computed in the query. Replaces N+1 ListAppTags calls.
func (db *DB) ListCatalogWithRelation(params CatalogParams) ([]CatalogRow, int, error) {
	conditions := []string{"apps.deleted_at IS NULL"}
	var args []any

	// Access control filter
	if params.CallerRole == "admin" {
		// Admin sees everything
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
		conditions = append(conditions, "apps.access_type = 'public'")
	}

	// Tag filter — support both legacy single Tag and multi-tag Tags
	relTags := params.Tags
	if len(relTags) == 0 && params.Tag != "" {
		relTags = []string{params.Tag}
	}
	if len(relTags) > 0 {
		tagMode := strings.ToLower(params.TagMode)
		if tagMode != "or" {
			tagMode = "and"
		}
		placeholders := make([]string, len(relTags))
		for i, t := range relTags {
			placeholders[i] = "?"
			args = append(args, t)
		}
		inClause := strings.Join(placeholders, ", ")
		if tagMode == "and" {
			conditions = append(conditions, fmt.Sprintf(
				`apps.id IN (
					SELECT at2.app_id FROM app_tags at2
					JOIN tags t2 ON at2.tag_id = t2.id
					WHERE t2.name IN (%s)
					GROUP BY at2.app_id
					HAVING COUNT(DISTINCT t2.id) = ?
				)`, inClause))
			args = append(args, len(relTags))
		} else {
			conditions = append(conditions, fmt.Sprintf(
				`apps.id IN (
					SELECT at2.app_id FROM app_tags at2
					JOIN tags t2 ON at2.tag_id = t2.id
					WHERE t2.name IN (%s)
				)`, inClause))
		}
	}

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

	// Build relation CASE expression
	relationExpr := "'viewer'" // default for unauthenticated
	if params.CallerRole == "admin" {
		relationExpr = "'admin'"
	} else if params.CallerSub != "" {
		relationExpr = `CASE
			WHEN apps.owner = ? THEN 'owner'
			WHEN EXISTS (
				SELECT 1 FROM app_access
				WHERE app_access.app_id = apps.id
				AND app_access.kind = 'user'
				AND app_access.principal = ?
				AND app_access.role = 'collaborator'
			) THEN 'collaborator'
			ELSE 'viewer'
		END`
		args = append(args, params.CallerSub, params.CallerSub)
	}

	// Tags subquery — uses GROUP_CONCAT for SQLite, STRING_AGG for Postgres
	var tagsAgg string
	switch db.dialect {
	case DialectPostgres:
		tagsAgg = `COALESCE((SELECT STRING_AGG(t.name, ',' ORDER BY t.name)
			FROM app_tags at JOIN tags t ON t.id = at.tag_id
			WHERE at.app_id = apps.id), '')`
	default:
		tagsAgg = `COALESCE((SELECT GROUP_CONCAT(t.name, ',')
			FROM (SELECT t2.name FROM app_tags at2 JOIN tags t2 ON t2.id = at2.tag_id
			      WHERE at2.app_id = apps.id ORDER BY t2.name) t), '')`
	}

	perPage := params.PerPage
	if perPage <= 0 || perPage > 100 {
		perPage = 25
	}
	page := params.Page
	if page < 1 {
		page = 1
	}

	// Sort
	orderBy := "apps.updated_at DESC"
	catalogSortCols := map[string]string{
		"name":          "apps.name",
		"status":        "apps.enabled",
		"last_deployed": "apps.updated_at",
	}
	if col, ok := catalogSortCols[params.Sort]; ok {
		dir := "ASC"
		if strings.EqualFold(params.SortDir, "desc") {
			dir = "DESC"
		}
		orderBy = col + " " + dir + ", apps.id DESC"
	}

	query := db.rebind(fmt.Sprintf(
		`SELECT apps.*, %s AS relation, %s AS tags
		 FROM apps %s
		 ORDER BY %s
		 LIMIT ? OFFSET ?`,
		relationExpr, tagsAgg, where, orderBy,
	))
	pageArgs := append(append([]any{}, args...), perPage, (page-1)*perPage)

	var rows []CatalogRow
	if err := db.DB.Select(&rows, query, pageArgs...); err != nil {
		return nil, 0, err
	}
	return rows, total, nil
}

// --- Collaborator display names ---

// ListAppAccessWithNames returns access grants joined with user display names.
func (db *DB) ListAppAccessWithNames(appID string) ([]AccessGrantWithName, error) {
	var grants []AccessGrantWithName
	err := db.DB.Select(&grants, db.rebind(
		`SELECT aa.app_id, aa.principal, aa.kind, aa.role, aa.granted_by, aa.granted_at,
		        COALESCE(u.name, aa.principal) AS display_name
		 FROM app_access aa
		 LEFT JOIN users u ON aa.principal = u.sub
		 WHERE aa.app_id = ?`), appID)
	if err != nil {
		return nil, err
	}
	return grants, nil
}

// --- App lookup variants ---

// GetAppByNameIncludeDeleted returns an app by name regardless of soft-delete status.
func (db *DB) GetAppByNameIncludeDeleted(name string) (*AppRow, error) {
	var app AppRow
	err := db.DB.Get(&app, db.rebind(
		`SELECT * FROM apps WHERE name = ?`), name)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &app, nil
}

// --- App aliases (renaming) ---

// RenameApp renames an app within a single transaction. It validates the new
// name, inserts the old name as an alias (2h TTL), and updates apps.name.
func (db *DB) RenameApp(id, oldName, newName string) error {
	tx, err := db.Beginx()
	if err != nil {
		return fmt.Errorf("begin: %w", err)
	}
	defer tx.Rollback()

	// Check uniqueness against apps.name and active aliases.
	var conflict int
	err = tx.Get(&conflict, db.rebind(
		`SELECT COUNT(*) FROM apps WHERE name = ? AND id != ? AND deleted_at IS NULL`),
		newName, id)
	if err != nil {
		return fmt.Errorf("check app name: %w", err)
	}
	if conflict > 0 {
		return fmt.Errorf("name %q is already in use", newName)
	}
	err = tx.Get(&conflict, db.rebind(
		`SELECT COUNT(*) FROM app_aliases WHERE name = ? AND phase = 'alias'`),
		newName)
	if err != nil {
		return fmt.Errorf("check alias name: %w", err)
	}
	if conflict > 0 {
		return fmt.Errorf("name %q is currently reserved as an alias", newName)
	}

	// Insert alias for the old name (2h TTL).
	aliasExpiry := time.Now().UTC().Add(2 * time.Hour).Format(time.RFC3339)
	_, err = tx.Exec(db.rebind(
		`INSERT INTO app_aliases (app_id, name, phase, expires_at)
		 VALUES (?, ?, 'alias', ?)
		 ON CONFLICT (name) DO UPDATE SET app_id = ?, phase = 'alias', expires_at = ?`),
		id, oldName, aliasExpiry, id, aliasExpiry)
	if err != nil {
		return fmt.Errorf("insert alias: %w", err)
	}

	// Update the app name.
	now := time.Now().UTC().Format(time.RFC3339)
	_, err = tx.Exec(db.rebind(
		`UPDATE apps SET name = ?, updated_at = ? WHERE id = ?`),
		newName, now, id)
	if err != nil {
		return fmt.Errorf("update name: %w", err)
	}

	return tx.Commit()
}

// GetAppByAlias looks up an app via the alias table.
// Returns (app, phase, err). Phase is "alias" or "redirect".
func (db *DB) GetAppByAlias(name string) (*AppRow, string, error) {
	var alias struct {
		AppID string `db:"app_id"`
		Phase string `db:"phase"`
	}
	err := db.DB.Get(&alias, db.rebind(
		`SELECT app_id, phase FROM app_aliases WHERE name = ?`), name)
	if err == sql.ErrNoRows {
		return nil, "", nil
	}
	if err != nil {
		return nil, "", err
	}

	app, err := db.GetApp(alias.AppID)
	if err != nil {
		return nil, "", err
	}
	return app, alias.Phase, nil
}

// TransitionExpiredAliases moves alias-phase rows to redirect-phase (7d TTL).
func (db *DB) TransitionExpiredAliases() error {
	now := time.Now().UTC().Format(time.RFC3339)
	newExpiry := time.Now().UTC().Add(7 * 24 * time.Hour).Format(time.RFC3339)
	_, err := db.Exec(db.rebind(
		`UPDATE app_aliases SET phase = 'redirect', expires_at = ?
		 WHERE phase = 'alias' AND expires_at < ?`),
		newExpiry, now)
	return err
}

// CleanupExpiredRedirects deletes redirect-phase rows past their expiry.
func (db *DB) CleanupExpiredRedirects() error {
	now := time.Now().UTC().Format(time.RFC3339)
	_, err := db.Exec(db.rebind(
		`DELETE FROM app_aliases WHERE phase = 'redirect' AND expires_at < ?`),
		now)
	return err
}

// --- Bundle logs ---

// InsertBundleLog persists build output for a deployment.
func (db *DB) InsertBundleLog(bundleID, output string) error {
	now := time.Now().UTC().Format(time.RFC3339)
	_, err := db.Exec(db.rebind(
		`INSERT INTO bundle_logs (bundle_id, output, created_at) VALUES (?, ?, ?)`),
		bundleID, output, now)
	return err
}

// GetBundleLog returns the stored build log for a bundle.
func (db *DB) GetBundleLog(bundleID string) (string, error) {
	var output string
	err := db.DB.Get(&output, db.rebind(
		`SELECT output FROM bundle_logs WHERE bundle_id = ?`), bundleID)
	if err == sql.ErrNoRows {
		return "", nil
	}
	return output, err
}

// --- User search ---

// SearchUsers performs a case-insensitive substring match on name and email
// for active users. Returns at most limit rows ordered by name.
func (db *DB) SearchUsers(query string, limit int) ([]UserRow, error) {
	if limit <= 0 {
		limit = 10
	}
	like := "%" + escapeLike(query) + "%"
	var users []UserRow
	err := db.DB.Select(&users, db.rebind(
		`SELECT * FROM users
		 WHERE active = 1
		   AND (LOWER(name) LIKE LOWER(?) ESCAPE '\' OR LOWER(email) LIKE LOWER(?) ESCAPE '\')
		 ORDER BY name ASC
		 LIMIT ?`),
		like, like, limit)
	if err != nil {
		return nil, err
	}
	return users, nil
}

// --- Tag extensions ---

// TagWithCount extends TagRow with an app count.
type TagWithCount struct {
	TagRow
	AppCount int `db:"app_count"`
}

// ListTagsWithCounts returns all tags with the number of non-deleted apps using each.
func (db *DB) ListTagsWithCounts() ([]TagWithCount, error) {
	var tags []TagWithCount
	err := db.DB.Select(&tags,
		`SELECT t.id, t.name, t.created_at, COUNT(apps.id) AS app_count
		 FROM tags t
		 LEFT JOIN app_tags at ON t.id = at.tag_id
		 LEFT JOIN apps ON at.app_id = apps.id AND apps.deleted_at IS NULL
		 GROUP BY t.id, t.name, t.created_at
		 ORDER BY t.name`)
	if err != nil {
		return nil, err
	}
	return tags, nil
}

// RenameTag updates a tag's name. Returns an error on conflict.
func (db *DB) RenameTag(id, newName string) error {
	_, err := db.Exec(db.rebind(
		`UPDATE tags SET name = ? WHERE id = ?`), newName, id)
	return err
}
