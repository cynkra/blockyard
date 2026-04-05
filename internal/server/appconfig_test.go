package server

import (
	"testing"

	"github.com/cynkra/blockyard/internal/config"
	"github.com/cynkra/blockyard/internal/db"
)

func TestAppImage(t *testing.T) {
	tests := []struct {
		name     string
		appImage string
		defImage string
		want     string
	}{
		{"app override", "custom:v2", "default:latest", "custom:v2"},
		{"server default", "", "default:latest", "default:latest"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			app := &db.AppRow{Image: tt.appImage}
			got := AppImage(app, tt.defImage)
			if got != tt.want {
				t.Errorf("AppImage() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestAppRuntime(t *testing.T) {
	tests := []struct {
		name    string
		app     *db.AppRow
		cfg     config.DockerConfig
		want    string
	}{
		{
			"app override",
			&db.AppRow{Runtime: "kata", AccessType: "public"},
			config.DockerConfig{Runtime: "runc", RuntimeDefaults: map[string]string{"public": "sysbox"}},
			"kata",
		},
		{
			"access type default",
			&db.AppRow{Runtime: "", AccessType: "public"},
			config.DockerConfig{Runtime: "runc", RuntimeDefaults: map[string]string{"public": "sysbox"}},
			"sysbox",
		},
		{
			"server default",
			&db.AppRow{Runtime: "", AccessType: "acl"},
			config.DockerConfig{Runtime: "runc"},
			"runc",
		},
		{
			"all empty",
			&db.AppRow{Runtime: "", AccessType: "acl"},
			config.DockerConfig{},
			"",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := AppRuntime(tt.app, tt.cfg)
			if got != tt.want {
				t.Errorf("AppRuntime() = %q, want %q", got, tt.want)
			}
		})
	}
}
