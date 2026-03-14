package integration

import (
	"context"
	"fmt"
	"regexp"
)

// validPathComponent matches safe characters for Vault KV path segments.
// Rejects slashes, path traversal, and control characters.
var validPathComponent = regexp.MustCompile(`^[a-zA-Z0-9._@\-]+$`)

// EnrollCredential writes a user's credential for a service into
// OpenBao's KV v2 store at secret/data/users/{sub}/apikeys/{service}.
// Uses the admin token, not the user's scoped token.
func EnrollCredential(ctx context.Context, client *Client, sub, service string, data map[string]any) error {
	if !validPathComponent.MatchString(sub) {
		return fmt.Errorf("invalid subject identifier: %q", sub)
	}
	if !validPathComponent.MatchString(service) {
		return fmt.Errorf("invalid service identifier: %q", service)
	}
	path := fmt.Sprintf("users/%s/apikeys/%s", sub, service)
	return client.KVWrite(ctx, path, data)
}
