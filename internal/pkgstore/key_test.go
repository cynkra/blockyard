package pkgstore

import (
	"testing"
)

func TestStoreKey_Standard(t *testing.T) {
	entry := LockfileEntry{
		Package: "shiny",
		Version: "1.9.1",
		Type:    "standard",
		SHA256:  "abc123def456",
		Metadata: LockfileMetadata{
			RemoteType: "standard",
		},
		Platform: "x86_64-pc-linux-gnu",
		RVersion: "4.5",
	}
	key, err := StoreKey(entry)
	if err != nil {
		t.Fatal(err)
	}
	if key == "" {
		t.Fatal("empty key")
	}
	// Key should be deterministic.
	key2, _ := StoreKey(entry)
	if key != key2 {
		t.Errorf("non-deterministic: %s != %s", key, key2)
	}
}

func TestStoreKey_GitHub(t *testing.T) {
	entry := LockfileEntry{
		Package:  "mypackage",
		Version:  "0.1.0",
		Type:     "github",
		Platform: "x86_64-pc-linux-gnu",
		RVersion: "4.5",
		Metadata: LockfileMetadata{
			RemoteType: "github",
			RemoteSha:  "deadbeef1234567890",
		},
	}
	key, err := StoreKey(entry)
	if err != nil {
		t.Fatal(err)
	}
	if key == "" {
		t.Fatal("empty key")
	}
}

func TestStoreKey_GitHubSubdir(t *testing.T) {
	entry1 := LockfileEntry{
		Package:  "pkg",
		Version:  "1.0",
		Type:     "github",
		Platform: "x86_64-pc-linux-gnu",
		RVersion: "4.5",
		Metadata: LockfileMetadata{
			RemoteType:   "github",
			RemoteSha:    "abc123",
			RemoteSubdir: "",
		},
	}
	entry2 := entry1
	entry2.Metadata.RemoteSubdir = "subpkg"

	key1, _ := StoreKey(entry1)
	key2, _ := StoreKey(entry2)
	if key1 == key2 {
		t.Error("subdir should change the hash")
	}
}

func TestStoreKey_UnsupportedType(t *testing.T) {
	entry := LockfileEntry{
		Package:  "pkg",
		Version:  "1.0",
		Type:     "url",
		Platform: "x86_64-pc-linux-gnu",
		RVersion: "4.5",
		Metadata: LockfileMetadata{RemoteType: "url"},
	}
	_, err := StoreKey(entry)
	if err == nil {
		t.Error("expected error for unsupported type")
	}
}

func TestStoreKey_FallbackToType(t *testing.T) {
	entry := LockfileEntry{
		Package:  "shiny",
		Version:  "1.9.1",
		Type:     "standard",
		SHA256:   "abc123",
		Platform: "x86_64-pc-linux-gnu",
		RVersion: "4.5",
	}
	key, err := StoreKey(entry)
	if err != nil {
		t.Fatal(err)
	}
	if key == "" {
		t.Fatal("empty key when using Type fallback")
	}
}

func TestConfigHash_Empty(t *testing.T) {
	h1 := ConfigHash(nil)
	h2 := ConfigHash(map[string]string{})
	if h1 != h2 {
		t.Errorf("nil and empty map should produce the same hash")
	}
	if h1 == "" {
		t.Fatal("empty hash")
	}
}

func TestConfigHash_SingleDep(t *testing.T) {
	h := ConfigHash(map[string]string{"Rcpp": "key1"})
	if h == "" {
		t.Fatal("empty hash")
	}
	if h == ConfigHash(nil) {
		t.Error("single dep should differ from empty")
	}
}

func TestConfigHash_MultipleDeps(t *testing.T) {
	h := ConfigHash(map[string]string{
		"Rcpp": "key1",
		"s2":   "key2",
	})
	if h == "" {
		t.Fatal("empty hash")
	}
}

func TestConfigHash_OrderIndependent(t *testing.T) {
	h1 := ConfigHash(map[string]string{
		"Rcpp": "key1",
		"s2":   "key2",
	})
	h2 := ConfigHash(map[string]string{
		"s2":   "key2",
		"Rcpp": "key1",
	})
	if h1 != h2 {
		t.Errorf("order should not matter: %s != %s", h1, h2)
	}
}

func TestStoreRef(t *testing.T) {
	ref := StoreRef("source123", "config456")
	if ref != "source123/config456" {
		t.Errorf("got %q", ref)
	}
}
