package detect

import (
	"crypto/sha256"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/cynkra/blockyard/internal/manifest"
)

// Result holds the results of app detection from a source directory.
type Result struct {
	// Which input case was detected.
	InputCase InputCase
	// App name (directory basename).
	Name string
	// App mode (shiny).
	Mode string
	// Entrypoint file.
	Entrypoint string
	// Dependency label for display.
	DepsLabel string
	// Repository URL for display.
	RepoLabel string
}

// InputCase represents how the app's dependencies are specified.
type InputCase int

const (
	CaseManifest    InputCase = iota // 1a: manifest.json exists
	CaseRenvLock                     // 1b: renv.lock exists
	CasePinFlag                      // 1c: --pin, need R + renv
	CaseDescription                  // 2a: DESCRIPTION exists
	CaseBareScripts                  // 2b: bare scripts only
)

func (c InputCase) String() string {
	switch c {
	case CaseManifest:
		return "manifest.json"
	case CaseRenvLock:
		return "renv.lock"
	case CasePinFlag:
		return "--pin (renv snapshot)"
	case CaseDescription:
		return "DESCRIPTION"
	case CaseBareScripts:
		return "bare scripts"
	default:
		return "unknown"
	}
}

const DefaultRepoURL = "https://p3m.dev/cran/latest"

// App inspects the source directory and determines the input case,
// app mode, and entrypoint.
func App(dir string, pinFlag bool) (*Result, []string) {
	name := filepath.Base(dir)
	mode := AppMode(dir)
	entrypoint := Entrypoint(dir)
	var warnings []string

	// Priority: manifest.json > renv.lock > DESCRIPTION > bare scripts.
	hasManifest := FileExists(filepath.Join(dir, "manifest.json"))
	hasRenvLock := FileExists(filepath.Join(dir, "renv.lock"))
	hasDescription := FileExists(filepath.Join(dir, "DESCRIPTION"))

	var ic InputCase
	var depsLabel, repoLabel string

	switch {
	case hasManifest:
		ic = CaseManifest
		depsLabel = "manifest.json"
		if hasRenvLock {
			warnings = append(warnings, "Using manifest.json; ignoring renv.lock")
		}
		if hasDescription {
			warnings = append(warnings, "Using manifest.json; ignoring DESCRIPTION")
		}
	case hasRenvLock:
		ic = CaseRenvLock
		depsLabel = "pinned (renv.lock found)"
		if hasDescription {
			warnings = append(warnings, "Using renv.lock; ignoring DESCRIPTION")
		}
	case pinFlag:
		ic = CasePinFlag
		depsLabel = "pinned (--pin, will snapshot)"
	case hasDescription:
		ic = CaseDescription
		depsLabel = "unpinned (DESCRIPTION)"
	default:
		ic = CaseBareScripts
		depsLabel = "unpinned (server scans)"
	}

	if ic == CaseRenvLock || ic == CaseManifest {
		repoLabel = "(from lockfile/manifest)"
	} else {
		repoLabel = DefaultRepoURL
	}

	return &Result{
		InputCase:  ic,
		Name:       name,
		Mode:       mode,
		Entrypoint: entrypoint,
		DepsLabel:  depsLabel,
		RepoLabel:  repoLabel,
	}, warnings
}

// AppMode infers the application mode from directory contents.
func AppMode(dir string) string {
	return "shiny"
}

// Entrypoint returns the primary entrypoint file name.
func Entrypoint(dir string) string {
	if FileExists(filepath.Join(dir, "app.R")) {
		return "app.R"
	}
	if FileExists(filepath.Join(dir, "server.R")) {
		return "server.R"
	}
	return "app.R"
}

// FileChecksums walks the directory and computes SHA-256 checksums
// for all regular files, skipping hidden files and renv artifacts.
func FileChecksums(dir string) map[string]manifest.FileInfo {
	files := make(map[string]manifest.FileInfo)
	_ = filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			// Skip hidden directories.
			if d != nil && d.IsDir() && strings.HasPrefix(d.Name(), ".") {
				return fs.SkipDir
			}
			return nil
		}
		rel, err := filepath.Rel(dir, path)
		if err != nil || strings.HasPrefix(rel, ".") {
			return nil
		}
		// Skip renv artifacts that may be generated during --pin.
		if rel == "renv.lock" || strings.HasPrefix(rel, "renv/") || strings.HasPrefix(rel, "renv\\") {
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return nil
		}
		files[rel] = manifest.FileInfo{
			Checksum: fmt.Sprintf("%x", sha256.Sum256(data)),
		}
		return nil
	})
	return files
}

// FileExists checks if a path is a regular file.
func FileExists(path string) bool {
	fi, err := os.Stat(path)
	return err == nil && !fi.IsDir()
}

// DirExists checks if a path is a directory.
func DirExists(path string) bool {
	fi, err := os.Stat(path)
	return err == nil && fi.IsDir()
}

// DefaultRepositories returns the default repository list.
func DefaultRepositories() []manifest.Repository {
	return []manifest.Repository{
		{Name: "CRAN", URL: DefaultRepoURL},
	}
}
