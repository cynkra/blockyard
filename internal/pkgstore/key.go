package pkgstore

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"
	"strings"
)

// StoreKey computes the curated hash for a pak lockfile entry.
// The hash is SHA-256 of a NUL-delimited string of identity fields.
//
// Hash input format:
//
//	{RemoteType}\0{field1}\0{field2}[...\0{fieldN}]
func StoreKey(entry LockfileEntry) (string, error) {
	remoteType := entry.Metadata.RemoteType
	if remoteType == "" {
		remoteType = entry.Type
	}

	var fields []string
	switch remoteType {
	case "standard":
		fields = []string{entry.Package, entry.Version, entry.SHA256}
	case "github", "gitlab", "bitbucket", "git":
		fields = []string{
			entry.Package,
			entry.Metadata.RemoteSha,
			entry.Metadata.RemoteSubdir,
		}
	default:
		return "", fmt.Errorf(
			"unsupported RemoteType for store key: %q"+
				" (url, local, and svn are not supported)", remoteType)
	}

	input := remoteType + "\x00" + strings.Join(fields, "\x00")
	h := sha256.Sum256([]byte(input))
	return hex.EncodeToString(h[:]), nil
}

// StoreRef returns a compound "sourceHash/configHash" string for use
// in the per-worker package manifest (.packages.json).
func StoreRef(sourceHash, configHash string) string {
	return sourceHash + "/" + configHash
}

// ConfigHash computes the config hash from a LinkingTo dependency map.
// Entries are sorted by package name to ensure deterministic output.
// An empty map produces the canonical empty config hash.
func ConfigHash(linkingToKeys map[string]string) string {
	if len(linkingToKeys) == 0 {
		h := sha256.Sum256([]byte(""))
		return hex.EncodeToString(h[:])
	}

	pkgs := make([]string, 0, len(linkingToKeys))
	for pkg := range linkingToKeys {
		pkgs = append(pkgs, pkg)
	}
	sort.Strings(pkgs)

	var parts []string
	for _, pkg := range pkgs {
		parts = append(parts, pkg+"\x00"+linkingToKeys[pkg])
	}
	input := strings.Join(parts, "\x00")
	h := sha256.Sum256([]byte(input))
	return hex.EncodeToString(h[:])
}
