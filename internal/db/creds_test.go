package db

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/cynkra/blockyard/internal/config"
	"github.com/cynkra/blockyard/internal/integration"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
)

func TestParseDSNUser(t *testing.T) {
	tests := []struct {
		name       string
		dsn        string
		wantUser   string
		wantPass   string
		wantErrStr string
	}{
		{
			name:     "with userinfo",
			dsn:      "postgres://alice:s3cret@db:5432/app",
			wantUser: "alice",
			wantPass: "s3cret",
		},
		{
			name:     "user only, no password",
			dsn:      "postgres://alice@db:5432/app",
			wantUser: "alice",
			wantPass: "",
		},
		{
			name:     "no userinfo",
			dsn:      "postgres://db:5432/app",
			wantUser: "",
			wantPass: "",
		},
		{
			name:       "malformed",
			dsn:        "not a url :: at all",
			wantErrStr: "parse postgres url",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			u, p, err := parseDSNUser(tc.dsn)
			if tc.wantErrStr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErrStr) {
					t.Fatalf("want error containing %q, got %v", tc.wantErrStr, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if u != tc.wantUser || p != tc.wantPass {
				t.Errorf("got (%q, %q), want (%q, %q)", u, p, tc.wantUser, tc.wantPass)
			}
		})
	}
}

func TestPgCredsProvider_NoRotator(t *testing.T) {
	p := newPgCredsProvider("alice", "s3cret", nil)

	u, pw := p.current()
	if u != "alice" || pw != "s3cret" {
		t.Errorf("current = (%q, %q), want (alice, s3cret)", u, pw)
	}
	if p.hasRotator() {
		t.Error("hasRotator = true, want false")
	}
	if err := p.rotate(context.Background()); err == nil {
		t.Error("rotate without rotator should error")
	}
}

func TestPgCredsProvider_RotateUpdatesCurrent(t *testing.T) {
	var calls atomic.Int32
	rot := RotatorFunc(func(_ context.Context) (string, string, error) {
		calls.Add(1)
		return "minted-user", "minted-pass", nil
	})
	p := newPgCredsProvider("env-user", "env-pass", rot)

	u, pw := p.current()
	if u != "env-user" || pw != "env-pass" {
		t.Errorf("pre-rotate current = (%q, %q), want (env-user, env-pass)", u, pw)
	}

	if err := p.rotate(context.Background()); err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 1 {
		t.Errorf("rotator calls = %d, want 1", calls.Load())
	}

	u, pw = p.current()
	if u != "minted-user" || pw != "minted-pass" {
		t.Errorf("post-rotate current = (%q, %q), want (minted-user, minted-pass)", u, pw)
	}
}

func TestPgCredsProvider_RotateError(t *testing.T) {
	wantErr := errors.New("vault down")
	rot := RotatorFunc(func(_ context.Context) (string, string, error) {
		return "", "", wantErr
	})
	p := newPgCredsProvider("env-user", "env-pass", rot)

	err := p.rotate(context.Background())
	if err == nil {
		t.Fatal("rotate should surface rotator errors")
	}
	if !errors.Is(err, wantErr) {
		t.Errorf("rotate err = %v, want wrap of %v", err, wantErr)
	}

	// Current must remain the env creds — failed rotation must not
	// leave the provider in a partially-updated state.
	u, pw := p.current()
	if u != "env-user" || pw != "env-pass" {
		t.Errorf("after failed rotate, current = (%q, %q); must remain env creds", u, pw)
	}
}

func TestIsPostgresAuthError(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"non-auth error", errors.New("connection refused"), false},
		{"PgError 28P01", &pgconn.PgError{Code: "28P01", Message: "password authentication failed"}, true},
		{"PgError 28000", &pgconn.PgError{Code: "28000", Message: "invalid authorization"}, true},
		{"PgError other", &pgconn.PgError{Code: "42P01", Message: "relation does not exist"}, false},
		{"wrapped 28P01", fmt.Errorf("ping: %w", &pgconn.PgError{Code: "28P01"}), true},
		{"role does not exist (connect-time)", errors.New(`FATAL: role "v-token-xyz-TIMESTAMP" does not exist (SQLSTATE 28P01)`), true},
		{"sqlstate substring without code match", errors.New("something SQLSTATE 28P01 here"), true},
		{"role exists message", errors.New(`CREATE ROLE "foo"`), false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := isPostgresAuthError(tc.err)
			if got != tc.want {
				t.Errorf("isPostgresAuthError(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}

// TestOpenPostgres_RotatesOnAuthFailure covers the issue-#238 hot path:
// blockyard starts up with stale env creds (provisioner's lease already
// revoked → role dropped), Open fails the first Ping with 28P01, falls
// through to the rotator, mints fresh creds via OpenBao, and succeeds
// on retry.
func TestOpenPostgres_RotatesOnAuthFailure(t *testing.T) {
	if pgBaseURL == "" {
		t.Skip("BLOCKYARD_TEST_POSTGRES_URL not set; skipping PostgreSQL tests")
	}

	// Clone the pre-migrated template so we start with a working DB
	// using the real credentials from pgBaseURL.
	dbName := uniqueDBName()
	mustCloneTemplate(t, dbName)
	t.Cleanup(func() { dropDB(dbName) })

	// Extract the real creds to hand back from the mock rotator.
	baseU, err := url.Parse(pgBaseURL)
	if err != nil {
		t.Fatal(err)
	}
	realUser := baseU.User.Username()
	realPass, _ := baseU.User.Password()

	// Build a DSN with a deliberately-bad password so the initial
	// Ping fails with 28P01 — mimicking the "env creds already
	// revoked" condition.
	badU := *baseU
	badU.User = url.UserPassword(realUser, "wrong-password-triggers-28P01")
	badU.Path = "/" + dbName
	badDSN := badU.String()

	// Mock OpenBao: responds to GET /v1/database/creds/{role} with
	// the real creds, so the rotator hands back something that
	// actually works.
	var mockHits atomic.Int32
	mock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mockHits.Add(1)
		if r.URL.Path != "/v1/database/creds/blockyard-app" {
			http.NotFound(w, r)
			return
		}
		json.NewEncoder(w).Encode(map[string]any{
			"lease_duration": 3600,
			"data":           map[string]any{"username": realUser, "password": realPass},
		})
	}))
	t.Cleanup(mock.Close)

	vault := integration.NewClient(mock.URL, func() string { return "test-token" })
	rot := RotatorFunc(func(ctx context.Context) (string, string, error) {
		u, p, _, err := vault.GenerateDBCreds(ctx, "test-token", "blockyard-app")
		return u, p, err
	})

	d, err := Open(
		config.DatabaseConfig{Driver: "postgres", URL: badDSN},
		WithCredsRotator(rot),
	)
	if err != nil {
		t.Fatalf("Open with rotator should recover from auth failure: %v", err)
	}
	t.Cleanup(func() { d.Close() })

	if mockHits.Load() != 1 {
		t.Errorf("vault was hit %d times, want 1 (one rotation)", mockHits.Load())
	}

	// Sanity: the returned DB is actually usable.
	if err := d.Ping(context.Background()); err != nil {
		t.Errorf("Ping after rotation: %v", err)
	}
}

// TestOpenPostgres_AuthFailureWithoutRotator verifies that without a
// rotator, an initial auth failure surfaces as a plain error rather
// than silently succeeding.
func TestOpenPostgres_AuthFailureWithoutRotator(t *testing.T) {
	if pgBaseURL == "" {
		t.Skip("BLOCKYARD_TEST_POSTGRES_URL not set; skipping PostgreSQL tests")
	}

	baseU, err := url.Parse(pgBaseURL)
	if err != nil {
		t.Fatal(err)
	}
	badU := *baseU
	badU.User = url.UserPassword(baseU.User.Username(), "wrong-password")
	badDSN := badU.String()

	_, err = Open(config.DatabaseConfig{Driver: "postgres", URL: badDSN})
	if err == nil {
		t.Fatal("Open must fail when password is wrong and no rotator is set")
	}
	if !isPostgresAuthError(err) && !strings.Contains(err.Error(), "authentication") {
		t.Errorf("expected auth error, got %v", err)
	}
}

// uniqueDBName returns a short random database name for a test
// scratch clone. Mirrors the pattern used by testPostgresDB.
func uniqueDBName() string {
	return "test_" + strings.ReplaceAll(uuid.New().String(), "-", "")[:16]
}

func mustCloneTemplate(t *testing.T, dbName string) {
	t.Helper()
	admin, err := sql.Open("pgx", pgBaseURL)
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close()
	if _, err := admin.Exec("CREATE DATABASE " + dbName + " TEMPLATE " + pgTemplateDB); err != nil {
		t.Fatal(err)
	}
}

func dropDB(dbName string) {
	admin, err := sql.Open("pgx", pgBaseURL)
	if err != nil {
		return
	}
	defer admin.Close()
	_, _ = admin.Exec("DROP DATABASE IF EXISTS " + dbName)
}
