package main

import (
	"flag"
	"log/slog"
	"os"

	"github.com/cynkra/blockyard/internal/config"
)

func main() {
	configPath := flag.String("config", "blockyard.toml", "path to config file")
	flag.Parse()

	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{
		Level: slog.LevelInfo,
	})))

	cfg, err := config.Load(*configPath)
	if err != nil {
		slog.Error("failed to load config", "error", err)
		os.Exit(1)
	}

	slog.Info("loaded config", "bind", cfg.Server.Bind)

	// Server wiring comes in later phases.
}
