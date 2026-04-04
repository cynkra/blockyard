package orchestrator

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/moby/moby/client"
)

// pullImage pulls the given image via the Docker API.
func (o *Orchestrator) pullImage(ctx context.Context, ref string) error {
	resp, err := o.docker.ImagePull(ctx, ref, client.ImagePullOptions{})
	if err != nil {
		return err
	}
	// Drain the response body to complete the pull.
	_, _ = io.Copy(io.Discard, resp)
	resp.Close()
	return nil
}

// waitReady polls /readyz on the new container until it returns 200.
// Returns the container's internal address (IP:port).
// Times out after WorkerStartTimeout (reuses existing config).
func (o *Orchestrator) waitReady(ctx context.Context, containerID string) (string, error) {
	timeout := o.cfg.Proxy.WorkerStartTimeout.Duration
	if timeout == 0 {
		timeout = 60 * time.Second
	}

	deadline := time.Now().Add(timeout)
	ticker := time.NewTicker(1 * time.Second)
	defer ticker.Stop()

	var addr string
	for {
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-ticker.C:
			if time.Now().After(deadline) {
				return "", fmt.Errorf("timeout after %s", timeout)
			}

			// Resolve address on each tick in case the container
			// hasn't received its IP yet.
			if addr == "" {
				var err error
				addr, err = o.containerAddr(ctx, containerID)
				if err != nil {
					continue // not ready yet
				}
			}

			if err := o.checkReady(ctx, addr); err == nil {
				return addr, nil
			}
		}
	}
}

// activate calls POST /api/v1/admin/activate on the new server.
// It uses an activation token passed to the clone as an env var.
func (o *Orchestrator) activate(ctx context.Context, addr string) error {
	url := "http://" + addr + "/api/v1/admin/activate"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, nil)
	if err != nil {
		return err
	}

	// Use the activation token generated during cloning.
	// Both servers know it: old generated it, new has it as env var.
	if o.activationToken != "" {
		req.Header.Set("Authorization", "Bearer "+o.activationToken)
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("POST activate: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("activate returned %d: %s", resp.StatusCode, string(body))
	}
	return nil
}

// killAndRemove stops and removes a container. Best-effort — logs
// errors but does not return them.
func (o *Orchestrator) killAndRemove(ctx context.Context, containerID string) {
	timeout := 10
	if _, err := o.docker.ContainerStop(ctx, containerID,
		client.ContainerStopOptions{Timeout: &timeout}); err != nil {
		o.log.Warn("stop container", "id", containerID[:12], "error", err)
	}
	if _, err := o.docker.ContainerRemove(ctx, containerID,
		client.ContainerRemoveOptions{Force: true}); err != nil {
		o.log.Warn("remove container", "id", containerID[:12], "error", err)
	}
}

// checkReady does a single /readyz check against the given address.
func (o *Orchestrator) checkReady(ctx context.Context, addr string) error {
	url := "http://" + addr + "/readyz"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}

	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("readyz returned %d", resp.StatusCode)
	}
	return nil
}

// containerAddr resolves the new container's IP address and port from
// its network settings.
func (o *Orchestrator) containerAddr(ctx context.Context, containerID string) (string, error) {
	result, err := o.docker.ContainerInspect(ctx, containerID,
		client.ContainerInspectOptions{})
	if err != nil {
		return "", fmt.Errorf("inspect container: %w", err)
	}

	// Get the port from the container's config (same as our port).
	port := o.listenPort()

	// Find the first valid IP address across all networks.
	if result.Container.NetworkSettings != nil {
		for _, ep := range result.Container.NetworkSettings.Networks {
			if ep.IPAddress.IsValid() {
				return ep.IPAddress.String() + ":" + port, nil
			}
		}
	}

	return "", fmt.Errorf("no IP address found for container %s", containerID[:12])
}

// listenPort extracts the port from the server's bind address.
func (o *Orchestrator) listenPort() string {
	bind := o.cfg.Server.Bind
	if idx := strings.LastIndex(bind, ":"); idx != -1 {
		return bind[idx+1:]
	}
	return "8080"
}

// generateActivationToken creates a cryptographically random token
// for authenticating the activate call between old and new servers.
func generateActivationToken() string {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		// Fallback to timestamp-based token if crypto/rand fails.
		return fmt.Sprintf("activation-%d", time.Now().UnixNano())
	}
	return hex.EncodeToString(b)
}
