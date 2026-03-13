package authz

import (
	"github.com/cynkra/blockyard/internal/auth"
)

// AccessKind distinguishes user grants from group grants.
type AccessKind string

const (
	AccessKindUser  AccessKind = "user"
	AccessKindGroup AccessKind = "group"
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
//	3. Explicit ACL grants (user + group) -> highest content role
//	4. Public app + authenticated caller with no grants -> RelationAnonymous
//	5. No match -> RelationNone
//
// accessType is the app's access_type column ("acl" or "public").
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

	// 3. ACL grants — collect all matching grants and take max role
	best := ContentRole(-1) // sentinel below ContentRoleViewer
	found := false
	for _, g := range grants {
		match := false
		switch g.Kind {
		case AccessKindUser:
			match = g.Principal == caller.Sub
		case AccessKindGroup:
			for _, cg := range caller.Groups {
				if cg == g.Principal {
					match = true
					break
				}
			}
		}
		if match && g.Role > best {
			best = g.Role
			found = true
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

	// 4. Public app — authenticated caller with no explicit grants
	if accessType == "public" {
		return RelationAnonymous
	}

	return RelationNone
}
