package authz

// ContentRole is a per-content role granted via the app_access table.
// Ordered by privilege for max-wins resolution.
type ContentRole int

const (
	ContentRoleViewer       ContentRole = iota // Can use the app via proxy
	ContentRoleCollaborator                    // Can deploy, start/stop, update config
)

// String returns the lowercase name of the content role.
func (r ContentRole) String() string {
	switch r {
	case ContentRoleCollaborator:
		return "collaborator"
	default:
		return "viewer"
	}
}

// ParseContentRole converts a string to a ContentRole.
// Returns ContentRoleViewer and false for unrecognized values.
func ParseContentRole(s string) (ContentRole, bool) {
	switch s {
	case "collaborator":
		return ContentRoleCollaborator, true
	case "viewer":
		return ContentRoleViewer, true
	default:
		return ContentRoleViewer, false
	}
}

// AppRelation is the effective relationship between a caller and a specific
// app. Determines what operations the caller can perform.
type AppRelation int

const (
	RelationNone                AppRelation = iota // No access at all
	RelationAnonymous                              // Public app, unauthenticated user
	RelationContentViewer                          // Per-content viewer (ACL grant)
	RelationContentCollaborator                    // Per-content collaborator (ACL grant)
	RelationOwner                                  // App owner
	RelationAdmin                                  // System admin
)

// CanAccessProxy reports whether this relation allows using the app via proxy.
func (r AppRelation) CanAccessProxy() bool {
	return r > RelationNone
}

// CanDeploy reports whether this relation allows deploying bundles.
func (r AppRelation) CanDeploy() bool {
	return r >= RelationContentCollaborator
}

// CanStartStop reports whether this relation allows starting/stopping the app.
func (r AppRelation) CanStartStop() bool {
	return r >= RelationContentCollaborator
}

// CanUpdateConfig reports whether this relation allows updating app config.
func (r AppRelation) CanUpdateConfig() bool {
	return r >= RelationContentCollaborator
}

// CanDelete reports whether this relation allows deleting the app.
func (r AppRelation) CanDelete() bool {
	return r >= RelationOwner
}

// CanManageACL reports whether this relation allows managing ACL grants.
func (r AppRelation) CanManageACL() bool {
	return r >= RelationOwner
}
