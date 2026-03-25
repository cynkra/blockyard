package manifest

import (
	"fmt"
	"os"
	"strings"
)

// FromDescription builds an unpinned manifest from a DESCRIPTION file.
// DCF fields are JSON-ified as string values into the description object.
func FromDescription(
	descPath string,
	meta Metadata,
	files map[string]FileInfo,
	repos []Repository,
) (*Manifest, error) {
	data, err := os.ReadFile(descPath)
	if err != nil {
		return nil, fmt.Errorf("read DESCRIPTION: %w", err)
	}

	fields := parseDCF(data)

	// Extract only dependency-relevant fields.
	// Suggests is excluded: deps:: tells pak to read Imports and
	// Depends only, and the design explicitly excludes Suggests.
	desc := make(map[string]string)
	for _, key := range []string{
		"Imports", "Depends", "Remotes", "LinkingTo",
	} {
		if v, ok := fields[key]; ok {
			desc[key] = v
		}
	}

	m := &Manifest{
		Version:      currentVersion,
		Metadata:     meta,
		Repositories: repos,
		Description:  desc,
		Files:        files,
	}
	return m, m.Validate()
}

// parseDCF parses a Debian Control Format file (used by R DESCRIPTION
// files). Returns a map of field names to their string values.
// Continuation lines (leading whitespace) are joined to the previous field.
func parseDCF(data []byte) map[string]string {
	fields := make(map[string]string)
	var currentKey string

	for _, line := range strings.Split(string(data), "\n") {
		if len(line) == 0 {
			continue
		}
		if line[0] == ' ' || line[0] == '\t' {
			// Continuation line.
			if currentKey != "" {
				fields[currentKey] += "\n" + line
			}
			continue
		}
		if idx := strings.Index(line, ":"); idx > 0 {
			currentKey = line[:idx]
			fields[currentKey] = strings.TrimSpace(line[idx+1:])
		}
	}
	return fields
}
