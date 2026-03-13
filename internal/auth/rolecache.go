package auth

import (
	"sync"

	"github.com/cynkra/blockyard/internal/db"
)

// RoleMappingCache is an in-memory cache of group -> role mappings.
// Loaded from the database at startup. Updated synchronously when
// role mappings are modified via the management API.
type RoleMappingCache struct {
	mu       sync.RWMutex
	mappings map[string]Role
}

// NewRoleMappingCache creates an empty cache.
func NewRoleMappingCache() *RoleMappingCache {
	return &RoleMappingCache{
		mappings: make(map[string]Role),
	}
}

// Load populates the cache from the database.
func (c *RoleMappingCache) Load(database *db.DB) error {
	rows, err := database.ListRoleMappings()
	if err != nil {
		return err
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	c.mappings = make(map[string]Role, len(rows))
	for _, row := range rows {
		role := ParseRole(row.Role)
		if role != RoleNone {
			c.mappings[row.GroupName] = role
		}
	}
	return nil
}

// Get looks up the role for a group name.
func (c *RoleMappingCache) Get(group string) (Role, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	r, ok := c.mappings[group]
	return r, ok
}

// Set updates a mapping (called after DB write).
func (c *RoleMappingCache) Set(group string, role Role) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.mappings[group] = role
}

// Remove deletes a mapping (called after DB delete).
func (c *RoleMappingCache) Remove(group string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.mappings, group)
}
