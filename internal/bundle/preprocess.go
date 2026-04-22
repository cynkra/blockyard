package bundle

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/cynkra/blockyard/internal/backend"
)

// preProcess runs pak::scan_deps() in a container to discover
// dependencies from bare scripts and generates a synthetic DESCRIPTION.
func preProcess(ctx context.Context, be backend.Backend,
	pakPath string, p RestoreParams) error {

	outputDir, err := os.MkdirTemp(p.BasePath, ".preprocess-")
	if err != nil {
		return fmt.Errorf("create preprocess output dir: %w", err)
	}
	defer os.RemoveAll(outputDir) //nolint:errcheck // best-effort cleanup

	rScript := `
		Sys.setenv(R_USER_CACHE_DIR = "/output")
		.libPaths(c("/pak", .libPaths()))
		library(pak)
		# Make pak's bundled dependencies (desc) available.
		pak_lib <- system.file("library", package = "pak")
		if (nzchar(pak_lib) && dir.exists(pak_lib)) {
		  .libPaths(c(pak_lib, .libPaths()))
		}
		deps <- pak::scan_deps("/app")
		pkgs <- unique(deps$package[deps$type == "prod"])
		dsc <- desc::desc("!new")
		dsc$set(Package = "app", Version = "0.0.1")
		for (p in pkgs) dsc$set_dep(p, type = "Imports")
		dsc$write("/output/DESCRIPTION")
	`

	spec := backend.BuildSpec{
		AppID:    p.AppID,
		BundleID: p.BundleID + "-preprocess",
		Image:    p.Image,
		Cmd:      []string{"Rscript", "--vanilla", "-e", rScript},
		Mounts: []backend.MountEntry{
			{Source: p.Paths.Unpacked, Target: "/app", ReadOnly: true},
			{Source: pakPath, Target: "/pak", ReadOnly: true},
			{Source: outputDir, Target: "/output", ReadOnly: false},
		},
		Labels: map[string]string{
			"dev.blockyard/managed": "true",
			"dev.blockyard/role":    "build",
		},
	}

	result, err := be.Build(ctx, spec)
	if err != nil {
		return fmt.Errorf("preprocess: %w", err)
	}
	if !result.Success {
		return fmt.Errorf("script scanning failed (exit %d): %s",
			result.ExitCode, lastLines(result.Logs, 10))
	}

	// Copy synthetic DESCRIPTION into the unpacked bundle dir.
	src := filepath.Join(outputDir, "DESCRIPTION")
	dst := filepath.Join(p.Paths.Unpacked, "DESCRIPTION")
	if err := copyFile(src, dst); err != nil {
		return fmt.Errorf("copy DESCRIPTION: %w", err)
	}
	return nil
}

func copyFile(src, dst string) error {
	in, err := os.Open(src) //nolint:gosec // G304: opens bundle file from managed path
	if err != nil {
		return err
	}
	defer in.Close()

	out, err := os.Create(dst) //nolint:gosec // G304: creates processed bundle at managed path
	if err != nil {
		return err
	}

	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}

func lastLines(s string, n int) string {
	lines := splitLines(s)
	if len(lines) <= n {
		return s
	}
	result := ""
	for _, l := range lines[len(lines)-n:] {
		result += l + "\n"
	}
	return result
}

func splitLines(s string) []string {
	var lines []string
	start := 0
	for i := 0; i < len(s); i++ {
		if s[i] == '\n' {
			lines = append(lines, s[start:i])
			start = i + 1
		}
	}
	if start < len(s) {
		lines = append(lines, s[start:])
	}
	return lines
}
