package manifest

import (
	"encoding/json"
	"fmt"
	"os"
)

// renvLock mirrors the relevant structure of renv.lock (JSON).
// Works with both v1 (minimal records) and v2 (full DESCRIPTION)
// lockfile formats — the Package struct maps only the fields we
// need; extra v2 fields are silently dropped during unmarshaling.
type renvLock struct {
	R struct {
		Version      string       `json:"Version"`
		Repositories []Repository `json:"Repositories"`
	} `json:"R"`
	Packages map[string]Package `json:"Packages"`
}

// FromRenvLock converts an renv.lock file to a pinned manifest.
// Package identity and source fields are preserved unchanged.
// Extra DESCRIPTION fields from v2 lockfiles are not carried.
func FromRenvLock(
	lockPath string,
	meta Metadata,
	files map[string]FileInfo,
) (*Manifest, error) {
	data, err := os.ReadFile(lockPath)
	if err != nil {
		return nil, fmt.Errorf("read renv.lock: %w", err)
	}

	var lock renvLock
	if err := json.Unmarshal(data, &lock); err != nil {
		return nil, fmt.Errorf("parse renv.lock: %w", err)
	}

	m := &Manifest{
		Version:      currentVersion,
		Platform:     lock.R.Version,
		Metadata:     meta,
		Repositories: lock.R.Repositories,
		Packages:     lock.Packages,
		Files:        files,
	}
	return m, m.Validate()
}
