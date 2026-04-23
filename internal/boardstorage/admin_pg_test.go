package boardstorage

import (
	"context"
	"testing"
)

func TestEnsureBlockyardAdmin_CreatesRole(t *testing.T) {
	d := boardStoragePgDB(t)
	// PG roles are cluster-global, not per-database, so an earlier
	// test in this run (or a previous invocation) may have left
	// blockyard_admin behind. Idempotent bootstrap handles both
	// states — the post-condition check below is what matters.
	if err := EnsureBlockyardAdmin(context.Background(), d); err != nil {
		t.Fatalf("EnsureBlockyardAdmin: %v", err)
	}

	// Role now exists with the expected attributes.
	var rolcreaterole, rolinherit bool
	err := d.QueryRowContext(context.Background(),
		`SELECT rolcreaterole, rolinherit FROM pg_roles WHERE rolname = 'blockyard_admin'`,
	).Scan(&rolcreaterole, &rolinherit)
	if err != nil {
		t.Fatalf("query role attrs: %v", err)
	}
	if !rolcreaterole {
		t.Error("blockyard_admin missing CREATEROLE")
	}
	if rolinherit {
		t.Error("blockyard_admin should be NOINHERIT")
	}

	// Membership check: blockr_user granted WITH ADMIN OPTION, SET
	// FALSE, INHERIT FALSE. pg_auth_members exposes all three as
	// booleans.
	var admin, inherit, setOpt bool
	err = d.QueryRowContext(context.Background(), `
        SELECT m.admin_option, m.inherit_option, m.set_option
        FROM pg_auth_members m
        JOIN pg_roles r ON r.oid = m.roleid
        JOIN pg_roles g ON g.oid = m.member
        WHERE r.rolname = 'blockr_user' AND g.rolname = 'blockyard_admin'`,
	).Scan(&admin, &inherit, &setOpt)
	if err != nil {
		t.Fatalf("pg_auth_members: %v", err)
	}
	if !admin {
		t.Error("blockr_user grant to blockyard_admin missing ADMIN OPTION")
	}
	if inherit {
		t.Error("blockr_user grant should be INHERIT FALSE")
	}
	if setOpt {
		t.Error("blockr_user grant should be SET FALSE")
	}
}

func TestEnsureBlockyardAdmin_Idempotent(t *testing.T) {
	d := boardStoragePgDB(t)
	for i := 0; i < 3; i++ {
		if err := EnsureBlockyardAdmin(context.Background(), d); err != nil {
			t.Fatalf("run %d: %v", i, err)
		}
	}
	var count int
	if err := d.QueryRowContext(context.Background(),
		`SELECT count(*) FROM pg_roles WHERE rolname = 'blockyard_admin'`,
	).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("blockyard_admin count = %d, want 1", count)
	}
}
