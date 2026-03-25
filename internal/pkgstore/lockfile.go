package pkgstore

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
)

// LockfileMetadata holds the nested metadata object from a pak lockfile
// entry. Only the fields needed for store key computation are mapped.
type LockfileMetadata struct {
	RemoteType   string `json:"RemoteType"`
	RemoteSha    string `json:"RemoteSha"`
	RemoteSubdir string `json:"RemoteSubdir,omitempty"`
}

// LockfileEntry is a single package record from the pak lockfile.
type LockfileEntry struct {
	Package          string           `json:"package"`
	Version          string           `json:"version"`
	Type             string           `json:"type"`              // "standard", "github", etc.
	NeedsCompilation bool             `json:"needscompilation"`
	Metadata         LockfileMetadata `json:"metadata"`
	SHA256           string           `json:"sha256"`            // archive hash (top-level)
	Platform         string           `json:"platform"`          // per-pkg R triple, e.g. "x86_64-pc-linux-gnu"
	RVersion         string           `json:"rversion"`          // per-pkg short R version, e.g. "4.5"
}

// Lockfile is the top-level pak lockfile structure.
type Lockfile struct {
	LockfileVersion int             `json:"lockfile_version"`
	RVersion        string          `json:"r_version"`  // "R version 4.5.2 (2025-10-31)"
	OS              string          `json:"os"`          // "Ubuntu 24.04.2 LTS" (human-readable)
	Platform        string          `json:"platform"`    // "x86_64-pc-linux-gnu" (R triple)
	Packages        []LockfileEntry `json:"packages"`
}

// ReadLockfile reads and validates a pak lockfile from the given path.
func ReadLockfile(path string) (*Lockfile, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var lf Lockfile
	if err := json.Unmarshal(data, &lf); err != nil {
		return nil, fmt.Errorf("parse pak lockfile: %w", err)
	}
	if err := lf.Validate(); err != nil {
		return nil, fmt.Errorf("invalid pak lockfile %s: %w", path, err)
	}
	return &lf, nil
}

// Validate checks that the pak lockfile has the structure and fields
// we depend on.
func (lf *Lockfile) Validate() error {
	if lf.LockfileVersion != 1 {
		return fmt.Errorf(
			"unsupported lockfile_version %d (expected 1)",
			lf.LockfileVersion)
	}
	if len(lf.Packages) == 0 {
		return errors.New("lockfile has no packages")
	}
	for i, entry := range lf.Packages {
		if err := entry.Validate(); err != nil {
			return fmt.Errorf("packages[%d]: %w", i, err)
		}
	}
	return nil
}

// Validate checks that a lockfile entry has the fields needed for
// store key computation and platform detection.
func (e LockfileEntry) Validate() error {
	if e.Package == "" {
		return errors.New("missing \"package\" field")
	}
	if e.Version == "" {
		return fmt.Errorf("package %s: missing \"version\"", e.Package)
	}
	if e.RVersion == "" {
		return fmt.Errorf("package %s: missing \"rversion\"", e.Package)
	}
	if e.Platform == "" {
		return fmt.Errorf("package %s: missing \"platform\"", e.Package)
	}

	// RemoteType: prefer metadata.RemoteType, fall back to top-level type.
	remoteType := e.Metadata.RemoteType
	if remoteType == "" {
		remoteType = e.Type
	}
	if remoteType == "" {
		return fmt.Errorf("package %s: missing type/metadata.RemoteType",
			e.Package)
	}

	switch remoteType {
	case "standard":
		if e.SHA256 == "" {
			return fmt.Errorf(
				"package %s: type \"standard\" requires \"sha256\"",
				e.Package)
		}
	case "github", "gitlab", "bitbucket", "git":
		if e.Metadata.RemoteSha == "" {
			return fmt.Errorf(
				"package %s: type %q requires metadata.RemoteSha",
				e.Package, remoteType)
		}
	default:
		return fmt.Errorf("package %s: unsupported type %q"+
			" (url, local, and svn are not supported)",
			e.Package, remoteType)
	}
	return nil
}
