// Package boardstorage hosts the control-plane side of the board-storage
// feature (see #283/#284/#285): startup SQL, per-user PG role
// provisioning, vault static-role registration. Runtime data access
// happens directly between the R worker and PostgreSQL/vault; nothing
// in this package is on that path.
package boardstorage

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"strings"
	"time"

	"github.com/cynkra/blockyard/internal/db"
	"github.com/cynkra/blockyard/internal/integration"
)

// Provisioner orchestrates the per-user side-effects at OIDC first
// login: resolve the vault entity ID for the user's sub, create the
// PG role named `user_<entity-id>`, register it with vault's database
// secrets engine, persist the role on the users row.
//
// Every step is idempotent, and `users.pg_role` is written last —
// so a login interrupted between steps replays cleanly next time
// (CREATE ROLE IF NOT EXISTS, GRANT is a no-op when already granted,
// vault POST is upsert). The durable signal that provisioning
// completed is users.pg_role being non-NULL.
//
// Entity-ID-based role naming (#285) replaces the earlier sub-
// normalization scheme: the templated per-user vault policy resolves
// `user_{{identity.entity.id}}` from the user's own token context,
// which only agrees with blockyard's role name if blockyard also uses
// the entity ID. Bridging two identifier spaces is how we'd get
// silent mismatches; using vault's own identifier for both sides
// removes the bridge.
type Provisioner struct {
	DB                  *db.DB
	Vault               *integration.Client
	VaultMount          string        // cfg.Database.VaultMount
	VaultDBConnName     string        // cfg.Database.VaultDBConnection
	VaultRotationPeriod time.Duration // cfg.Database.VaultRotationPeriod

	// MountAccessor is the opaque accessor of the OIDC/JWT auth mount
	// blockyard users log in through. Required for the entity lookup
	// below; resolved once at startup via AuthMountAccessor and
	// passed in at construction time.
	MountAccessor string
}

// ProvisionUser runs the first-login flow for `sub`. Fast-path:
// returns nil immediately if users.pg_role is already populated.
// Otherwise resolves the vault entity for this sub, derives the PG
// role name `user_<entity-id>`, executes the provisioning steps,
// and writes pg_role to the users row.
//
// On any error, leaves users.pg_role NULL so the next login retries
// from scratch. Partial state (an orphan PG role or a vault static
// role without a matching users row) is tolerated because every
// step short-circuits on re-run.
func (p *Provisioner) ProvisionUser(ctx context.Context, sub string) error {
	existing, err := p.DB.GetUserPgRole(ctx, sub)
	if err != nil {
		return err
	}
	if existing != "" {
		return nil
	}

	entityID, err := p.Vault.IdentityLookupEntityByAlias(ctx, sub, p.MountAccessor)
	if err != nil {
		return fmt.Errorf("lookup vault entity for %s: %w", sub, err)
	}
	roleName := "user_" + entityID

	password, err := randomPassword()
	if err != nil {
		return err
	}

	if err := ensureUserRole(ctx, p.DB, roleName, password); err != nil {
		return err
	}

	if err := p.Vault.DatabaseStaticRoleCreate(
		ctx, p.VaultMount, roleName, roleName,
		p.VaultDBConnName, p.VaultRotationPeriod.String(),
	); err != nil {
		return fmt.Errorf("register vault static role %s: %w", roleName, err)
	}

	if err := p.DB.SetUserPgRole(ctx, sub, roleName); err != nil {
		return err
	}
	return nil
}

// ensureUserRole creates the per-user PG role (if absent) and grants
// the two required memberships. Idempotent: CREATE is guarded by an
// existence check, and the two GRANTs are no-ops when already in
// place.
//
// Runs as blockyard's configured admin connection, which must hold
// CREATEROLE + ADMIN OPTION on blockr_user. In production that
// identity is `blockyard_admin` (created by the startup SQL); in
// tests it's typically the superuser that owns the database.
func ensureUserRole(ctx context.Context, d *db.DB, roleName, password string) error {
	var exists bool
	if err := d.QueryRowContext(ctx,
		`SELECT EXISTS(SELECT 1 FROM pg_roles WHERE rolname = $1)`,
		roleName,
	).Scan(&exists); err != nil {
		return fmt.Errorf("check role %s: %w", roleName, err)
	}
	ident := pgIdent(roleName)
	if !exists {
		// Password is hex-only (64 chars from crypto/rand); no quote
		// escaping needed but pgLiteral handles it regardless.
		stmt := fmt.Sprintf(`CREATE ROLE %s LOGIN PASSWORD %s`, ident, pgLiteral(password))
		if _, err := d.ExecContext(ctx, stmt); err != nil {
			return fmt.Errorf("create role %s: %w", roleName, err)
		}
	}
	// INHERIT TRUE lets the user session act as blockr_user for
	// SELECT/INSERT/UPDATE/DELETE on the board tables. SET FALSE
	// prevents `SET ROLE blockr_user` — that would let a user create
	// rows owned by the group role, which breaks RLS's
	// current_user-based ownership check.
	if _, err := d.ExecContext(ctx,
		fmt.Sprintf(`GRANT blockr_user TO %s WITH INHERIT TRUE, SET FALSE`, ident),
	); err != nil {
		return fmt.Errorf("grant blockr_user to %s: %w", roleName, err)
	}
	// ADMIN OPTION on the per-user role lets vault_db_admin rotate
	// its password. PG16 requires membership with ADMIN OPTION for
	// one role to ALTER another's password.
	if _, err := d.ExecContext(ctx,
		fmt.Sprintf(`GRANT %s TO vault_db_admin WITH ADMIN OPTION`, ident),
	); err != nil {
		return fmt.Errorf("grant %s to vault_db_admin: %w", roleName, err)
	}
	return nil
}

// SetRoleLogin flips a per-user role between LOGIN and NOLOGIN.
// Called from the admin deactivation path: we don't DROP the role
// (that would fail once boards reference it as owner via the
// `owner_sub` FK-adjacent mapping), but flipping LOGIN has the same
// effect of blocking access while preserving ownership references.
func SetRoleLogin(ctx context.Context, d *db.DB, roleName string, login bool) error {
	verb := "NOLOGIN"
	if login {
		verb = "LOGIN"
	}
	_, err := d.ExecContext(ctx, fmt.Sprintf(`ALTER ROLE %s %s`, pgIdent(roleName), verb))
	if err != nil {
		return fmt.Errorf("alter role %s %s: %w", roleName, verb, err)
	}
	return nil
}

func randomPassword() (string, error) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("rand read: %w", err)
	}
	return hex.EncodeToString(buf), nil
}

// pgIdent wraps s as a double-quoted PG identifier. Role names
// derived from vault entity UUIDs are [a-f0-9-]+ so quoting is for
// consistency rather than safety. Embedded double quotes are escaped
// per PG rules.
func pgIdent(s string) string {
	return `"` + strings.ReplaceAll(s, `"`, `""`) + `"`
}

// pgLiteral wraps s as a single-quoted PG string literal. Used for
// the CREATE ROLE … PASSWORD … clause where bind parameters are
// not accepted (DDL).
func pgLiteral(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "''") + "'"
}
