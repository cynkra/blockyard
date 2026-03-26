package auth_test

import (
	"context"
	"testing"

	"github.com/cynkra/blockyard/internal/auth"
)

func TestRoleOrdering(t *testing.T) {
	if !(auth.RoleNone < auth.RoleViewer) {
		t.Error("RoleNone should be less than RoleViewer")
	}
	if !(auth.RoleViewer < auth.RolePublisher) {
		t.Error("RoleViewer should be less than RolePublisher")
	}
	if !(auth.RolePublisher < auth.RoleAdmin) {
		t.Error("RolePublisher should be less than RoleAdmin")
	}
}

func TestRoleString(t *testing.T) {
	tests := []struct {
		role auth.Role
		want string
	}{
		{auth.RoleNone, "none"},
		{auth.RoleViewer, "viewer"},
		{auth.RolePublisher, "publisher"},
		{auth.RoleAdmin, "admin"},
	}
	for _, tt := range tests {
		if got := tt.role.String(); got != tt.want {
			t.Errorf("Role(%d).String() = %q, want %q", tt.role, got, tt.want)
		}
	}
}

func TestParseRole(t *testing.T) {
	tests := []struct {
		input string
		want  auth.Role
	}{
		{"admin", auth.RoleAdmin},
		{"publisher", auth.RolePublisher},
		{"viewer", auth.RoleViewer},
		{"none", auth.RoleNone},
		{"unknown", auth.RoleNone},
		{"", auth.RoleNone},
	}
	for _, tt := range tests {
		if got := auth.ParseRole(tt.input); got != tt.want {
			t.Errorf("ParseRole(%q) = %v, want %v", tt.input, got, tt.want)
		}
	}
}

func TestCanCreateApp(t *testing.T) {
	if auth.RoleNone.CanCreateApp() {
		t.Error("RoleNone should not be able to create apps")
	}
	if auth.RoleViewer.CanCreateApp() {
		t.Error("RoleViewer should not be able to create apps")
	}
	if !auth.RolePublisher.CanCreateApp() {
		t.Error("RolePublisher should be able to create apps")
	}
	if !auth.RoleAdmin.CanCreateApp() {
		t.Error("RoleAdmin should be able to create apps")
	}
}

func TestCanViewAllApps(t *testing.T) {
	if auth.RoleNone.CanViewAllApps() {
		t.Error("RoleNone should not view all apps")
	}
	if auth.RoleViewer.CanViewAllApps() {
		t.Error("RoleViewer should not view all apps")
	}
	if auth.RolePublisher.CanViewAllApps() {
		t.Error("RolePublisher should not view all apps")
	}
	if !auth.RoleAdmin.CanViewAllApps() {
		t.Error("RoleAdmin should view all apps")
	}
}

func TestCanManageRoles(t *testing.T) {
	if auth.RolePublisher.CanManageRoles() {
		t.Error("RolePublisher should not manage roles")
	}
	if !auth.RoleAdmin.CanManageRoles() {
		t.Error("RoleAdmin should manage roles")
	}
}

func TestCallerContextRoundTrip(t *testing.T) {
	caller := &auth.CallerIdentity{
		Sub:    "user-1",
		Role:   auth.RolePublisher,
		Source: auth.AuthSourcePAT,
	}

	ctx := auth.ContextWithCaller(context.Background(), caller)
	got := auth.CallerFromContext(ctx)
	if got == nil {
		t.Fatal("CallerFromContext returned nil")
	}
	if got.Sub != "user-1" {
		t.Errorf("Sub = %q, want %q", got.Sub, "user-1")
	}
	if got.Role != auth.RolePublisher {
		t.Errorf("Role = %v, want %v", got.Role, auth.RolePublisher)
	}
}

func TestCallerFromContextNil(t *testing.T) {
	got := auth.CallerFromContext(context.Background())
	if got != nil {
		t.Errorf("CallerFromContext on empty context = %v, want nil", got)
	}
}

func TestCanManageTags(t *testing.T) {
	if auth.RolePublisher.CanManageTags() {
		t.Error("RolePublisher should not manage tags")
	}
	if !auth.RoleAdmin.CanManageTags() {
		t.Error("RoleAdmin should manage tags")
	}
	if auth.RoleViewer.CanManageTags() {
		t.Error("RoleViewer should not manage tags")
	}
}

func TestDisplayName(t *testing.T) {
	c := &auth.CallerIdentity{Sub: "user-1", Name: "Alice"}
	if got := c.DisplayName(); got != "Alice" {
		t.Errorf("DisplayName() = %q, want %q", got, "Alice")
	}

	c2 := &auth.CallerIdentity{Sub: "user-2"}
	if got := c2.DisplayName(); got != "user-2" {
		t.Errorf("DisplayName() = %q, want %q", got, "user-2")
	}
}

func TestContextWithUser(t *testing.T) {
	u := &auth.AuthenticatedUser{Sub: "u1", AccessToken: "tok"}
	ctx := auth.ContextWithUser(context.Background(), u)
	got := auth.UserFromContext(ctx)
	if got == nil {
		t.Fatal("expected user from context")
	}
	if got.Sub != "u1" {
		t.Errorf("Sub = %q, want %q", got.Sub, "u1")
	}
}

func TestUserFromContextNil(t *testing.T) {
	got := auth.UserFromContext(context.Background())
	if got != nil {
		t.Error("expected nil for empty context")
	}
}

