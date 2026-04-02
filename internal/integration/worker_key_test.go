package integration

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"testing"
)

// mockKVStore returns a Client backed by an in-memory KV v2 store.
func mockKVStore(t *testing.T) *Client {
	t.Helper()
	store := make(map[string]map[string]any) // path → data
	return mockBao(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Strip /v1/secret/data/ prefix to get the logical path.
		const prefix = "/v1/secret/data/"
		if len(r.URL.Path) <= len(prefix) {
			http.NotFound(w, r)
			return
		}
		path := r.URL.Path[len(prefix):]

		switch r.Method {
		case "GET":
			data, ok := store[path]
			if !ok {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			json.NewEncoder(w).Encode(map[string]any{
				"data": map[string]any{"data": data},
			})
		case "PUT":
			var body struct {
				Data map[string]any `json:"data"`
			}
			json.NewDecoder(r.Body).Decode(&body)
			store[path] = body.Data
			w.WriteHeader(http.StatusOK)
		default:
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		}
	}))
}

func TestResolveWorkerKeyRoundTrip(t *testing.T) {
	client := mockKVStore(t)
	ctx := context.Background()

	// First call: generates and stores.
	key1, err := ResolveWorkerKey(ctx, client)
	if err != nil {
		t.Fatal(err)
	}
	if len(key1) != 32 {
		t.Fatalf("expected 32-byte key, got %d", len(key1))
	}

	// Second call: reads existing.
	key2, err := ResolveWorkerKey(ctx, client)
	if err != nil {
		t.Fatal(err)
	}

	if !bytes.Equal(key1, key2) {
		t.Fatal("second call should return the same key")
	}
}
