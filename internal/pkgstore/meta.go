package pkgstore

import (
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"
	"time"
)

// StoreConfigs represents the configs.json file at the source-hash level.
type StoreConfigs struct {
	SourceCompiled bool                         `json:"source_compiled"`
	LinkingTo      []string                     `json:"linkingto"`
	Configs        map[string]map[string]string `json:"configs"`
	// Configs key: config hash
	// Configs value: map of {linked_package: store_key}
	//   (empty map for packages without LinkingTo deps)
}

// ConfigMeta represents the per-config sidecar file.
type ConfigMeta struct {
	CreatedAt time.Time `json:"created_at"`
}

// WriteStoreConfigs atomically writes configs.json via write-to-temp +
// rename.
func WriteStoreConfigs(path string, sc StoreConfigs) error {
	data, err := json.MarshalIndent(sc, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil { //nolint:gosec // G306: package metadata, not secrets
		return err
	}
	return os.Rename(tmp, path)
}

// ReadStoreConfigs reads a configs.json file.
func ReadStoreConfigs(path string) (StoreConfigs, error) {
	data, err := os.ReadFile(path) //nolint:gosec // G304: reads package metadata from managed path
	if err != nil {
		return StoreConfigs{}, err
	}
	var sc StoreConfigs
	return sc, json.Unmarshal(data, &sc)
}

// WriteConfigMeta writes a per-config sidecar file.
func WriteConfigMeta(path string, meta ConfigMeta) error {
	data, err := json.MarshalIndent(meta, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0o644) //nolint:gosec // G306: package metadata, not secrets
}

// ResolveConfig reads configs.json for a package's source hash and
// returns the config hash that matches the current lockfile's
// LinkingTo store keys. Returns ("", false) if no matching config
// exists (miss) or if configs.json doesn't exist (never seen).
func (s *Store) ResolveConfig(
	pkg, sourceHash string, lf *Lockfile,
) (configHash string, ok bool) {
	sc, err := ReadStoreConfigs(s.ConfigsPath(pkg, sourceHash))
	if err != nil {
		return "", false
	}

	linkingToKeys := make(map[string]string)
	for _, linkedPkg := range sc.LinkingTo {
		key := lockfileStoreKey(lf, linkedPkg)
		if key != "" {
			linkingToKeys[linkedPkg] = key
		}
	}
	expected := ConfigHash(linkingToKeys)

	if _, exists := sc.Configs[expected]; exists {
		return expected, true
	}
	return "", false
}

// WriteIngestMeta writes the config sidecar and updates configs.json
// for a newly ingested package config.
func (s *Store) WriteIngestMeta(
	entry LockfileEntry, lf *Lockfile,
	sourceHash, configHash string, linkingToKeys map[string]string,
	sourceCompiled bool, linkingToNames []string,
) error {
	configsPath := s.ConfigsPath(entry.Package, sourceHash)
	sc, err := ReadStoreConfigs(configsPath)
	if err != nil {
		sc = StoreConfigs{
			SourceCompiled: sourceCompiled,
			LinkingTo:      linkingToNames,
			Configs:        make(map[string]map[string]string),
		}
	}
	sc.Configs[configHash] = linkingToKeys
	if err := WriteStoreConfigs(configsPath, sc); err != nil {
		return fmt.Errorf("write configs.json: %w", err)
	}

	meta := ConfigMeta{CreatedAt: time.Now()}
	metaPath := s.ConfigMetaPath(entry.Package, sourceHash, configHash)
	return WriteConfigMeta(metaPath, meta)
}

// IngestContext extracts compile-time context from an installed
// package's DESCRIPTION and computes the config hash from the
// lockfile. Returns the config hash, LinkingTo store key map,
// source_compiled flag, and sorted LinkingTo package names.
func IngestContext(
	descPath string, lf *Lockfile,
) (configHash string, linkingToKeys map[string]string,
	sourceCompiled bool, linkingToNames []string, err error) {

	desc, err := ParseDCF(descPath)
	if err != nil {
		return ConfigHash(nil), nil, false, nil, nil
	}

	sourceCompiled = strings.EqualFold(desc["NeedsCompilation"], "yes")
	linkingToKeys = make(map[string]string)

	if lt := desc["LinkingTo"]; lt != "" {
		linkingToNames = parsePkgList(lt)
		sort.Strings(linkingToNames)
		for _, linkedPkg := range linkingToNames {
			key := lockfileStoreKey(lf, linkedPkg)
			if key != "" {
				linkingToKeys[linkedPkg] = key
			}
		}
	}

	configHash = ConfigHash(linkingToKeys)
	return configHash, linkingToKeys, sourceCompiled, linkingToNames, nil
}

// ParseDCF reads a Debian Control File (DESCRIPTION) into a map.
func ParseDCF(path string) (map[string]string, error) {
	data, err := os.ReadFile(path) //nolint:gosec // G304: reads package metadata from managed path
	if err != nil {
		return nil, err
	}
	result := make(map[string]string)
	var currentKey, currentVal string
	for _, line := range strings.Split(string(data), "\n") {
		if strings.HasPrefix(line, " ") || strings.HasPrefix(line, "\t") {
			currentVal += " " + strings.TrimSpace(line)
		} else if idx := strings.IndexByte(line, ':'); idx > 0 {
			if currentKey != "" {
				result[currentKey] = strings.TrimSpace(currentVal)
			}
			currentKey = strings.TrimSpace(line[:idx])
			currentVal = strings.TrimSpace(line[idx+1:])
		}
	}
	if currentKey != "" {
		result[currentKey] = strings.TrimSpace(currentVal)
	}
	return result, nil
}

// parsePkgList splits a comma-separated DESCRIPTION field (e.g.,
// "Rcpp (>= 1.0.0), s2") into bare package names.
func parsePkgList(s string) []string {
	var result []string
	for _, part := range strings.Split(s, ",") {
		name := strings.TrimSpace(part)
		if idx := strings.IndexByte(name, '('); idx > 0 {
			name = strings.TrimSpace(name[:idx])
		}
		if name != "" {
			result = append(result, name)
		}
	}
	return result
}

// lockfileStoreKey computes the store key for a named package from
// the lockfile, returning "" if the package is not found.
func lockfileStoreKey(lf *Lockfile, pkg string) string {
	for _, entry := range lf.Packages {
		if entry.Package == pkg {
			key, err := StoreKey(entry)
			if err != nil {
				return ""
			}
			return key
		}
	}
	return ""
}
