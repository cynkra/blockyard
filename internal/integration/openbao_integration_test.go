//go:build openbao_test

package integration_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"testing"
	"time"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/image"
	"github.com/docker/docker/client"

	"github.com/cynkra/blockyard/internal/integration"
	"github.com/cynkra/blockyard/internal/testutil"
)

const (
	openbaoImage = "ghcr.io/openbao/openbao:latest"
)

var (
	openbaoURL   string
	containerID  string
	rootToken    string
	mockIdP      *testutil.MockIdP
)

func TestMain(m *testing.M) {
	ctx := context.Background()

	cli, err := client.NewClientWithOpts(client.FromEnv, client.WithAPIVersionNegotiation())
	if err != nil {
		fmt.Fprintf(os.Stderr, "docker client: %v\n", err)
		os.Exit(1)
	}
	defer cli.Close()

	// Pull image.
	reader, err := cli.ImagePull(ctx, openbaoImage, image.PullOptions{})
	if err != nil {
		fmt.Fprintf(os.Stderr, "image pull: %v\n", err)
		os.Exit(1)
	}
	io.Copy(io.Discard, reader)
	reader.Close()

	// Start MockIdP for JWT auth configuration.
	mockIdP = testutil.NewMockIdP()

	// Generate a root token for dev mode.
	rootToken = "root-test-token"

	// Create OpenBao dev server container.
	// Use host networking so OpenBao can reach the mock IdP on 127.0.0.1.
	baoPort := "8200"
	resp, err := cli.ContainerCreate(ctx,
		&container.Config{
			Image: openbaoImage,
			Cmd:   []string{"server", "-dev", "-dev-root-token-id=" + rootToken, "-dev-listen-address=0.0.0.0:" + baoPort},
			Env: []string{
				"BAO_DEV_ROOT_TOKEN_ID=" + rootToken,
			},
			Labels: map[string]string{"blockyard-test": "openbao"},
		},
		&container.HostConfig{
			NetworkMode: "host",
			CapAdd:      []string{"IPC_LOCK"},
		},
		nil, nil, "blockyard-openbao-test",
	)
	if err != nil {
		fmt.Fprintf(os.Stderr, "container create: %v\n", err)
		os.Exit(1)
	}
	containerID = resp.ID

	if err := cli.ContainerStart(ctx, containerID, container.StartOptions{}); err != nil {
		fmt.Fprintf(os.Stderr, "container start: %v\n", err)
		cli.ContainerRemove(ctx, containerID, container.RemoveOptions{Force: true})
		os.Exit(1)
	}

	openbaoURL = fmt.Sprintf("http://127.0.0.1:%s", baoPort)

	// Wait for OpenBao to be ready.
	deadline := time.Now().Add(30 * time.Second)
	ready := false
	for time.Now().Before(deadline) {
		resp, err := http.Get(openbaoURL + "/v1/sys/health")
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				ready = true
				break
			}
		}
		time.Sleep(500 * time.Millisecond)
	}
	if !ready {
		fmt.Fprintf(os.Stderr, "openbao did not become ready within 30s\n")
		cleanup(ctx, cli)
		os.Exit(1)
	}

	// Configure JWT auth method.
	if err := configureJWTAuth(ctx); err != nil {
		fmt.Fprintf(os.Stderr, "configure jwt auth: %v\n", err)
		cleanup(ctx, cli)
		os.Exit(1)
	}

	code := m.Run()
	mockIdP.Close()
	cleanup(ctx, cli)
	os.Exit(code)
}

func cleanup(ctx context.Context, cli *client.Client) {
	timeout := 10
	cli.ContainerStop(ctx, containerID, container.StopOptions{Timeout: &timeout})
	cli.ContainerRemove(ctx, containerID, container.RemoveOptions{Force: true})
}

// configureJWTAuth sets up the JWT auth method in OpenBao with the mock IdP.
func configureJWTAuth(ctx context.Context) error {
	httpClient := &http.Client{Timeout: 10 * time.Second}

	// 1. Enable JWT auth method.
	if err := vaultPost(httpClient, "/v1/sys/auth/jwt", map[string]any{
		"type": "jwt",
	}); err != nil {
		return fmt.Errorf("enable jwt auth: %w", err)
	}

	// 2. Configure JWT auth with mock IdP's JWKS.
	if err := vaultPost(httpClient, "/v1/auth/jwt/config", map[string]any{
		"jwks_url":         mockIdP.IssuerURL() + "/jwks",
		"default_role":     "blockyard-user",
		"bound_issuer":     mockIdP.IssuerURL(),
	}); err != nil {
		return fmt.Errorf("configure jwt auth: %w", err)
	}

	// 3. Create policy for blockyard users.
	policy := `
path "secret/data/users/{{identity.entity.aliases.` + "auth_jwt_*" + `.name}}/*" {
  capabilities = ["read"]
}
`
	if err := vaultPost(httpClient, "/v1/sys/policy/blockyard-user", map[string]any{
		"policy": policy,
	}); err != nil {
		return fmt.Errorf("create policy: %w", err)
	}

	// 4. Create blockyard-user role.
	if err := vaultPost(httpClient, "/v1/auth/jwt/role/blockyard-user", map[string]any{
		"role_type":       "jwt",
		"bound_audiences": []string{"blockyard"},
		"user_claim":      "sub",
		"token_policies":  []string{"blockyard-user"},
		"token_ttl":       "1h",
	}); err != nil {
		return fmt.Errorf("create role: %w", err)
	}

	return nil
}

func vaultPost(httpClient *http.Client, path string, data map[string]any) error {
	body, _ := json.Marshal(data)
	req, err := http.NewRequest("POST", openbaoURL+path, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("X-Vault-Token", rootToken)
	req.Header.Set("Content-Type", "application/json")

	resp, err := httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		respBody, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("status %d for %s: %s", resp.StatusCode, path, respBody)
	}
	io.Copy(io.Discard, resp.Body)
	return nil
}

func TestBootstrapReal(t *testing.T) {
	client := integration.NewClient(openbaoURL, func() string { return rootToken })
	if err := integration.Bootstrap(context.Background(), client, "jwt", false); err != nil {
		t.Fatalf("Bootstrap: %v", err)
	}
}

func TestHealthReal(t *testing.T) {
	client := integration.NewClient(openbaoURL, func() string { return rootToken })
	if err := client.Health(context.Background()); err != nil {
		t.Fatalf("Health: %v", err)
	}
}

func TestJWTLoginReal(t *testing.T) {
	client := integration.NewClient(openbaoURL, func() string { return rootToken })

	// Issue a JWT using the mock IdP.
	jwt := mockIdP.IssueJWT("test-user-1", []string{"testers"})

	token, ttl, err := client.JWTLogin(context.Background(), "jwt", jwt)
	if err != nil {
		t.Fatalf("JWTLogin: %v", err)
	}
	if token == "" {
		t.Error("expected non-empty token")
	}
	if ttl <= 0 {
		t.Errorf("expected positive TTL, got %v", ttl)
	}
}

func TestEnrollAndReadCredential(t *testing.T) {
	client := integration.NewClient(openbaoURL, func() string { return rootToken })

	sub := "test-user-enroll"
	service := "openai"

	// Write credential using admin token.
	err := integration.EnrollCredential(context.Background(), client, sub, service, map[string]any{
		"api_key": "sk-test-key-123",
	})
	if err != nil {
		t.Fatalf("EnrollCredential: %v", err)
	}

	// Read it back using admin token (for verification).
	data, err := client.KVRead(context.Background(), fmt.Sprintf("users/%s/apikeys/%s", sub, service), rootToken)
	if err != nil {
		t.Fatalf("KVRead: %v", err)
	}
	if data["api_key"] != "sk-test-key-123" {
		t.Errorf("api_key = %v, want sk-test-key-123", data["api_key"])
	}
}

func TestTokenScopingReal(t *testing.T) {
	client := integration.NewClient(openbaoURL, func() string { return rootToken })

	// Write a secret for user-a.
	err := integration.EnrollCredential(context.Background(), client, "user-a", "svc", map[string]any{
		"api_key": "key-a",
	})
	if err != nil {
		t.Fatalf("enroll user-a: %v", err)
	}

	// Get a scoped token for user-b.
	jwtB := mockIdP.IssueJWT("user-b", []string{"testers"})
	tokenB, _, err := client.JWTLogin(context.Background(), "jwt", jwtB)
	if err != nil {
		t.Fatalf("JWTLogin user-b: %v", err)
	}

	// user-b should not be able to read user-a's secrets.
	_, err = client.KVRead(context.Background(), "users/user-a/apikeys/svc", tokenB)
	if err == nil {
		t.Error("expected error when user-b reads user-a's secret")
	}
}
