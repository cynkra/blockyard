//go:build !minimal || process_backend

package orchestrator

import (
	"context"
	"strings"
	"testing"

	"github.com/cynkra/blockyard/internal/config"
)

func TestLoopbackAddrForPolling(t *testing.T) {
	tests := []struct {
		in   string
		want string
	}{
		{"127.0.0.1:8090", "127.0.0.1:8090"},
		{"0.0.0.0:8090", "127.0.0.1:8090"},
		{"[::]:8090", "127.0.0.1:8090"},
		{":8090", "127.0.0.1:8090"},
		{"example.com:8090", "example.com:8090"},
		{"192.168.1.5:8090", "192.168.1.5:8090"},
		// malformed: passed through
		{"garbage", "garbage"},
	}
	for _, tt := range tests {
		t.Run(tt.in, func(t *testing.T) {
			got := loopbackAddrForPolling(tt.in)
			if got != tt.want {
				t.Errorf("loopbackAddrForPolling(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

func TestAltBindRange(t *testing.T) {
	tests := []struct {
		name      string
		cfg       *config.Config
		wantFirst int
		wantLast  int
		wantErr   bool
	}{
		{
			name:      "nil update section uses default",
			cfg:       &config.Config{},
			wantFirst: 8090,
			wantLast:  8099,
		},
		{
			name:      "empty range uses default",
			cfg:       &config.Config{Update: &config.UpdateConfig{}},
			wantFirst: 8090,
			wantLast:  8099,
		},
		{
			name: "explicit range",
			cfg: &config.Config{
				Update: &config.UpdateConfig{AltBindRange: "9000-9010"},
			},
			wantFirst: 9000,
			wantLast:  9010,
		},
		{
			name: "malformed",
			cfg: &config.Config{
				Update: &config.UpdateConfig{AltBindRange: "not-a-range"},
			},
			wantErr: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			first, last, err := altBindRange(tt.cfg)
			if tt.wantErr {
				if err == nil {
					t.Errorf("altBindRange: expected error, got (%d, %d)", first, last)
				}
				return
			}
			if err != nil {
				t.Errorf("altBindRange: unexpected error %v", err)
				return
			}
			if first != tt.wantFirst || last != tt.wantLast {
				t.Errorf("altBindRange = (%d, %d), want (%d, %d)",
					first, last, tt.wantFirst, tt.wantLast)
			}
		})
	}
}

func TestSetEnv(t *testing.T) {
	env := []string{"FOO=old", "BAR=keep"}
	env = setEnv(env, "FOO", "new")
	if got := findEnv(env, "FOO"); got != "new" {
		t.Errorf("FOO = %q, want new", got)
	}
	if got := findEnv(env, "BAR"); got != "keep" {
		t.Errorf("BAR = %q, want keep", got)
	}

	env = setEnv(env, "BAZ", "added")
	if got := findEnv(env, "BAZ"); got != "added" {
		t.Errorf("BAZ = %q, want added", got)
	}
	if len(env) != 3 {
		t.Errorf("expected 3 entries, got %d: %v", len(env), env)
	}
}

func TestStripEnv(t *testing.T) {
	env := []string{"FOO=keep", "INVOCATION_ID=abc", "BAR=keep", "JOURNAL_STREAM=xyz"}
	env = stripEnv(env, "INVOCATION_ID", "JOURNAL_STREAM")
	if len(env) != 2 {
		t.Errorf("expected 2 entries, got %d: %v", len(env), env)
	}
	if findEnv(env, "INVOCATION_ID") != "" {
		t.Error("INVOCATION_ID should have been stripped")
	}
	if findEnv(env, "JOURNAL_STREAM") != "" {
		t.Error("JOURNAL_STREAM should have been stripped")
	}
	if findEnv(env, "FOO") != "keep" || findEnv(env, "BAR") != "keep" {
		t.Error("FOO/BAR should still be present")
	}
}

func findEnv(env []string, key string) string {
	prefix := key + "="
	for _, kv := range env {
		if strings.HasPrefix(kv, prefix) {
			return strings.TrimPrefix(kv, prefix)
		}
	}
	return ""
}

func TestProcessFactorySupportsRollback(t *testing.T) {
	f := NewProcessFactory(&config.Config{}, "1.0.0")
	if f.SupportsRollback() {
		t.Error("process factory must not support rollback")
	}
}

func TestProcessFactoryCurrentImageBase(t *testing.T) {
	f := NewProcessFactory(&config.Config{}, "1.0.0")
	// Must return a stable non-empty value — the orchestrator logs
	// base:tag pairs during Update.
	if f.CurrentImageBase(context.Background()) == "" {
		t.Error("CurrentImageBase should return a stable placeholder")
	}
}

func TestProcessFactoryCurrentImageTag(t *testing.T) {
	f := NewProcessFactory(&config.Config{}, "2.3.4")
	if got := f.CurrentImageTag(context.Background()); got != "2.3.4" {
		t.Errorf("CurrentImageTag = %q, want %q", got, "2.3.4")
	}
}
