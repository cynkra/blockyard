package auth

import "context"

// Role is a system-level role derived from IdP groups via role_mappings.
// Ordered by privilege — higher value means more privilege.
type Role int

const (
	RoleNone      Role = iota // No mapped role
	RoleViewer                // Can view granted apps
	RolePublisher             // Can create + manage own apps
	RoleAdmin                 // Full access to everything
)

// String returns the lowercase name of the role.
func (r Role) String() string {
	switch r {
	case RoleViewer:
		return "viewer"
	case RolePublisher:
		return "publisher"
	case RoleAdmin:
		return "admin"
	default:
		return "none"
	}
}

// ParseRole converts a string to a Role. Returns RoleNone for unrecognized values.
func ParseRole(s string) Role {
	switch s {
	case "admin":
		return RoleAdmin
	case "publisher":
		return RolePublisher
	case "viewer":
		return RoleViewer
	default:
		return RoleNone
	}
}

// CanCreateApp reports whether this role can create new apps.
func (r Role) CanCreateApp() bool {
	return r >= RolePublisher
}

// CanViewAllApps reports whether this role can see all apps regardless
// of ownership or grants.
func (r Role) CanViewAllApps() bool {
	return r >= RoleAdmin
}

// CanManageRoles reports whether this role can manage role mappings.
func (r Role) CanManageRoles() bool {
	return r >= RoleAdmin
}

// AuthSource describes how the caller authenticated.
type AuthSource int

const (
	AuthSourceSession    AuthSource = iota // Browser session via OIDC
	AuthSourceJWT                          // JWT Bearer token (client credentials)
	AuthSourceStaticToken                  // Static bearer token (v0 compat, dev mode)
)

// CallerIdentity is the unified caller identity produced by both auth
// middlewares. Stored in request context for use by authorization checks.
type CallerIdentity struct {
	Sub    string
	Groups []string
	Role   Role
	Source AuthSource
}

type callerContextKey int

const callerKey callerContextKey = iota

// ContextWithCaller returns a new context carrying the given CallerIdentity.
func ContextWithCaller(ctx context.Context, c *CallerIdentity) context.Context {
	return context.WithValue(ctx, callerKey, c)
}

// CallerFromContext extracts the CallerIdentity from the context.
// Returns nil if no identity is present.
func CallerFromContext(ctx context.Context) *CallerIdentity {
	c, _ := ctx.Value(callerKey).(*CallerIdentity)
	return c
}

// DeriveRole determines the effective role for a set of groups by looking
// up each group in the role mapping cache and taking the highest-privilege match.
func DeriveRole(groups []string, cache *RoleMappingCache) Role {
	best := RoleNone
	for _, g := range groups {
		if r, ok := cache.Get(g); ok && r > best {
			best = r
		}
	}
	return best
}
