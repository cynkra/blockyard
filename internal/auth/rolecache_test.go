package auth_test

import (
	"testing"

	"github.com/cynkra/blockyard/internal/auth"
	"github.com/cynkra/blockyard/internal/db"
)

func TestRoleMappingCacheGetSet(t *testing.T) {
	c := auth.NewRoleMappingCache()

	// Empty cache
	if _, ok := c.Get("admins"); ok {
		t.Error("empty cache should not have any mappings")
	}

	// Set and get
	c.Set("admins", auth.RoleAdmin)
	role, ok := c.Get("admins")
	if !ok || role != auth.RoleAdmin {
		t.Errorf("Get(admins) = %v, %v; want RoleAdmin, true", role, ok)
	}
}

func TestRoleMappingCacheRemove(t *testing.T) {
	c := auth.NewRoleMappingCache()
	c.Set("developers", auth.RolePublisher)
	c.Remove("developers")

	if _, ok := c.Get("developers"); ok {
		t.Error("removed mapping should not be found")
	}
}

func TestRoleMappingCacheLoad(t *testing.T) {
	database, err := db.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()

	// Insert test mappings
	if err := database.UpsertRoleMapping("admins", "admin"); err != nil {
		t.Fatal(err)
	}
	if err := database.UpsertRoleMapping("developers", "publisher"); err != nil {
		t.Fatal(err)
	}

	c := auth.NewRoleMappingCache()
	if err := c.Load(database); err != nil {
		t.Fatal(err)
	}

	role, ok := c.Get("admins")
	if !ok || role != auth.RoleAdmin {
		t.Errorf("Get(admins) = %v, %v; want RoleAdmin, true", role, ok)
	}

	role, ok = c.Get("developers")
	if !ok || role != auth.RolePublisher {
		t.Errorf("Get(developers) = %v, %v; want RolePublisher, true", role, ok)
	}

	if _, ok := c.Get("unknown"); ok {
		t.Error("unknown group should not be found")
	}
}
