package boardstorage

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cynkra/blockyard/internal/integration"
)

// mockVault stubs the two endpoints ProvisionUser hits:
//
//   - POST /v1/identity/lookup/entity  → returns a synthetic entity ID
//     deterministically derived from the alias name, so callers can
//     predict `user_<id>` without pre-registering mappings.
//   - POST /v1/{mount}/static-roles/{name} → records the call and
//     acknowledges.
//
// Tests can flip `fail` to simulate vault-down for the static-role
// step; the entity lookup stays available so partial-failure
// scenarios are reachable.
type mockVault struct {
	mu      sync.Mutex
	calls   []mockVaultCall
	server  *httptest.Server
	fail    bool
}

type mockVaultCall struct {
	method string
	path   string
	body   string
}

func newMockVault(t *testing.T) *mockVault {
	t.Helper()
	m := &mockVault{}
	m.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		m.mu.Lock()
		m.calls = append(m.calls, mockVaultCall{
			method: r.Method, path: r.URL.Path, body: string(body),
		})
		fail := m.fail
		m.mu.Unlock()

		// identity/lookup/entity is always available (production also
		// answers it independently of the DB secrets engine). Returning
		// a synthetic entity ID per alias keeps tests deterministic.
		if r.URL.Path == "/v1/identity/lookup/entity" {
			var req struct {
				AliasName string `json:"alias_name"`
			}
			_ = json.Unmarshal(body, &req)
			resp := map[string]any{
				"data": map[string]any{"id": mockEntityID(req.AliasName)},
			}
			_ = json.NewEncoder(w).Encode(resp)
			return
		}

		// static-roles path — honour the fail flag here.
		if fail {
			http.Error(w, "forced failure", http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(m.server.Close)
	return m
}

// mockEntityID produces a deterministic, UUID-shaped string from an
// alias name so tests can predict `user_<id>` without pre-registering
// vault state. Not cryptographic; the sha256 slice is just a
// convenient stable bytestream.
func mockEntityID(alias string) string {
	h := sha256.Sum256([]byte(alias))
	b := h[:16]
	return fmt.Sprintf("%08x-%04x-%04x-%04x-%012x",
		b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

// mockRoleName returns the role name ProvisionUser will derive for a
// given alias, given the mock's entity-ID synthesis.
func mockRoleName(alias string) string {
	return "user_" + mockEntityID(alias)
}

func (m *mockVault) callCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.calls)
}

// staticRoleCalls returns only the calls that targeted the
// static-roles endpoint, i.e. excludes identity lookup housekeeping.
func (m *mockVault) staticRoleCalls() []mockVaultCall {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []mockVaultCall
	for _, c := range m.calls {
		if strings.Contains(c.path, "/static-roles/") {
			out = append(out, c)
		}
	}
	return out
}

func (m *mockVault) setFail(fail bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.fail = fail
}

func newVaultClient(m *mockVault) *integration.Client {
	return integration.NewClient(m.server.URL, func() string { return "mock-admin-token" }, nil)
}

// newProvisioner wires a Provisioner against the mock vault with the
// defaults every test in this file shares.
func newProvisioner(m *mockVault, _ string) *Provisioner {
	return &Provisioner{
		// DB intentionally left nil here; callers pass their own.
		Vault:               newVaultClient(m),
		VaultMount:          "database",
		VaultDBConnName:     "postgresql",
		VaultRotationPeriod: 24 * time.Hour,
		MountAccessor:       "auth_jwt_mock",
	}
}

func TestProvisionUser_HappyPath(t *testing.T) {
	d := boardStoragePgDB(t)
	bootstrapAdmin(t, d)
	m := newMockVault(t)

	const sub = "alice"
	// users row must exist before ProvisionUser runs — matches the
	// CallbackHandler sequence (upsert user, then provision).
	_, err := d.ExecContext(context.Background(),
		`INSERT INTO blockyard.users (sub, email, name, last_login)
         VALUES ($1, $2, $3, now())`,
		sub, "alice@example.com", "Alice")
	if err != nil {
		t.Fatalf("insert users: %v", err)
	}

	p := newProvisioner(m, sub)
	p.DB = d
	if err := p.ProvisionUser(context.Background(), sub); err != nil {
		t.Fatalf("ProvisionUser: %v", err)
	}

	wantRole := mockRoleName(sub)

	// PG role exists with LOGIN.
	var canLogin bool
	err = d.QueryRowContext(context.Background(),
		`SELECT rolcanlogin FROM pg_roles WHERE rolname = $1`, wantRole,
	).Scan(&canLogin)
	if err != nil {
		t.Fatalf("query %s: %v", wantRole, err)
	}
	if !canLogin {
		t.Errorf("%s should have LOGIN", wantRole)
	}

	// Vault received one static-roles call to the expected path.
	sc := m.staticRoleCalls()
	if len(sc) != 1 {
		t.Fatalf("static-role call count = %d, want 1", len(sc))
	}
	wantPath := "/v1/database/static-roles/" + wantRole
	if sc[0].path != wantPath {
		t.Errorf("vault path = %q, want %q", sc[0].path, wantPath)
	}
	for _, needle := range []string{
		`"username":"` + wantRole + `"`,
		`"db_name":"postgresql"`,
		`"rotation_period":"24h0m0s"`,
	} {
		if !strings.Contains(sc[0].body, needle) {
			t.Errorf("vault body missing %q: %s", needle, sc[0].body)
		}
	}

	// users.pg_role persisted.
	pgRole, err := d.GetUserPgRole(context.Background(), sub)
	if err != nil {
		t.Fatalf("GetUserPgRole: %v", err)
	}
	if pgRole != wantRole {
		t.Errorf("pg_role = %q, want %q", pgRole, wantRole)
	}
}

func TestProvisionUser_IdempotentAcrossLogins(t *testing.T) {
	d := boardStoragePgDB(t)
	bootstrapAdmin(t, d)
	m := newMockVault(t)
	const sub = "bob"
	_, err := d.ExecContext(context.Background(),
		`INSERT INTO blockyard.users (sub, email, name, last_login)
         VALUES ($1, $2, $3, now())`,
		sub, "bob@example.com", "Bob")
	if err != nil {
		t.Fatal(err)
	}
	p := newProvisioner(m, sub)
	p.DB = d
	if err := p.ProvisionUser(context.Background(), sub); err != nil {
		t.Fatalf("first: %v", err)
	}
	first := m.callCount()
	// Second login: pg_role is already set, fast-path kicks in, no
	// vault or SQL work at all.
	if err := p.ProvisionUser(context.Background(), sub); err != nil {
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
	const sub = "carol"
	_, err := d.ExecContext(context.Background(),
		`INSERT INTO blockyard.users (sub, email, name, last_login)
         VALUES ($1, $2, $3, now())`,
		sub, "carol@example.com", "Carol")
	if err != nil {
		t.Fatal(err)
	}
	p := newProvisioner(m, sub)
	p.DB = d
	err = p.ProvisionUser(context.Background(), sub)
	if err == nil {
		t.Fatal("expected provisioning to fail with vault down")
	}

	// users.pg_role must NOT be persisted.
	pgRole, gerr := d.GetUserPgRole(context.Background(), sub)
	if gerr != nil {
		t.Fatal(gerr)
	}
	if pgRole != "" {
		t.Errorf("pg_role should be empty after vault failure; got %q", pgRole)
	}

	// Retry after vault recovers must succeed and leave the system
	// in the same end state as a clean first login.
	m.setFail(false)
	if err := p.ProvisionUser(context.Background(), sub); err != nil {
		t.Fatalf("retry: %v", err)
	}
	pgRole, _ = d.GetUserPgRole(context.Background(), sub)
	if pgRole != mockRoleName(sub) {
		t.Errorf("pg_role after retry = %q, want %q", pgRole, mockRoleName(sub))
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
