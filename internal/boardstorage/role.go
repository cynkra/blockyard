// Package boardstorage hosts the control-plane side of the board-storage
// feature (see #283/#284): startup SQL, per-user PG role provisioning,
// vault static-role registration. Runtime data access happens
// directly between the R worker and PostgreSQL/vault; nothing in this
// package is on that path.
package boardstorage

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
)

// maxRoleNameLen is PostgreSQL's NAMEDATALEN-1 (63 chars). Identifiers
// longer than this are silently truncated by the server, which would
// collide two subs whose normalized forms share a 63-char prefix;
// we truncate deterministically with a stable hash suffix instead.
const maxRoleNameLen = 63

// NormalizePgRole converts an OIDC `sub` claim to a valid PostgreSQL
// role name. The mapping is deterministic and idempotent across
// logins: the same `sub` always produces the same role name.
//
// Rules:
//
//   - Prefix `user_`.
//   - Lowercase ASCII letters; digits pass through.
//   - Every other rune (punctuation, non-ASCII, whitespace) → `_`.
//   - If the result exceeds 63 chars, truncate and append a stable
//     suffix derived from sha256(sub).
//
// The `user_` prefix is load-bearing: it namespaces per-user roles
// away from group roles (blockr_user), the app role, and the admin
// role, and lets vault policy templates match `user_*` for path
// capabilities.
func NormalizePgRole(sub string) string {
	var b strings.Builder
	b.Grow(len(sub) + len("user_"))
	b.WriteString("user_")
	for _, r := range sub {
		switch {
		case r >= 'A' && r <= 'Z':
			b.WriteRune(r + ('a' - 'A'))
		case (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9'):
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
	}
	name := b.String()
	if len(name) <= maxRoleNameLen {
		return name
	}
	h := sha256.Sum256([]byte(sub))
	// 4 bytes = 8 hex chars + underscore = 9-char suffix. Leaves 54
	// chars of normalized prefix before the collision-breaker.
	suffix := "_" + hex.EncodeToString(h[:4])
	return name[:maxRoleNameLen-len(suffix)] + suffix
}
