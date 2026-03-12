package integration

import (
	"context"
	"fmt"
)

// EnrollCredential writes a user's credential for a service into
// OpenBao's KV v2 store at secret/data/users/{sub}/apikeys/{service}.
// Uses the admin token, not the user's scoped token.
func EnrollCredential(ctx context.Context, client *Client, sub, service string, data map[string]any) error {
	path := fmt.Sprintf("users/%s/apikeys/%s", sub, service)
	return client.KVWrite(ctx, path, data)
}
