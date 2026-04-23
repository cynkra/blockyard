package boardstorage

import (
	"strings"
	"testing"
)

func TestNormalizePgRole(t *testing.T) {
	cases := []struct {
		name string
		sub  string
		want string
	}{
		{"ascii lowercase", "alice", "user_alice"},
		{"ascii mixed case", "Alice", "user_alice"},
		{"digits kept", "u42", "user_u42"},
		{"email like", "alice@example.com", "user_alice_example_com"},
		{"keycloak uuid", "a1b2c3d4-5678-90ab-cdef-1234567890ab",
			"user_a1b2c3d4_5678_90ab_cdef_1234567890ab"},
		{"unicode folds to underscore", "üser.éxample", "user__ser__xample"},
		{"spaces", "first last", "user_first_last"},
		{"leading non-alnum", "_weird", "user__weird"},
		{"empty sub", "", "user_"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := NormalizePgRole(tc.sub)
			if got != tc.want {
				t.Fatalf("NormalizePgRole(%q) = %q, want %q", tc.sub, got, tc.want)
			}
			if len(got) > maxRoleNameLen {
				t.Fatalf("length %d > %d", len(got), maxRoleNameLen)
			}
			if !strings.HasPrefix(got, "user_") {
				t.Fatalf("missing user_ prefix: %q", got)
			}
		})
	}
}

func TestNormalizePgRoleDeterministic(t *testing.T) {
	// Same input must produce the same output across calls — load-
	// bearing for "subsequent login is a no-op" in the first-login
	// provisioning flow.
	sub := "keycloak|very/strange:id#123"
	a, b := NormalizePgRole(sub), NormalizePgRole(sub)
	if a != b {
		t.Fatalf("non-deterministic: %q vs %q", a, b)
	}
}

func TestNormalizePgRoleTruncation(t *testing.T) {
	// Construct a sub whose normalized form would exceed 63 chars so
	// the hash-suffix branch runs. `user_` + 70 ascii chars = 75.
	sub := strings.Repeat("a", 70)
	got := NormalizePgRole(sub)
	if len(got) != maxRoleNameLen {
		t.Fatalf("length %d != %d", len(got), maxRoleNameLen)
	}
	if !strings.HasPrefix(got, "user_") {
		t.Fatalf("missing user_ prefix: %q", got)
	}
	// Same sub → same truncated output.
	if NormalizePgRole(sub) != got {
		t.Fatal("truncation not deterministic")
	}
	// A different long sub that shares the 54-char prefix must not
	// collide with this one, because the sha256-derived suffix
	// differs.
	other := strings.Repeat("a", 65) + "zzzzz"
	if NormalizePgRole(other) == got {
		t.Fatalf("collision on distinct long subs: %q", got)
	}
}

func TestNormalizePgRoleRoleNamePattern(t *testing.T) {
	// Every output must be a plain PG identifier: [a-z0-9_]+ so it
	// works both quoted and unquoted in SQL.
	samples := []string{
		"alice", "Alice@corp.EXAMPLE", "sub|with/slashes",
		"🙂emoji", strings.Repeat("x", 200),
	}
	for _, s := range samples {
		got := NormalizePgRole(s)
		for i, r := range got {
			valid := (r >= 'a' && r <= 'z') ||
				(r >= '0' && r <= '9') ||
				r == '_'
			if !valid {
				t.Fatalf("sub=%q got=%q: invalid rune %q at index %d", s, got, r, i)
			}
		}
	}
}
