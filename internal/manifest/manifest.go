package manifest

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
)

const currentVersion = 1

// Manifest is the canonical interface between CLI and server. It has two
// shapes: pinned (with Packages) and unpinned (with Description). Both share
// an envelope (Version, Platform, Metadata, Repositories, Files).
type Manifest struct {
	Version      int                 `json:"version"`
	Platform     string              `json:"platform"`
	Metadata     Metadata            `json:"metadata"`
	Repositories []Repository        `json:"repositories"`
	Packages     map[string]Package  `json:"packages,omitempty"`
	Description  map[string]string   `json:"description,omitempty"`
	Files        map[string]FileInfo `json:"files"`
}

// Metadata holds deployment-level information about the app.
type Metadata struct {
	AppMode    string `json:"appmode"`
	Entrypoint string `json:"entrypoint"`
}

// Repository is a named R package repository with a platform-neutral URL.
type Repository struct {
	Name string `json:"Name"`
	URL  string `json:"URL"`
}

// Package holds the renv.lock fields consumed by the build pipeline.
// Only identity and source fields are mapped — record_to_ref() uses
// Source/Remote* to derive pkgdepends refs. Fields like Hash,
// Requirements, and DESCRIPTION metadata are not carried.
type Package struct {
	Package        string `json:"Package"`
	Version        string `json:"Version"`
	Source         string `json:"Source"`
	Repository     string `json:"Repository,omitempty"`
	RemoteType     string `json:"RemoteType,omitempty"`
	RemoteHost     string `json:"RemoteHost,omitempty"`
	RemoteUsername string `json:"RemoteUsername,omitempty"`
	RemoteRepo     string `json:"RemoteRepo,omitempty"`
	RemoteRef      string `json:"RemoteRef,omitempty"`
	RemoteSha      string `json:"RemoteSha,omitempty"`
	RemoteSubdir   string `json:"RemoteSubdir,omitempty"`
	RemoteUrl      string `json:"RemoteUrl,omitempty"`
}

// FileInfo holds the checksum for a file in the bundle.
type FileInfo struct {
	Checksum string `json:"checksum"`
}

// BuildMode describes whether the manifest specifies pinned or unpinned deps.
type BuildMode int

const (
	BuildModePinned   BuildMode = iota // manifest with packages
	BuildModeUnpinned                  // manifest with description
)

func (m BuildMode) String() string {
	switch m {
	case BuildModePinned:
		return "pinned"
	case BuildModeUnpinned:
		return "unpinned"
	default:
		return "unknown"
	}
}

// IsPinned returns true when the manifest carries explicit package records.
func (m *Manifest) IsPinned() bool { return len(m.Packages) > 0 }

// BuildMode returns the build mode implied by the manifest shape.
func (m *Manifest) BuildMode() BuildMode {
	if m.IsPinned() {
		return BuildModePinned
	}
	return BuildModeUnpinned
}

// Validate checks that the manifest is well-formed.
func (m *Manifest) Validate() error {
	if m.Version != currentVersion {
		return fmt.Errorf(
			"unsupported manifest version %d (server supports %d)",
			m.Version, currentVersion)
	}
	if len(m.Packages) > 0 && len(m.Description) > 0 {
		return errors.New(
			"manifest carries both packages and description; " +
				"must be one or the other")
	}
	if m.Metadata.Entrypoint == "" {
		return errors.New("manifest missing metadata.entrypoint")
	}
	for _, pkg := range m.Packages {
		if err := pkg.Validate(); err != nil {
			return fmt.Errorf("invalid package record: %w", err)
		}
	}
	return nil
}

// Validate checks that a package record has the fields required by
// record_to_ref() for its Source type.
func (p Package) Validate() error {
	if p.Package == "" {
		return errors.New("missing Package field")
	}
	if p.Source == "" {
		return fmt.Errorf("package %s: missing Source field", p.Package)
	}

	switch p.Source {
	case "Repository", "Bioconductor":
		if p.Version == "" {
			return fmt.Errorf("package %s: Source %q requires Version",
				p.Package, p.Source)
		}
	case "GitHub", "GitLab", "Bitbucket":
		for _, f := range []struct{ name, val string }{
			{"RemoteUsername", p.RemoteUsername},
			{"RemoteRepo", p.RemoteRepo},
			{"RemoteSha", p.RemoteSha},
		} {
			if f.val == "" {
				return fmt.Errorf("package %s: Source %q requires %s",
					p.Package, p.Source, f.name)
			}
		}
	case "git":
		if p.RemoteUrl == "" {
			return fmt.Errorf("package %s: Source \"git\" requires RemoteUrl",
				p.Package)
		}
	default:
		return fmt.Errorf("package %s: unsupported Source %q", p.Package, p.Source)
	}
	return nil
}

// Read loads and validates a manifest from a JSON file.
func Read(path string) (*Manifest, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var m Manifest
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, fmt.Errorf("parse manifest: %w", err)
	}
	if err := m.Validate(); err != nil {
		return nil, fmt.Errorf("invalid manifest: %w", err)
	}
	return &m, nil
}

// PakRefs returns the list of pak-style package references derived from
// the manifest. For pinned manifests this converts each package record;
// for unpinned manifests it returns ["deps::/app"] so pak scans the
// DESCRIPTION file inside the bundle.
func (m *Manifest) PakRefs() []string {
	if len(m.Packages) == 0 {
		return []string{"deps::/app"}
	}
	refs := make([]string, 0, len(m.Packages))
	for _, pkg := range m.Packages {
		refs = append(refs, pkg.PakRef())
	}
	return refs
}

// PakRef converts a single package record to a pak-style reference.
func (p Package) PakRef() string {
	switch p.Source {
	case "Bioconductor":
		return "bioc::" + p.Package + "@" + p.Version
	case "Repository":
		return p.Package + "@" + p.Version
	case "GitHub":
		return p.RemoteUsername + "/" + p.RemoteRepo + "@" + p.RemoteSha
	case "GitLab":
		return "gitlab::" + p.RemoteUsername + "/" + p.RemoteRepo + "@" + p.RemoteSha
	case "Bitbucket":
		return "bitbucket::" + p.RemoteUsername + "/" + p.RemoteRepo + "@" + p.RemoteSha
	case "git":
		return "git::" + p.RemoteUrl
	default:
		return p.Package
	}
}

// RepoLines returns the repositories as "Name=URL" lines suitable for
// writing to a text file that the R build script can parse without jsonlite.
func (m *Manifest) RepoLines() []string {
	lines := make([]string, 0, len(m.Repositories))
	for _, r := range m.Repositories {
		lines = append(lines, r.Name+"="+r.URL)
	}
	return lines
}

// Write serializes the manifest to a JSON file.
func (m *Manifest) Write(path string) error {
	data, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0o644)
}
