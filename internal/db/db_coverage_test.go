package db

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/cynkra/blockyard/internal/config"
)

// --- WithCredsRotator + Open options ---

// TestOpen_WithCredsRotator_NoOpOnSQLite covers the WithCredsRotator
// option function and confirms that it is a no-op for SQLite (the
// openSQLite path ignores options).
func TestOpen_WithCredsRotator_NoOpOnSQLite(t *testing.T) {
	rot := RotatorFunc(func(_ context.Context) (string, string, error) {
		return "u", "p", nil
	})
	dir := t.TempDir()
	d, err := Open(
		config.DatabaseConfig{Driver: "sqlite", Path: dir + "/x.db"},
		WithCredsRotator(rot),
	)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer d.Close()
	// SQLite path does not wire a creds provider.
	if d.creds != nil {
		t.Error("SQLite DB should not carry a creds provider")
	}
}

// TestOpen_UnsupportedDriverSurfacesClearError asserts that the default
// branch in Open returns the "unsupported database driver" sentinel
// with the driver string interpolated.
func TestOpen_UnsupportedDriverReturnsError(t *testing.T) {
	_, err := Open(config.DatabaseConfig{Driver: "mysql"})
	if err == nil || !strings.Contains(err.Error(), "unsupported") {
		t.Fatalf("expected unsupported-driver error, got %v", err)
	}
}

// TestOpenPostgres_ParseErrorBubbles covers the pgxpool.ParseConfig
// error branch in openPostgres — a clearly malformed DSN.
func TestOpenPostgres_ParseError(t *testing.T) {
	_, err := Open(config.DatabaseConfig{
		Driver: "postgres",
		URL:    "postgres://[not a valid url",
	})
	if err == nil {
		t.Fatal("expected parse error for malformed postgres URL")
	}
}

// TestOpenPostgres_SeedRotatorFailure covers the branch where the
// vault seed fetch fails before the first connection is made.
func TestOpenPostgres_SeedRotatorFailure(t *testing.T) {
	if pgBaseURL == "" {
		t.Skip("postgres not available")
	}
	rot := RotatorFunc(func(_ context.Context) (string, string, error) {
		return "", "", errors.New("vault unreachable")
	})
	_, err := Open(
		config.DatabaseConfig{Driver: "postgres", URL: pgBaseURL},
		WithCredsRotator(rot),
	)
	if err == nil {
		t.Fatal("expected seed-rotator failure to surface from Open")
	}
	if !strings.Contains(err.Error(), "seed postgres creds") {
		t.Errorf("error should mention 'seed postgres creds': %v", err)
	}
}

// --- EnsurePostgresVersion ---

func TestEnsurePostgresVersion_RejectsSQLite(t *testing.T) {
	d := testDB(t)
	err := d.EnsurePostgresVersion(context.Background(), PostgresMinVersion16)
	if err == nil {
		t.Fatal("expected EnsurePostgresVersion to reject SQLite")
	}
	if !strings.Contains(err.Error(), "postgres driver") {
		t.Errorf("error should mention 'postgres driver': %v", err)
	}
}

func TestEnsurePostgresVersion_ServerMeetsMin(t *testing.T) {
	if pgBaseURL == "" {
		t.Skip("postgres not available")
	}
	d := testPostgresDB(t)
	// Use a very low minimum so any real PG passes.
	if err := d.EnsurePostgresVersion(context.Background(), 100000); err != nil {
		t.Errorf("expected modern PG to meet min=100000, got: %v", err)
	}
}

func TestEnsurePostgresVersion_TooOld(t *testing.T) {
	if pgBaseURL == "" {
		t.Skip("postgres not available")
	}
	d := testPostgresDB(t)
	// Use an impossibly high minimum so the server always reports too old.
	err := d.EnsurePostgresVersion(context.Background(), 999999)
	if err == nil {
		t.Fatal("expected 'too old' error from a 999999 floor")
	}
	if !strings.Contains(err.Error(), "or later required") {
		t.Errorf("error phrasing changed: %v", err)
	}
}

// --- Query error paths ---

// TestQueriesReturnErrorOnClosedDB verifies that the error branches in
// the common CRUD methods wire through when the pool is closed. sqlx
// errors propagate with the query text wrapped via fmt.Errorf.
func TestQueriesReturnErrorOnClosedDB(t *testing.T) {
	d := testDB(t)
	if err := d.Close(); err != nil {
		t.Fatal(err)
	}

	// Each of these functions has an "if err != nil" in its body that
	// only fires when the underlying DB hand is broken. Running them
	// through a closed DB trips every one in a single pass.
	if _, err := d.ListApps(); err == nil {
		t.Error("ListApps on closed DB: expected error")
	}
	if _, err := d.ListAccessibleApps("user"); err == nil {
		t.Error("ListAccessibleApps on closed DB: expected error")
	}
	if _, err := d.ListBundlesByApp("app-x"); err == nil {
		t.Error("ListBundlesByApp on closed DB: expected error")
	}
	if _, err := d.ListAppDataMounts("app-x"); err == nil {
		t.Error("ListAppDataMounts on closed DB: expected error")
	}
	if _, err := d.ListPreWarmedApps(); err == nil {
		t.Error("ListPreWarmedApps on closed DB: expected error")
	}
	if _, err := d.ListDeletedApps(); err == nil {
		t.Error("ListDeletedApps on closed DB: expected error")
	}
	if _, err := d.ListExpiredDeletedApps("1970-01-01"); err == nil {
		t.Error("ListExpiredDeletedApps on closed DB: expected error")
	}
	if _, err := d.ListAppsWithRefreshSchedule(); err == nil {
		t.Error("ListAppsWithRefreshSchedule on closed DB: expected error")
	}
	if _, err := d.ListAppAccess("app-x"); err == nil {
		t.Error("ListAppAccess on closed DB: expected error")
	}
	if _, err := d.ListTags(); err == nil {
		t.Error("ListTags on closed DB: expected error")
	}
	if _, err := d.FailStaleBuilds(); err == nil {
		t.Error("FailStaleBuilds on closed DB: expected error")
	}
	if err := d.ClearActiveBundle("app-x"); err == nil {
		t.Error("ClearActiveBundle on closed DB: expected error")
	}
	if _, err := d.RevokeAppAccess("app-x", "p", "user"); err == nil {
		t.Error("RevokeAppAccess on closed DB: expected error")
	}
	if _, err := d.DeleteBundle("b-x"); err == nil {
		t.Error("DeleteBundle on closed DB: expected error")
	}
	if err := d.UpdateBundleStatus("b-x", "ready"); err == nil {
		t.Error("UpdateBundleStatus on closed DB: expected error")
	}
	if err := d.SetBundleDeployed("b-x", "u"); err == nil {
		t.Error("SetBundleDeployed on closed DB: expected error")
	}
	if err := d.SetActiveBundle("a", "b"); err == nil {
		t.Error("SetActiveBundle on closed DB: expected error")
	}
	if err := d.ActivateBundle("a", "b"); err == nil {
		t.Error("ActivateBundle on closed DB: expected error")
	}
	if err := d.SetAppDataMounts("a", nil); err == nil {
		t.Error("SetAppDataMounts on closed DB: expected error")
	}
}

// TestGetErrorsOnClosedDB complements the list-query check for Get-like
// queries whose sql.ErrNoRows early-return is covered, but the generic
// "err != nil" branch is not — close the DB to hit it.
func TestGetErrorsOnClosedDB(t *testing.T) {
	d := testDB(t)
	if err := d.Close(); err != nil {
		t.Fatal(err)
	}

	// These calls must each return a non-nil error from the generic
	// err-check (not an ErrNoRows-mapped nil).
	if _, err := d.GetApp("id"); err == nil {
		t.Error("GetApp on closed DB: expected error")
	}
	if _, err := d.GetAppByName("name"); err == nil {
		t.Error("GetAppByName on closed DB: expected error")
	}
	if _, err := d.GetAppIncludeDeleted("id"); err == nil {
		t.Error("GetAppIncludeDeleted on closed DB: expected error")
	}
	if _, err := d.GetBundle("id"); err == nil {
		t.Error("GetBundle on closed DB: expected error")
	}
	if _, err := d.GetUser("sub"); err == nil {
		t.Error("GetUser on closed DB: expected error")
	}
	if _, err := d.GetTag("name"); err == nil {
		t.Error("GetTag on closed DB: expected error")
	}
}

// TestCreateBundle_ForeignKeyError covers the CreateBundle error branch
// where the app does not exist. This drives the sqlite FK violation
// that CreateBundle wraps with a diagnostic.
func TestCreateBundle_ForeignKeyError(t *testing.T) {
	d := testDB(t)
	_, err := d.CreateBundle("b-1", "nonexistent-app", "admin", false)
	if err == nil {
		t.Fatal("expected FK-violation error")
	}
}

// TestOpenSQLite_EmptyPath tries opening sqlite with an empty path —
// this is an invalid configuration that should return a clear error
// from the driver, exercising the err path of Open.
func TestOpenSQLite_EmptyPath(t *testing.T) {
	// Open("") resolves DSN to "?_pragma=..." which sqlite accepts as
	// the current directory; so exercise a definitely-invalid path: a
	// directory that doesn't exist and can't be created.
	_, err := Open(config.DatabaseConfig{
		Driver: "sqlite",
		Path:   "/proc/nonexistent/" + fmt.Sprintf("x-%d.db", 1),
	})
	if err == nil {
		t.Fatal("expected error opening sqlite under /proc/nonexistent")
	}
}

// TestSessionErrorsOnClosedDB covers the error branches in session
// mutating queries when the DB connection is unusable.
func TestSessionErrorsOnClosedDB(t *testing.T) {
	d := testDB(t)
	if err := d.Close(); err != nil {
		t.Fatal(err)
	}

	if err := d.CreateSession("s", "app", "w", ""); err == nil {
		t.Error("CreateSession on closed DB: expected error")
	}
	if err := d.EndSession("s", "ended"); err == nil {
		t.Error("EndSession on closed DB: expected error")
	}
	if err := d.CrashWorkerSessions("w"); err == nil {
		t.Error("CrashWorkerSessions on closed DB: expected error")
	}
	if err := d.EndWorkerSessions("w"); err == nil {
		t.Error("EndWorkerSessions on closed DB: expected error")
	}
	if err := d.EndAppSessions("app"); err == nil {
		t.Error("EndAppSessions on closed DB: expected error")
	}
	if _, err := d.ListSessions("app", SessionListOpts{}); err == nil {
		t.Error("ListSessions on closed DB: expected error")
	}
	if _, err := d.GetSession("s"); err == nil {
		t.Error("GetSession on closed DB: expected error")
	}
}

// TestPATErrorsOnClosedDB covers PAT mutating queries against a closed
// pool. All should surface the connection error via their "if err !=
// nil" branches.
func TestPATErrorsOnClosedDB(t *testing.T) {
	d := testDB(t)
	if err := d.Close(); err != nil {
		t.Fatal(err)
	}

	hash := []byte("irrelevant-for-this-test")
	if _, err := d.CreatePAT("p", hash, "u", "n", nil); err == nil {
		t.Error("CreatePAT on closed DB: expected error")
	}
	if _, err := d.LookupPATByHash(hash); err == nil {
		t.Error("LookupPATByHash on closed DB: expected error")
	}
	if _, err := d.ListPATsByUser("u"); err == nil {
		t.Error("ListPATsByUser on closed DB: expected error")
	}
	if _, err := d.RevokePAT("p", "u"); err == nil {
		t.Error("RevokePAT on closed DB: expected error")
	}
	if _, err := d.RevokeAllPATs("u"); err == nil {
		t.Error("RevokeAllPATs on closed DB: expected error")
	}
}

// TestUserErrorsOnClosedDB covers the user-table mutators.
func TestUserErrorsOnClosedDB(t *testing.T) {
	d := testDB(t)
	if err := d.Close(); err != nil {
		t.Fatal(err)
	}

	if _, err := d.UpsertUser("sub", "x@y", "name"); err == nil {
		t.Error("UpsertUser on closed DB: expected error")
	}
	if _, err := d.UpsertUserWithRole("sub", "x@y", "name", "admin"); err == nil {
		t.Error("UpsertUserWithRole on closed DB: expected error")
	}
	if _, _, err := d.ListUsers(ListUsersOpts{}); err == nil {
		t.Error("ListUsers on closed DB: expected error")
	}
}

// TestTagErrorsOnClosedDB covers tag-related mutators.
func TestTagErrorsOnClosedDB(t *testing.T) {
	d := testDB(t)
	if err := d.Close(); err != nil {
		t.Fatal(err)
	}

	if _, err := d.CreateTag("name"); err == nil {
		t.Error("CreateTag on closed DB: expected error")
	}
	if err := d.AddAppTag("a", "t"); err == nil {
		t.Error("AddAppTag on closed DB: expected error")
	}
	if _, err := d.RemoveAppTag("a", "t"); err == nil {
		t.Error("RemoveAppTag on closed DB: expected error")
	}
	if _, err := d.ListAppTags("a"); err == nil {
		t.Error("ListAppTags on closed DB: expected error")
	}
	if _, err := d.DeleteTag("name"); err == nil {
		t.Error("DeleteTag on closed DB: expected error")
	}
}

// TestPing_SQLiteBasic exercises the Ping happy path for SQLite so the
// pre-rotate-check branch of Ping() is executed without a rotator.
func TestPing_SQLiteBasic(t *testing.T) {
	d := testDB(t)
	if err := d.Ping(context.Background()); err != nil {
		t.Errorf("Ping: %v", err)
	}
}

// TestOpen_SqliteExistingDirectory covers the branch in openSQLite
// where the target directory is resolved to an already-existing dir
// (MkdirAll is a no-op).
func TestOpen_SqliteExistingDirectory(t *testing.T) {
	dir := t.TempDir()
	nested := dir + "/pre-existing"
	if err := os.MkdirAll(nested, 0o755); err != nil {
		t.Fatal(err)
	}
	d, err := Open(config.DatabaseConfig{
		Driver: "sqlite",
		Path:   nested + "/x.db",
	})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer d.Close()
}
