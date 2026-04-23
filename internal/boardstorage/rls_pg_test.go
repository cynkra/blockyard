package boardstorage

import (
	"context"
	"database/sql"
	"fmt"
	"testing"
)

// rlsFixture provisions two users (alice, bob) with known passwords
// and returns sql.DB handles authenticated as each of them. The
// owner-all and share-on-restricted policies are exercised against
// these connections in the subtests below.
type rlsFixture struct {
	admin *sql.DB // the migrator/bootstrap connection (superuser)

	aliceSub  string
	aliceRole string
	aliceDB   *sql.DB

	bobSub  string
	bobRole string
	bobDB   *sql.DB

	// Seeded board IDs by ACL type, owned by bob.
	bobPrivateID    string
	bobPublicID     string
	bobRestrictedID string
	bobRestrictedVersion string // id of the single version
}

func newRLSFixture(t *testing.T) *rlsFixture {
	t.Helper()
	d := boardStoragePgDB(t)
	bootstrapAdmin(t, d)

	const alicePw, bobPw = "alicepw", "bobpw"
	aliceRole := provisionUserRoleSQL(t, d, "alice@example.com", alicePw)
	bobRole := provisionUserRoleSQL(t, d, "bob@example.com", bobPw)

	dbName := currentDB(t, d)
	fx := &rlsFixture{
		admin:     d.DB.DB,
		aliceSub:  "alice@example.com",
		aliceRole: aliceRole,
		aliceDB:   connectAs(t, dbName, aliceRole, alicePw),
		bobSub:    "bob@example.com",
		bobRole:   bobRole,
		bobDB:     connectAs(t, dbName, bobRole, bobPw),
	}
	t.Cleanup(func() {
		fx.aliceDB.Close()
		fx.bobDB.Close()
	})

	// Seed three boards owned by bob, one per ACL type. Uses bob's
	// own connection so the INSERT passes WITH CHECK (owner_all).
	for _, row := range []struct {
		alias    string
		aclType  string
		target   *string
	}{
		{"private-board", "private", &fx.bobPrivateID},
		{"public-board", "public", &fx.bobPublicID},
		{"restricted-board", "restricted", &fx.bobRestrictedID},
	} {
		err := fx.bobDB.QueryRow(
			`INSERT INTO blockyard.boards (owner_sub, board_id, name, acl_type)
             VALUES ($1, $2, $3, $4) RETURNING id`,
			fx.bobSub, row.alias, row.alias, row.aclType,
		).Scan(row.target)
		if err != nil {
			t.Fatalf("seed %s: %v", row.alias, err)
		}
	}
	// One version per seeded board (trigger requires ≥1 version for
	// any subsequent delete).
	for _, id := range []string{fx.bobPrivateID, fx.bobPublicID, fx.bobRestrictedID} {
		target := ""
		if id == fx.bobRestrictedID {
			target = fx.bobRestrictedVersion
			_ = target
		}
		var verID string
		err := fx.bobDB.QueryRow(
			`INSERT INTO blockyard.board_versions (board_ref, data, format)
             VALUES ($1, '{}'::jsonb, 'json') RETURNING id`,
			id,
		).Scan(&verID)
		if err != nil {
			t.Fatalf("seed version for %s: %v", id, err)
		}
		if id == fx.bobRestrictedID {
			fx.bobRestrictedVersion = verID
		}
	}

	return fx
}

// ---- Operational RLS: owner path ----

func TestRLS_OwnerCRUD(t *testing.T) {
	fx := newRLSFixture(t)

	// SELECT own board.
	var name string
	err := fx.bobDB.QueryRow(
		`SELECT name FROM blockyard.boards WHERE id = $1`, fx.bobPrivateID,
	).Scan(&name)
	if err != nil {
		t.Fatalf("own SELECT: %v", err)
	}

	// UPDATE own board.
	if _, err := fx.bobDB.Exec(
		`UPDATE blockyard.boards SET name = 'renamed' WHERE id = $1`, fx.bobPrivateID,
	); err != nil {
		t.Fatalf("own UPDATE: %v", err)
	}

	// INSERT a version on own board.
	if _, err := fx.bobDB.Exec(
		`INSERT INTO blockyard.board_versions (board_ref, data, format)
         VALUES ($1, '{"k":1}'::jsonb, 'json')`, fx.bobPrivateID,
	); err != nil {
		t.Fatalf("own version INSERT: %v", err)
	}

	// List: bob sees all three of his boards.
	rows, err := fx.bobDB.Query(`SELECT id FROM blockyard.boards`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	seen := map[string]bool{}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			t.Fatal(err)
		}
		seen[id] = true
	}
	for _, id := range []string{fx.bobPrivateID, fx.bobPublicID, fx.bobRestrictedID} {
		if !seen[id] {
			t.Errorf("bob's own board %s missing from list", id)
		}
	}
}

// ---- Operational RLS: cross-user visibility ----

func TestRLS_PrivateIsolation(t *testing.T) {
	fx := newRLSFixture(t)
	ensureNoRows(t, fx.aliceDB,
		`SELECT id FROM blockyard.boards WHERE id = $1`, fx.bobPrivateID)
	ensureNoRows(t, fx.aliceDB,
		`SELECT id FROM blockyard.board_versions WHERE board_ref = $1`,
		fx.bobPrivateID)
}

func TestRLS_PublicRead(t *testing.T) {
	fx := newRLSFixture(t)
	var id string
	err := fx.aliceDB.QueryRow(
		`SELECT id FROM blockyard.boards WHERE id = $1`, fx.bobPublicID,
	).Scan(&id)
	if err != nil {
		t.Fatalf("alice cannot read bob's public board: %v", err)
	}
	// Versions of a public board are also readable.
	var count int
	err = fx.aliceDB.QueryRow(
		`SELECT count(*) FROM blockyard.board_versions WHERE board_ref = $1`,
		fx.bobPublicID,
	).Scan(&count)
	if err != nil || count == 0 {
		t.Fatalf("alice cannot read public versions: count=%d err=%v", count, err)
	}
}

func TestRLS_RestrictedRead(t *testing.T) {
	fx := newRLSFixture(t)
	// Pre-condition: alice cannot read the restricted board.
	ensureNoRows(t, fx.aliceDB,
		`SELECT id FROM blockyard.boards WHERE id = $1`, fx.bobRestrictedID)

	// Bob grants her access.
	if _, err := fx.bobDB.Exec(
		`INSERT INTO blockyard.board_shares (board_ref, shared_with_sub)
         VALUES ($1, $2)`,
		fx.bobRestrictedID, fx.aliceSub,
	); err != nil {
		t.Fatalf("bob share to alice: %v", err)
	}

	// Alice now sees it.
	var id string
	err := fx.aliceDB.QueryRow(
		`SELECT id FROM blockyard.boards WHERE id = $1`, fx.bobRestrictedID,
	).Scan(&id)
	if err != nil {
		t.Fatalf("alice cannot read shared board: %v", err)
	}
	// And its versions.
	var verCount int
	err = fx.aliceDB.QueryRow(
		`SELECT count(*) FROM blockyard.board_versions WHERE board_ref = $1`,
		fx.bobRestrictedID,
	).Scan(&verCount)
	if err != nil || verCount == 0 {
		t.Fatalf("alice cannot read shared versions: count=%d err=%v", verCount, err)
	}
}

func TestRLS_WriteProtection(t *testing.T) {
	fx := newRLSFixture(t)

	// Alice cannot INSERT a board claiming bob as owner: WITH CHECK
	// on owner_all rejects the row.
	_, err := fx.aliceDB.Exec(
		`INSERT INTO blockyard.boards (owner_sub, board_id, name)
         VALUES ($1, 'evil', 'evil')`, fx.bobSub,
	)
	if err == nil {
		t.Fatal("alice was allowed to impersonate bob's ownership")
	}

	// Alice cannot UPDATE/DELETE bob's private board (she can't
	// even SELECT it — row simply invisible).
	res, err := fx.aliceDB.Exec(
		`UPDATE blockyard.boards SET name = 'pwned' WHERE id = $1`,
		fx.bobPrivateID,
	)
	if err != nil {
		t.Fatalf("unexpected error on UPDATE: %v", err)
	}
	if n, _ := res.RowsAffected(); n != 0 {
		t.Fatalf("alice UPDATEd bob's private board: %d rows", n)
	}

	// Restricted with share: alice can SELECT, but still cannot
	// UPDATE or DELETE — owner-only policies cover writes.
	if _, err := fx.bobDB.Exec(
		`INSERT INTO blockyard.board_shares (board_ref, shared_with_sub)
         VALUES ($1, $2)`,
		fx.bobRestrictedID, fx.aliceSub,
	); err != nil {
		t.Fatal(err)
	}
	res, err = fx.aliceDB.Exec(
		`DELETE FROM blockyard.boards WHERE id = $1`, fx.bobRestrictedID,
	)
	if err != nil {
		t.Fatalf("unexpected error on DELETE: %v", err)
	}
	if n, _ := res.RowsAffected(); n != 0 {
		t.Fatalf("alice DELETEd bob's restricted board: %d rows", n)
	}
}

func TestRLS_ShareVisibility(t *testing.T) {
	fx := newRLSFixture(t)
	// Bob shares restricted with alice.
	if _, err := fx.bobDB.Exec(
		`INSERT INTO blockyard.board_shares (board_ref, shared_with_sub)
         VALUES ($1, $2)`,
		fx.bobRestrictedID, fx.aliceSub,
	); err != nil {
		t.Fatal(err)
	}

	// Bob sees his own shares as the board owner.
	var bobCount int
	err := fx.bobDB.QueryRow(
		`SELECT count(*) FROM blockyard.board_shares WHERE board_ref = $1`,
		fx.bobRestrictedID,
	).Scan(&bobCount)
	if err != nil || bobCount != 1 {
		t.Fatalf("owner share visibility: count=%d err=%v", bobCount, err)
	}

	// Alice sees her own share row via shares_see_own.
	var aliceCount int
	err = fx.aliceDB.QueryRow(
		`SELECT count(*) FROM blockyard.board_shares
         WHERE shared_with_sub = $1`, fx.aliceSub,
	).Scan(&aliceCount)
	if err != nil || aliceCount != 1 {
		t.Fatalf("share recipient visibility: count=%d err=%v", aliceCount, err)
	}
}

func TestRLS_LastVersionDeleteRaises(t *testing.T) {
	fx := newRLSFixture(t)
	// Seeded board has exactly one version; trigger must fire.
	_, err := fx.bobDB.Exec(
		`DELETE FROM blockyard.board_versions WHERE id = $1`,
		fx.bobRestrictedVersion,
	)
	if err == nil {
		t.Fatal("last-version delete should have been rejected")
	}
	// Add a second version, delete one → succeeds.
	if _, err := fx.bobDB.Exec(
		`INSERT INTO blockyard.board_versions (board_ref, data, format)
         VALUES ($1, '{}'::jsonb, 'json')`,
		fx.bobRestrictedID,
	); err != nil {
		t.Fatal(err)
	}
	if _, err := fx.bobDB.Exec(
		`DELETE FROM blockyard.board_versions WHERE id = $1`,
		fx.bobRestrictedVersion,
	); err != nil {
		t.Fatalf("delete non-last version: %v", err)
	}
}

// ---- Privilege escalation battery ----

func TestRLS_UserCannotSetRoleBlockrUser(t *testing.T) {
	fx := newRLSFixture(t)
	expectErrContains(t, fx.aliceDB, `SET ROLE blockr_user`, "permission")
}

func TestRLS_UserCannotSetRoleOther(t *testing.T) {
	fx := newRLSFixture(t)
	expectErrContains(t, fx.aliceDB,
		fmt.Sprintf(`SET ROLE %s`, fx.bobRole), "permission")
}

func TestRLS_UserCannotSetRoleAdmin(t *testing.T) {
	fx := newRLSFixture(t)
	expectErrContains(t, fx.aliceDB, `SET ROLE blockyard_admin`, "permission")
}

func TestRLS_UserCannotCreateOrAlterRoles(t *testing.T) {
	fx := newRLSFixture(t)
	expectErrContains(t, fx.aliceDB, `CREATE ROLE pwned`, "permission")
	expectErrContains(t, fx.aliceDB, `ALTER ROLE blockr_user BYPASSRLS`, "permission")
}

func TestRLS_UserCannotAlterTables(t *testing.T) {
	fx := newRLSFixture(t)
	// DROP/ALTER on tables owned by the migrator must be rejected;
	// the role doesn't own them and isn't a superuser.
	expectErrContains(t, fx.aliceDB,
		`DROP TABLE blockyard.boards`, "must be owner")
	expectErrContains(t, fx.aliceDB,
		`ALTER TABLE blockyard.boards DROP COLUMN id`, "must be owner")
}

func TestRLS_UserCannotReadRolePasswords(t *testing.T) {
	fx := newRLSFixture(t)
	expectErrContains(t, fx.aliceDB,
		`SELECT rolpassword FROM pg_authid`, "permission")
}

func TestRLS_RowSecurityOffStillFiltered(t *testing.T) {
	fx := newRLSFixture(t)
	// `row_security = off` only lets the session bypass RLS when the
	// role is the table owner or has BYPASSRLS. Alice is neither,
	// so one of two outcomes is acceptable:
	//   - PG errors with "would be affected by row-level security"
	//     (it refuses to silently ignore the bypass attempt).
	//   - The rows are still filtered and zero returned.
	// Either outcome means the escalation attempt was blocked; the
	// failure mode we're guarding against is bob's private rows
	// coming back.
	if _, err := fx.aliceDB.Exec(`SET row_security = off`); err != nil {
		return
	}
	rows, err := fx.aliceDB.Query(
		`SELECT id FROM blockyard.boards WHERE id = $1`, fx.bobPrivateID)
	if err != nil {
		// PG refused to run the query — escalation blocked.
		return
	}
	defer rows.Close()
	if rows.Next() {
		t.Fatal("row_security=off with non-privileged role leaked bob's private board")
	}
}

func TestRLS_UserCannotWriteUsersTable(t *testing.T) {
	fx := newRLSFixture(t)
	// blockr_user has SELECT but no INSERT/UPDATE/DELETE on users;
	// attempting to rewrite pg_role (the identity map) must fail.
	expectErrContains(t, fx.aliceDB,
		`UPDATE blockyard.users SET pg_role = 'user_pwned'`, "permission")
}

// ---- Deactivation end-to-end ----

func TestDeactivation_BlocksLogin(t *testing.T) {
	d := boardStoragePgDB(t)
	bootstrapAdmin(t, d)

	const sub, pw = "eve@example.com", "evepw"
	roleName := provisionUserRoleSQL(t, d, sub, pw)
	dbName := currentDB(t, d)

	// Baseline: login works before deactivation.
	connectAs(t, dbName, roleName, pw).Close()

	if err := SetRoleLogin(context.Background(), d, roleName, false); err != nil {
		t.Fatalf("deactivate: %v", err)
	}
	if conn := tryConnectAs(dbName, roleName, pw); conn != nil {
		conn.Close()
		t.Fatal("expected login failure after NOLOGIN")
	}

	if err := SetRoleLogin(context.Background(), d, roleName, true); err != nil {
		t.Fatalf("reactivate: %v", err)
	}
	// Login succeeds again after the flip.
	connectAs(t, dbName, roleName, pw).Close()
}
