package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

// config stores CLI credentials and server URL.
type config struct {
	Server string `json:"server"`
	Token  string `json:"token"`
}

// configDir returns the XDG-compliant config directory for the CLI.
func configDir() string {
	if xdg := os.Getenv("XDG_CONFIG_HOME"); xdg != "" {
		return filepath.Join(xdg, "by")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return filepath.Join(".", ".config", "by")
	}
	return filepath.Join(home, ".config", "by")
}

func configPath() string {
	return filepath.Join(configDir(), "config.json")
}

// loadConfig loads credentials from the config file.
func loadConfig() (*config, error) {
	data, err := os.ReadFile(configPath())
	if err != nil {
		if os.IsNotExist(err) {
			return &config{}, nil
		}
		return nil, err
	}
	var cfg config
	if err := json.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("parse config: %w", err)
	}
	return &cfg, nil
}

// saveConfig writes credentials to the config file.
func saveConfig(cfg *config) error {
	dir := configDir()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(configPath(), data, 0o600)
}

// resolveCredentials returns the server URL and token, preferring env vars
// over the config file.
func resolveCredentials() (serverURL, token string, err error) {
	serverURL = os.Getenv("BLOCKYARD_URL")
	token = os.Getenv("BLOCKYARD_TOKEN")

	if serverURL != "" && token != "" {
		return serverURL, token, nil
	}

	cfg, err := loadConfig()
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

// mustClient creates a client from resolved credentials or exits with an error.
func mustClient(jsonOutput bool) *client {
	serverURL, token, err := resolveCredentials()
	if err != nil {
		exitError(jsonOutput, err)
	}
	return newClient(serverURL, token)
}

// mustStreamingClient creates a streaming client from resolved credentials.
func mustStreamingClient(jsonOutput bool) *client {
	serverURL, token, err := resolveCredentials()
	if err != nil {
		exitError(jsonOutput, err)
	}
	return newStreamingClient(serverURL, token)
}
