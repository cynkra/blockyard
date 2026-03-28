package main

import (
	"bufio"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strings"

	"github.com/spf13/cobra"
)

func loginCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "login",
		Short: "Store credentials interactively",
		Long:  "Authenticate with a Blockyard server by storing a personal access token.",
		RunE: func(cmd *cobra.Command, args []string) error {
			jsonOutput := jsonFlag(cmd)

			serverURL, _ := cmd.Flags().GetString("server")
			reader := bufio.NewReader(os.Stdin)

			if serverURL == "" {
				fmt.Print("Server URL: ")
				line, _ := reader.ReadString('\n')
				serverURL = strings.TrimSpace(line)
			}
			if serverURL == "" {
				exitErrorf(jsonOutput, "server URL is required")
			}
			serverURL = strings.TrimRight(serverURL, "/")

			// Open browser to profile page for PAT creation.
			profileURL := serverURL + "/profile#tokens"
			fmt.Println("Opening browser to create a token...")
			openBrowser(profileURL)

			fmt.Print("Paste your token: ")
			line, _ := reader.ReadString('\n')
			token := strings.TrimSpace(line)
			if token == "" {
				exitErrorf(jsonOutput, "token is required")
			}

			// Verify token by calling GET /api/v1/users/me.
			c := newClient(serverURL, token)
			resp, err := c.get("/api/v1/users/me")
			if err != nil {
				exitErrorf(jsonOutput, "failed to connect to server: %v", err)
			}
			var user struct {
				Sub  string `json:"sub"`
				Name string `json:"name"`
			}
			if err := decodeJSON(resp, &user); err != nil {
				exitErrorf(jsonOutput, "authentication failed: %v", err)
			}

			// Save credentials.
			cfg := &config{
				Server: serverURL,
				Token:  token,
			}
			if err := saveConfig(cfg); err != nil {
				exitErrorf(jsonOutput, "failed to save config: %v", err)
			}

			host := strings.TrimPrefix(strings.TrimPrefix(serverURL, "https://"), "http://")
			displayName := user.Name
			if displayName == "" {
				displayName = user.Sub
			}

			if jsonOutput {
				printJSON(map[string]string{
					"server": serverURL,
					"user":   displayName,
				})
			} else {
				fmt.Printf("Logged in to %s as %s.\n", host, displayName)
			}
			return nil
		},
	}
	cmd.Flags().String("server", "", "Server URL")
	return cmd
}

// openBrowser opens the given URL in the default browser.
func openBrowser(url string) {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		cmd = exec.Command("open", url)
	case "windows":
		cmd = exec.Command("cmd", "/c", "start", url)
	default:
		cmd = exec.Command("xdg-open", url)
	}
	// Best-effort; ignore errors (e.g., headless systems).
	_ = cmd.Start()
}
