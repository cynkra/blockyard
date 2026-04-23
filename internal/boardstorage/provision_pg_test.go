package boardstorage

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/cynkra/blockyard/internal/integration"
)

// mockVault stubs vault's POST {mount}/static-roles/{name} endpoint.
// Records every call so tests can assert the payload, and lets tests
// simulate vault-down by setting fail = true.
type mockVault struct {
	mu     sync.Mutex
	calls  []mockVaultCall
	server *httptest.Server
	fail   bool
}

type mockVaultCall struct {
	path string
	body string
}

func newMockVault(t *testing.T) *mockVault {
	t.Helper()
	m := &mockVault{}
	m.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		m.mu.Lock()
		m.calls = append(m.calls, mockVaultCall{path: r.URL.Path, body: string(body)})
		fail := m.fail
		m.mu.Unlock()
		if fail {
			http.Error(w, "forced failure", http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(m.server.Close)
	return m
}

func (m *mockVault) callCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.calls)
}

func (m *mockVault) lastCall() (mockVaultCall, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.calls) == 0 {
		return mockVaultCall{}, false
	}
	return m.calls[len(m.calls)-1], true
}

func (m *mockVault) setFail(fail bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.fail = fail
}

func newVaultClient(m *mockVault) *integration.Client {
	return integration.NewClient(m.server.URL, func() string { return "mock-admin-token" })
}

func TestProvisionUser_HappyPath(t *testing.T) {
	d := boardStoragePgDB(t)
	bootstrapAdmin(t, d)
	m := newMockVault(t)

	// users row must exist before ProvisionUser runs — matches the
	// CallbackHandler sequence (upsert user, then provision).
	_, err := d.ExecContext(context.Background(),
		`INSERT INTO blockyard.users (sub, email, name, last_login)
         VALUES ($1, $2, $3, now())`,
		"alice", "alice@example.com", "Alice")
	if err != nil {
		t.Fatalf("insert users: %v", err)
	}

	p := &Provisioner{
		DB:              d,
		Vault:           newVaultClient(m),
		VaultMount:      "database",
		VaultDBConnName: "postgresql",
	}
	if err := p.ProvisionUser(context.Background(), "alice"); err != nil {
		t.Fatalf("ProvisionUser: %v", err)
	}

	// PG role exists with LOGIN.
	var canLogin bool
	err = d.QueryRowContext(context.Background(),
		`SELECT rolcanlogin FROM pg_roles WHERE rolname = 'user_alice'`,
	).Scan(&canLogin)
	if err != nil {
		t.Fatalf("query user_alice: %v", err)
	}
	if !canLogin {
		t.Error("user_alice should have LOGIN")
	}

	// Vault received exactly one call to the expected path.
	if got, want := m.callCount(), 1; got != want {
		t.Fatalf("vault call count = %d, want %d", got, want)
	}
	call, _ := m.lastCall()
	if call.path != "/v1/database/static-roles/user_alice" {
		t.Errorf("vault path = %q", call.path)
	}
	for _, needle := range []string{`"username":"user_alice"`, `"db_name":"postgresql"`, `"rotation_period":"24h"`} {
		if !strings.Contains(call.body, needle) {
			t.Errorf("vault body missing %q: %s", needle, call.body)
		}
	}

	// users.pg_role persisted.
	pgRole, err := d.GetUserPgRole(context.Background(), "alice")
	if err != nil {
		t.Fatalf("GetUserPgRole: %v", err)
	}
	if pgRole != "user_alice" {
		t.Errorf("pg_role = %q, want %q", pgRole, "user_alice")
	}
}

func TestProvisionUser_IdempotentAcrossLogins(t *testing.T) {
	d := boardStoragePgDB(t)
	bootstrapAdmin(t, d)
	m := newMockVault(t)
	_, err := d.ExecContext(context.Background(),
		`INSERT INTO blockyard.users (sub, email, name, last_login)
         VALUES ($1, $2, $3, now())`,
		"bob", "bob@example.com", "Bob")
	if err != nil {
		t.Fatal(err)
	}
	p := &Provisioner{
		DB:              d,
		Vault:           newVaultClient(m),
		VaultMount:      "database",
		VaultDBConnName: "postgresql",
	}
	if err := p.ProvisionUser(context.Background(), "bob"); err != nil {
		t.Fatalf("first: %v", err)
	}
	first := m.callCount()
	// Second login: pg_role is already set, fast-path kicks in, no
	// vault or SQL work at all.
	if err := p.ProvisionUser(context.Background(), "bob"); err != nil {
		t.Fatalf("second: %v", err)
	}
	if got := m.callCount(); got != first {
		t.Errorf("second login hit vault: %d → %d calls", first, got)
	}
}

func TestProvisionUser_VaultFailureLeavesNoPersistedState(t *testing.T) {
	d := boardStoragePgDB(t)
	bootstrapAdmin(t, d)
	m := newMockVault(t)
	m.setFail(true)
	_, err := d.ExecContext(context.Background(),
		`INSERT INTO blockyard.users (sub, email, name, last_login)
         VALUES ($1, $2, $3, now())`,
		"carol", "carol@example.com", "Carol")
	if err != nil {
		t.Fatal(err)
	}
	p := &Provisioner{
		DB:              d,
		Vault:           newVaultClient(m),
		VaultMount:      "database",
		VaultDBConnName: "postgresql",
	}
	err = p.ProvisionUser(context.Background(), "carol")
	if err == nil {
		t.Fatal("expected provisioning to fail with vault down")
	}

	// users.pg_role must NOT be persisted.
	pgRole, gerr := d.GetUserPgRole(context.Background(), "carol")
	if gerr != nil {
		t.Fatal(gerr)
	}
	if pgRole != "" {
		t.Errorf("pg_role should be empty after vault failure; got %q", pgRole)
	}

	// Retry after vault recovers must succeed and leave the system
	// in the same end state as a clean first login.
	m.setFail(false)
	if err := p.ProvisionUser(context.Background(), "carol"); err != nil {
		t.Fatalf("retry: %v", err)
	}
	pgRole, _ = d.GetUserPgRole(context.Background(), "carol")
	if pgRole != "user_carol" {
		t.Errorf("pg_role after retry = %q", pgRole)
	}
}

func TestSetRoleLogin_TogglesLoginAttribute(t *testing.T) {
	d := boardStoragePgDB(t)
	bootstrapAdmin(t, d)
	// Set up a user role with a known password so we can query its
	// attributes directly.
	const sub = "dave"
	roleName := provisionUserRoleSQL(t, d, sub, "testpassword")

	for _, tc := range []struct {
		name  string
		login bool
	}{
		{"deactivate", false},
		{"reactivate", true},
		{"idempotent-reactivate", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := SetRoleLogin(context.Background(), d, roleName, tc.login); err != nil {
				t.Fatalf("SetRoleLogin: %v", err)
			}
			var canLogin bool
			if err := d.QueryRowContext(context.Background(),
				`SELECT rolcanlogin FROM pg_roles WHERE rolname = $1`, roleName,
			).Scan(&canLogin); err != nil {
				t.Fatal(err)
			}
			if canLogin != tc.login {
				t.Errorf("rolcanlogin = %v, want %v", canLogin, tc.login)
			}
		})
	}
}
