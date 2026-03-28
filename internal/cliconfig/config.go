package cliconfig

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

// Config stores CLI credentials and server URL.
type Config struct {
	Server string `json:"server"`
	Token  string `json:"token"`
}

// Dir returns the XDG-compliant config directory for the CLI.
func Dir() string {
	if xdg := os.Getenv("XDG_CONFIG_HOME"); xdg != "" {
		return filepath.Join(xdg, "by")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return filepath.Join(".", ".config", "by")
	}
	return filepath.Join(home, ".config", "by")
}

// Path returns the full path to the config file.
func Path() string {
	return filepath.Join(Dir(), "config.json")
}

// Load loads credentials from the config file.
func Load() (*Config, error) {
	data, err := os.ReadFile(Path())
	if err != nil {
		if os.IsNotExist(err) {
			return &Config{}, nil
		}
		return nil, err
	}
	var cfg Config
	if err := json.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("parse config: %w", err)
	}
	return &cfg, nil
}

// Save writes credentials to the config file.
func Save(cfg *Config) error {
	dir := Dir()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(Path(), data, 0o600)
}

// ResolveCredentials returns the server URL and token, preferring env vars
// over the config file.
func ResolveCredentials() (serverURL, token string, err error) {
	serverURL = os.Getenv("BLOCKYARD_URL")
	token = os.Getenv("BLOCKYARD_TOKEN")

	if serverURL != "" && token != "" {
		return serverURL, token, nil
	}

	cfg, err := Load()
	if err != nil {
		return "", "", fmt.Errorf("load config: %w", err)
	}

	if serverURL == "" {
		serverURL = cfg.Server
	}
	if token == "" {
		token = cfg.Token
	}

	if serverURL == "" {
		return "", "", fmt.Errorf("no server configured; run 'by login' or set BLOCKYARD_URL")
	}
	if token == "" {
		return "", "", fmt.Errorf("no token configured; run 'by login' or set BLOCKYARD_TOKEN")
	}
	return serverURL, token, nil
}
