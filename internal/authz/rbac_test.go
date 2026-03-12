package authz_test

import (
	"testing"

	"github.com/cynkra/blockyard/internal/authz"
)

func TestContentRoleString(t *testing.T) {
	if authz.ContentRoleViewer.String() != "viewer" {
		t.Errorf("ContentRoleViewer.String() = %q", authz.ContentRoleViewer.String())
	}
	if authz.ContentRoleCollaborator.String() != "collaborator" {
		t.Errorf("ContentRoleCollaborator.String() = %q", authz.ContentRoleCollaborator.String())
	}
}

func TestParseContentRole(t *testing.T) {
	tests := []struct {
		input   string
		want    authz.ContentRole
		wantOk  bool
	}{
		{"viewer", authz.ContentRoleViewer, true},
		{"collaborator", authz.ContentRoleCollaborator, true},
		{"unknown", authz.ContentRoleViewer, false},
	}
	for _, tt := range tests {
		got, ok := authz.ParseContentRole(tt.input)
		if got != tt.want || ok != tt.wantOk {
			t.Errorf("ParseContentRole(%q) = %v, %v; want %v, %v",
				tt.input, got, ok, tt.want, tt.wantOk)
		}
	}
}

func TestAppRelationPermissions(t *testing.T) {
	tests := []struct {
		relation  authz.AppRelation
		proxy     bool
		deploy    bool
		startStop bool
		update    bool
		delete    bool
		acl       bool
	}{
		{authz.RelationNone, false, false, false, false, false, false},
		{authz.RelationAnonymous, true, false, false, false, false, false},
		{authz.RelationContentViewer, true, false, false, false, false, false},
		{authz.RelationContentCollaborator, true, true, true, true, false, false},
		{authz.RelationOwner, true, true, true, true, true, true},
		{authz.RelationAdmin, true, true, true, true, true, true},
	}

	for _, tt := range tests {
		if got := tt.relation.CanAccessProxy(); got != tt.proxy {
			t.Errorf("%v.CanAccessProxy() = %v, want %v", tt.relation, got, tt.proxy)
		}
		if got := tt.relation.CanDeploy(); got != tt.deploy {
			t.Errorf("%v.CanDeploy() = %v, want %v", tt.relation, got, tt.deploy)
		}
		if got := tt.relation.CanStartStop(); got != tt.startStop {
			t.Errorf("%v.CanStartStop() = %v, want %v", tt.relation, got, tt.startStop)
		}
		if got := tt.relation.CanUpdateConfig(); got != tt.update {
			t.Errorf("%v.CanUpdateConfig() = %v, want %v", tt.relation, got, tt.update)
		}
		if got := tt.relation.CanDelete(); got != tt.delete {
			t.Errorf("%v.CanDelete() = %v, want %v", tt.relation, got, tt.delete)
		}
		if got := tt.relation.CanManageACL(); got != tt.acl {
			t.Errorf("%v.CanManageACL() = %v, want %v", tt.relation, got, tt.acl)
		}
	}
}
