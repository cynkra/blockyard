package boardstorage

import (
	"context"
	"fmt"

	"github.com/cynkra/blockyard/internal/db"
)

// ensureAdminSQL is idempotent — safe to run at every boot. Uses
// PG16-only GRANT syntax (INHERIT FALSE, SET FALSE); callers must
// gate on the PG16+ preflight from #283 before invoking.
//
// Load-bearing property: blockyard_admin can GRANT blockr_user to
// per-user roles (ADMIN OPTION) but cannot itself SET ROLE
// blockr_user (SET FALSE) nor inherit its privileges (INHERIT
// FALSE). So a compromised admin connection can provision and
// deactivate users, but cannot read or write user data.
const ensureAdminSQL = `DO $$ BEGIN
    IF NOT EXISTS (SELECT FROM pg_roles WHERE rolname = 'blockyard_admin') THEN
        CREATE ROLE blockyard_admin NOINHERIT CREATEROLE;
        GRANT blockr_user TO blockyard_admin
            WITH ADMIN OPTION, INHERIT FALSE, SET FALSE;
    END IF;
END $$`

// EnsureBlockyardAdmin runs the idempotent startup SQL that creates
// the blockyard_admin role (and grants it ADMIN OPTION on
// blockr_user) when board storage is enabled. No-op if the role
// already exists.
//
// Not a migration: the GRANT syntax is PG16-only, and operators
// may run on PG13/14/15 with board storage disabled. Migrations
// are dialect-sensitive but not version-sensitive; this sidesteps
// that by living in Go startup code guarded by the preflight.
func EnsureBlockyardAdmin(ctx context.Context, d *db.DB) error {
	if _, err := d.ExecContext(ctx, ensureAdminSQL); err != nil {
		return fmt.Errorf("ensure blockyard_admin: %w", err)
	}
	return nil
}
