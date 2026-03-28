package main

import (
	"crypto/sha256"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/cynkra/blockyard/internal/manifest"
)

// detectResult holds the results of app detection from a source directory.
type detectResult struct {
	// Which input case was detected.
	InputCase inputCase
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

type inputCase int

const (
	caseManifest    inputCase = iota // 1a: manifest.json exists
	caseRenvLock                     // 1b: renv.lock exists
	casePinFlag                      // 1c: --pin, need R + renv
	caseDescription                  // 2a: DESCRIPTION exists
	caseBareScripts                  // 2b: bare scripts only
)

func (c inputCase) String() string {
	switch c {
	case caseManifest:
		return "manifest.json"
	case caseRenvLock:
		return "renv.lock"
	case casePinFlag:
		return "--pin (renv snapshot)"
	case caseDescription:
		return "DESCRIPTION"
	case caseBareScripts:
		return "bare scripts"
	default:
		return "unknown"
	}
}

// detectApp inspects the source directory and determines the input case,
// app mode, and entrypoint.
func detectApp(dir string, pinFlag bool) (*detectResult, []string) {
	name := filepath.Base(dir)
	mode := detectAppMode(dir)
	entrypoint := detectEntrypoint(dir)
	var warnings []string

	// Priority: manifest.json > renv.lock > DESCRIPTION > bare scripts.
	hasManifest := fileExists(filepath.Join(dir, "manifest.json"))
	hasRenvLock := fileExists(filepath.Join(dir, "renv.lock"))
	hasDescription := fileExists(filepath.Join(dir, "DESCRIPTION"))

	var ic inputCase
	var depsLabel, repoLabel string

	switch {
	case hasManifest:
		ic = caseManifest
		depsLabel = "manifest.json"
		if hasRenvLock {
			warnings = append(warnings, "Using manifest.json; ignoring renv.lock")
		}
		if hasDescription {
			warnings = append(warnings, "Using manifest.json; ignoring DESCRIPTION")
		}
	case hasRenvLock:
		ic = caseRenvLock
		depsLabel = "pinned (renv.lock found)"
		if hasDescription {
			warnings = append(warnings, "Using renv.lock; ignoring DESCRIPTION")
		}
	case pinFlag:
		ic = casePinFlag
		depsLabel = "pinned (--pin, will snapshot)"
	case hasDescription:
		ic = caseDescription
		depsLabel = "unpinned (DESCRIPTION)"
	default:
		ic = caseBareScripts
		depsLabel = "unpinned (server scans)"
	}

	if ic == caseRenvLock || ic == caseManifest {
		repoLabel = "(from lockfile/manifest)"
	} else {
		repoLabel = defaultRepoURL
	}

	return &detectResult{
		InputCase:  ic,
		Name:       name,
		Mode:       mode,
		Entrypoint: entrypoint,
		DepsLabel:  depsLabel,
		RepoLabel:  repoLabel,
	}, warnings
}

const defaultRepoURL = "https://p3m.dev/cran/latest"

// detectAppMode infers the application mode from directory contents.
func detectAppMode(dir string) string {
	return "shiny"
}

// detectEntrypoint returns the primary entrypoint file name.
func detectEntrypoint(dir string) string {
	if fileExists(filepath.Join(dir, "app.R")) {
		return "app.R"
	}
	if fileExists(filepath.Join(dir, "server.R")) {
		return "server.R"
	}
	return "app.R"
}

// computeFileChecksums walks the directory and computes SHA-256 checksums
// for all regular files, skipping hidden files and renv artifacts.
func computeFileChecksums(dir string) map[string]manifest.FileInfo {
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

// fileExists checks if a path is a regular file.
func fileExists(path string) bool {
	fi, err := os.Stat(path)
	return err == nil && !fi.IsDir()
}

// defaultRepositories returns the default repository list.
func defaultRepositories() []manifest.Repository {
	return []manifest.Repository{
		{Name: "CRAN", URL: defaultRepoURL},
	}
}
