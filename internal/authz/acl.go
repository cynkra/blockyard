package authz

import (
	"github.com/cynkra/blockyard/internal/auth"
)

// AccessKind distinguishes user grants from group grants.
type AccessKind string

const (
	AccessKindUser AccessKind = "user"
)

// AccessGrant represents a row from the app_access table.
type AccessGrant struct {
	AppID     string
	Principal string
	Kind      AccessKind
	Role      ContentRole
	GrantedBy string
	GrantedAt string
}

// EvaluateAccess determines the caller's relationship to a specific app.
//
// Evaluation order:
//
//	0. Public app + nil caller -> RelationAnonymous
//	1. System admin -> RelationAdmin (overrides all)
//	2. App owner -> RelationOwner
//	3. Explicit user ACL grants -> highest content role
//	4. logged_in app + authenticated caller -> RelationContentViewer
//	5. Public app + authenticated caller with no grants -> RelationAnonymous
//	6. No match -> RelationNone
//
// accessType is the app's access_type column ("acl", "logged_in", or "public").
// caller may be nil for unauthenticated requests to public apps.
func EvaluateAccess(
	caller *auth.CallerIdentity,
	appOwner string,
	grants []AccessGrant,
	accessType string,
) AppRelation {
	// 0. Unauthenticated caller — only allowed on public apps
	if caller == nil {
		if accessType == "public" {
			return RelationAnonymous
		}
		return RelationNone
	}

	// 1. System admin
	if caller.Role == auth.RoleAdmin {
		return RelationAdmin
	}

	// 2. Owner
	if caller.Sub == appOwner {
		return RelationOwner
	}

	// 3. User ACL grants — collect matching user grants and take max role
	best := ContentRole(-1) // sentinel below ContentRoleViewer
	found := false
	for _, g := range grants {
		if g.Kind == AccessKindUser && g.Principal == caller.Sub {
			if g.Role > best {
				best = g.Role
				found = true
			}
		}
	}

	if found {
		switch best {
		case ContentRoleCollaborator:
			return RelationContentCollaborator
		default:
			return RelationContentViewer
		}
	}

	// 4. logged_in app — any authenticated user gets viewer access
	if accessType == "logged_in" {
		return RelationContentViewer
	}

	// 5. Public app — authenticated caller with no explicit grants
	if accessType == "public" {
		return RelationAnonymous
	}

	return RelationNone
}
