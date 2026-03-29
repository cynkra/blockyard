package main

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/cynkra/blockyard/internal/deploy"
	"github.com/cynkra/blockyard/internal/detect"
	"github.com/cynkra/blockyard/internal/manifest"
	"github.com/spf13/cobra"
)

func initCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "init <path>",
		Short: "Generate manifest.json without deploying",
		Long:  "Inspect a Shiny app directory and generate a manifest.json file.",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			jsonOutput := jsonFlag(cmd)
			dir := args[0]

			absDir, err := filepath.Abs(dir)
			if err != nil {
				exitErrorf(jsonOutput, "invalid path: %v", err)
			}
			if !detect.DirExists(absDir) {
				exitErrorf(jsonOutput, "directory not found: %s", dir)
			}

			pinFlag, _ := cmd.Flags().GetBool("pin")
			reposFlag, _ := cmd.Flags().GetString("repositories")

			manifestPath := filepath.Join(absDir, "manifest.json")

			// If manifest.json already exists, validate and exit.
			if detect.FileExists(manifestPath) {
				m, err := readAndValidateManifest(manifestPath)
				if err != nil {
					exitErrorf(jsonOutput, "%v", err)
				}
				if jsonOutput {
					printJSON(map[string]any{
						"status":   "exists",
						"manifest": manifestPath,
						"mode":     m.BuildMode().String(),
					})
				} else {
					fmt.Printf("manifest.json already exists (%s mode). Validated OK.\n", m.BuildMode())
				}
				return nil
			}

			det, warnings := detect.App(absDir, pinFlag)
			for _, w := range warnings {
				if !jsonOutput {
					fmt.Fprintf(os.Stderr, "Warning: %s\n", w)
				}
			}

			// Bare scripts can't generate a manifest.
			if det.InputCase == detect.CaseBareScripts {
				exitErrorf(jsonOutput, "cannot generate manifest from bare scripts; add a DESCRIPTION or renv.lock file, or use --pin")
			}

			if !jsonOutput {
				fmt.Println("Detected:")
				printKeyValue([][2]string{
					{"Name", det.Name},
					{"Mode", fmt.Sprintf("%s (entrypoint: %s)", det.Mode, det.Entrypoint)},
					{"Deps", det.DepsLabel},
					{"Repository", det.RepoLabel},
				})
				fmt.Println()
			}

			m, err := deploy.PrepareManifest(absDir, det, reposFlag)
			if err != nil {
				exitErrorf(jsonOutput, "%v", err)
			}

			if m == nil {
				exitErrorf(jsonOutput, "no manifest generated")
			}

			if err := m.Write(manifestPath); err != nil {
				exitErrorf(jsonOutput, "write manifest: %v", err)
			}

			if jsonOutput {
				printJSON(map[string]any{
					"status":   "created",
					"manifest": manifestPath,
					"mode":     m.BuildMode().String(),
				})
			} else {
				fmt.Println("Wrote manifest.json")
			}
			return nil
		},
	}
	cmd.Flags().Bool("pin", false, "Pin dependencies via renv snapshot (requires R + renv)")
	cmd.Flags().String("repositories", "", "Repository URLs (comma-separated)")
	return cmd
}

// readAndValidateManifest reads and validates a manifest file.
func readAndValidateManifest(path string) (*manifest.Manifest, error) {
	m, err := manifest.Read(path)
	if err != nil {
		return nil, fmt.Errorf("invalid manifest: %w", err)
	}
	return m, nil
}
