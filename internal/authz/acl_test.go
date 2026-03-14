package authz_test

import (
	"testing"

	"github.com/cynkra/blockyard/internal/auth"
	"github.com/cynkra/blockyard/internal/authz"
)

func TestEvaluateAccessNilCallerACL(t *testing.T) {
	rel := authz.EvaluateAccess(nil, "owner-1", nil, "acl")
	if rel != authz.RelationNone {
		t.Errorf("nil caller + acl app = %v, want RelationNone", rel)
	}
}

func TestEvaluateAccessNilCallerPublic(t *testing.T) {
	rel := authz.EvaluateAccess(nil, "owner-1", nil, "public")
	if rel != authz.RelationAnonymous {
		t.Errorf("nil caller + public app = %v, want RelationAnonymous", rel)
	}
}

func TestEvaluateAccessAdmin(t *testing.T) {
	caller := &auth.CallerIdentity{Sub: "admin-1", Role: auth.RoleAdmin}
	rel := authz.EvaluateAccess(caller, "other-owner", nil, "acl")
	if rel != authz.RelationAdmin {
		t.Errorf("admin = %v, want RelationAdmin", rel)
	}
}

func TestEvaluateAccessAdminOverridesOwner(t *testing.T) {
	// Admin who is also owner should get RelationAdmin (admin takes precedence)
	caller := &auth.CallerIdentity{Sub: "user-1", Role: auth.RoleAdmin}
	rel := authz.EvaluateAccess(caller, "user-1", nil, "acl")
	if rel != authz.RelationAdmin {
		t.Errorf("admin who is also owner = %v, want RelationAdmin", rel)
	}
}

func TestEvaluateAccessOwner(t *testing.T) {
	caller := &auth.CallerIdentity{Sub: "user-1", Role: auth.RolePublisher}
	rel := authz.EvaluateAccess(caller, "user-1", nil, "acl")
	if rel != authz.RelationOwner {
		t.Errorf("owner = %v, want RelationOwner", rel)
	}
}

func TestEvaluateAccessUserViewerGrant(t *testing.T) {
	caller := &auth.CallerIdentity{Sub: "user-2", Role: auth.RoleViewer}
	grants := []authz.AccessGrant{
		{Principal: "user-2", Kind: authz.AccessKindUser, Role: authz.ContentRoleViewer},
	}
	rel := authz.EvaluateAccess(caller, "user-1", grants, "acl")
	if rel != authz.RelationContentViewer {
		t.Errorf("user viewer grant = %v, want RelationContentViewer", rel)
	}
}

func TestEvaluateAccessUserCollaboratorGrant(t *testing.T) {
	caller := &auth.CallerIdentity{Sub: "user-2", Role: auth.RolePublisher}
	grants := []authz.AccessGrant{
		{Principal: "user-2", Kind: authz.AccessKindUser, Role: authz.ContentRoleCollaborator},
	}
	rel := authz.EvaluateAccess(caller, "user-1", grants, "acl")
	if rel != authz.RelationContentCollaborator {
		t.Errorf("user collaborator grant = %v, want RelationContentCollaborator", rel)
	}
}

func TestEvaluateAccessMaxUserGrantWins(t *testing.T) {
	// User has both viewer and collaborator grants -> collaborator wins
	caller := &auth.CallerIdentity{Sub: "user-2", Role: auth.RoleNone}
	grants := []authz.AccessGrant{
		{Principal: "user-2", Kind: authz.AccessKindUser, Role: authz.ContentRoleViewer},
		{Principal: "user-2", Kind: authz.AccessKindUser, Role: authz.ContentRoleCollaborator},
	}
	rel := authz.EvaluateAccess(caller, "user-1", grants, "acl")
	if rel != authz.RelationContentCollaborator {
		t.Errorf("max grant = %v, want RelationContentCollaborator", rel)
	}
}

func TestEvaluateAccessLoggedInAuthenticated(t *testing.T) {
	caller := &auth.CallerIdentity{Sub: "user-2", Role: auth.RoleViewer}
	rel := authz.EvaluateAccess(caller, "user-1", nil, "logged_in")
	if rel != authz.RelationContentViewer {
		t.Errorf("logged_in + authenticated = %v, want RelationContentViewer", rel)
	}
}

func TestEvaluateAccessLoggedInUnauthenticated(t *testing.T) {
	rel := authz.EvaluateAccess(nil, "owner-1", nil, "logged_in")
	if rel != authz.RelationNone {
		t.Errorf("logged_in + unauthenticated = %v, want RelationNone", rel)
	}
}

func TestEvaluateAccessNoGrantsACL(t *testing.T) {
	caller := &auth.CallerIdentity{Sub: "user-2", Role: auth.RolePublisher}
	rel := authz.EvaluateAccess(caller, "user-1", nil, "acl")
	if rel != authz.RelationNone {
		t.Errorf("no grants + acl = %v, want RelationNone", rel)
	}
}

func TestEvaluateAccessNoGrantsPublic(t *testing.T) {
	caller := &auth.CallerIdentity{Sub: "user-2", Role: auth.RolePublisher}
	rel := authz.EvaluateAccess(caller, "user-1", nil, "public")
	if rel != authz.RelationAnonymous {
		t.Errorf("no grants + public = %v, want RelationAnonymous", rel)
	}
}

func TestEvaluateAccessAnonymousCanProxy(t *testing.T) {
	rel := authz.EvaluateAccess(nil, "owner-1", nil, "public")
	if !rel.CanAccessProxy() {
		t.Error("RelationAnonymous should be able to access proxy")
	}
	if rel.CanDeploy() {
		t.Error("RelationAnonymous should not be able to deploy")
	}
}
