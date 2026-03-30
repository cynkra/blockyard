package docker

import (
	"context"
	"testing"

	"github.com/cynkra/blockyard/internal/backend"
)

func TestParseMemoryLimit(t *testing.T) {
	tests := []struct {
		input string
		want  int64
		ok    bool
	}{
		{"512m", 512 * 1024 * 1024, true},
		{"1g", 1024 * 1024 * 1024, true},
		{"256mb", 256 * 1024 * 1024, true},
		{"100kb", 100 * 1024, true},
		{"1024", 1024, true},
		{"  2g  ", 2 * 1024 * 1024 * 1024, true},
		{"invalid", 0, false},
	}

	for _, tt := range tests {
		got, ok := ParseMemoryLimit(tt.input)
		if ok != tt.ok {
			t.Errorf("ParseMemoryLimit(%q) ok = %v, want %v", tt.input, ok, tt.ok)
			continue
		}
		if ok && got != tt.want {
			t.Errorf("ParseMemoryLimit(%q) = %d, want %d", tt.input, got, tt.want)
		}
	}
}

func TestExtractContainerIDFromCgroup(t *testing.T) {
	tests := []struct {
		line string
		want string
	}{
		{
			"0::/docker/abc123def456abc123def456abc123def456abc123def456abc123def456abcd",
			"abc123def456abc123def456abc123def456abc123def456abc123def456abcd",
		},
		{
			"0::/system.slice/docker-abc123def456abc123def456abc123def456abc123def456abc123def456abcd.scope",
			"abc123def456abc123def456abc123def456abc123def456abc123def456abcd",
		},
		{"0::/user.slice/user-1000.slice", ""},
		{"0::/docker/abc", ""}, // too short
	}

	for _, tt := range tests {
		got := extractContainerIDFromCgroup(tt.line)
		if got != tt.want {
			t.Errorf("extractContainerIDFromCgroup(%q) = %q, want %q", tt.line, got, tt.want)
		}
	}
}

func TestIsHex(t *testing.T) {
	tests := []struct {
		input string
		want  bool
	}{
		{"abc123", true},
		{"ABC123", true},
		{"0123456789abcdef", true},
		{"xyz", false},
		{"abc-123", false},
	}

	for _, tt := range tests {
		got := isHex(tt.input)
		if got != tt.want {
			t.Errorf("isHex(%q) = %v, want %v", tt.input, got, tt.want)
		}
	}
}

func TestWorkerLabels(t *testing.T) {
	spec := backend.WorkerSpec{
		AppID:    "app-1",
		WorkerID: "worker-1",
		Labels:   map[string]string{"custom": "value"},
	}
	labels := workerLabels(spec)

	expected := map[string]string{
		"dev.blockyard/managed":   "true",
		"dev.blockyard/app-id":    "app-1",
		"dev.blockyard/worker-id": "worker-1",
		"dev.blockyard/role":      "worker",
		"custom":                  "value",
	}
	for k, v := range expected {
		if labels[k] != v {
			t.Errorf("workerLabels[%q] = %q, want %q", k, labels[k], v)
		}
	}
}

func TestBuildLabels(t *testing.T) {
	spec := backend.BuildSpec{
		AppID:    "app-1",
		BundleID: "bundle-1",
		Labels:   map[string]string{},
	}
	labels := buildLabels(spec)

	expected := map[string]string{
		"dev.blockyard/managed":   "true",
		"dev.blockyard/app-id":    "app-1",
		"dev.blockyard/bundle-id": "bundle-1",
		"dev.blockyard/role":      "build",
	}
	for k, v := range expected {
		if labels[k] != v {
			t.Errorf("buildLabels[%q] = %q, want %q", k, labels[k], v)
		}
	}
}

func TestDetectMountModeNative(t *testing.T) {
	cfg, err := detectMountMode(context.Background(), nil, "", "/data/bundles")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Mode != MountModeNative {
		t.Errorf("expected MountModeNative, got %d", cfg.Mode)
	}
}

func TestValidIptablesComment(t *testing.T) {
	tests := []struct {
		input string
		want  bool
	}{
		{"blockyard-worker-abc123", true},
		{"my_comment", true},
		{"ABC", true},
		{"a1-b2_c3", true},
		{"", false},
		{"has space", false},
		{"semi;colon", false},
		{"quo\"te", false},
		{"back`tick", false},
		{"pipe|char", false},
		{"dollar$", false},
	}

	for _, tt := range tests {
		got := validIptablesComment(tt.input)
		if got != tt.want {
			t.Errorf("validIptablesComment(%q) = %v, want %v", tt.input, got, tt.want)
		}
	}
}

func TestDetectServerID_FromEnv(t *testing.T) {
	t.Setenv("BLOCKYARD_SERVER_ID", "abc123def456")
	got := detectServerID()
	if got != "abc123def456" {
		t.Errorf("detectServerID() = %q, want abc123def456", got)
	}
}

func TestNetworkLabels(t *testing.T) {
	labels := networkLabels("app-1", "worker-1")

	expected := map[string]string{
		"dev.blockyard/managed":   "true",
		"dev.blockyard/app-id":    "app-1",
		"dev.blockyard/worker-id": "worker-1",
	}
	for k, v := range expected {
		if labels[k] != v {
			t.Errorf("networkLabels[%q] = %q, want %q", k, labels[k], v)
		}
	}
}
