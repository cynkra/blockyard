// Package dockerauth resolves Docker registry credentials from the
// user's ~/.docker/config.json so that Go SDK callers can authenticate
// image pulls. The Docker daemon does not auto-enrich SDK requests
// with the CLI's credentials — each caller supplies its own
// X-Registry-Auth header — so SDK pulls go out anonymous unless we
// thread the auth through ourselves.
package dockerauth

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/distribution/reference"
	"github.com/moby/moby/api/types/registry"
)

// dockerHubHost is the canonical domain returned by reference.Domain
// for unqualified images such as "alpine:3.23".
const dockerHubHost = "docker.io"

// dockerHubLegacyKey is the historical index URL the Docker CLI writes
// under "auths" for Docker Hub credentials.
const dockerHubLegacyKey = "https://index.docker.io/v1/"

// RegistryAuthFor returns the base64url-encoded RegistryAuth string to
// pass as client.ImagePullOptions{RegistryAuth: ...} so the daemon
// forwards credentials to the registry when pulling ref.
//
// Returns ("", nil) when no credentials are configured for the ref's
// domain (anonymous pull) or when the config file is absent. Returns
// an error only for malformed refs or a corrupt config file.
func RegistryAuthFor(ref string) (string, error) {
	named, err := reference.ParseNormalizedNamed(ref)
	if err != nil {
		return "", fmt.Errorf("parse reference %q: %w", ref, err)
	}
	return registryAuthForDomain(reference.Domain(named), configPath())
}

func registryAuthForDomain(domain, path string) (string, error) {
	entry, ok, err := lookupAuth(domain, path)
	if err != nil || !ok {
		return "", err
	}

	cfg := registry.AuthConfig{
		IdentityToken: entry.IdentityToken,
		ServerAddress: domain,
	}
	if entry.Auth != "" {
		user, pass, err := decodeAuth(entry.Auth)
		if err != nil {
			return "", err
		}
		cfg.Username = user
		cfg.Password = pass
	}

	buf, err := json.Marshal(cfg)
	if err != nil {
		return "", err
	}
	return base64.URLEncoding.EncodeToString(buf), nil
}

type dockerConfigAuth struct {
	Auth          string `json:"auth"`
	IdentityToken string `json:"identitytoken,omitempty"`
}

type dockerConfig struct {
	Auths map[string]dockerConfigAuth `json:"auths"`
}

func lookupAuth(domain, path string) (dockerConfigAuth, bool, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return dockerConfigAuth{}, false, nil
	}
	if err != nil {
		return dockerConfigAuth{}, false, fmt.Errorf("read %s: %w", path, err)
	}
	var cfg dockerConfig
	if err := json.Unmarshal(data, &cfg); err != nil {
		return dockerConfigAuth{}, false, fmt.Errorf("parse %s: %w", path, err)
	}

	keys := []string{domain}
	if domain == dockerHubHost {
		keys = append(keys, dockerHubLegacyKey, "index.docker.io")
	}
	for _, k := range keys {
		if e, ok := cfg.Auths[k]; ok && (e.Auth != "" || e.IdentityToken != "") {
			return e, true, nil
		}
	}
	return dockerConfigAuth{}, false, nil
}

func configPath() string {
	if p := os.Getenv("DOCKER_CONFIG"); p != "" {
		return filepath.Join(p, "config.json")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return filepath.Join(".docker", "config.json")
	}
	return filepath.Join(home, ".docker", "config.json")
}

func decodeAuth(s string) (string, string, error) {
	raw, err := base64.StdEncoding.DecodeString(s)
	if err != nil {
		raw, err = base64.URLEncoding.DecodeString(s)
		if err != nil {
			return "", "", fmt.Errorf("decode auth: %w", err)
		}
	}
	user, pass, ok := strings.Cut(string(raw), ":")
	if !ok {
		return "", "", fmt.Errorf("invalid auth: expected user:pass")
	}
	return user, pass, nil
}
