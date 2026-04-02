package preflight

import (
	"context"
	"errors"
	"testing"
)

func TestCheckDatabase(t *testing.T) {
	t.Run("ok", func(t *testing.T) {
		deps := RuntimeDeps{DBPing: func(ctx context.Context) error { return nil }}
		res := checkDatabase(context.Background(), deps)
		if res.Severity != SeverityOK {
			t.Errorf("expected OK, got %v", res.Severity)
		}
		if res.Category != "runtime" {
			t.Errorf("expected runtime category, got %q", res.Category)
		}
	})

	t.Run("error", func(t *testing.T) {
		deps := RuntimeDeps{DBPing: func(ctx context.Context) error { return errors.New("connection refused") }}
		res := checkDatabase(context.Background(), deps)
		if res.Severity != SeverityError {
			t.Errorf("expected Error, got %v", res.Severity)
		}
	})

	t.Run("nil func", func(t *testing.T) {
		deps := RuntimeDeps{}
		res := checkDatabase(context.Background(), deps)
		if res.Severity != SeverityOK {
			t.Errorf("expected OK when DBPing is nil, got %v", res.Severity)
		}
	})
}

func TestCheckDocker(t *testing.T) {
	t.Run("ok", func(t *testing.T) {
		deps := RuntimeDeps{DockerPing: func(ctx context.Context) error { return nil }}
		res := checkDocker(context.Background(), deps)
		if res.Severity != SeverityOK {
			t.Errorf("expected OK, got %v", res.Severity)
		}
	})

	t.Run("error", func(t *testing.T) {
		deps := RuntimeDeps{DockerPing: func(ctx context.Context) error { return errors.New("socket gone") }}
		res := checkDocker(context.Background(), deps)
		if res.Severity != SeverityError {
			t.Errorf("expected Error, got %v", res.Severity)
		}
	})
}

func TestCheckRedis(t *testing.T) {
	t.Run("ok", func(t *testing.T) {
		deps := RuntimeDeps{RedisPing: func(ctx context.Context) error { return nil }}
		res := checkRedis(context.Background(), deps)
		if res.Severity != SeverityOK {
			t.Errorf("expected OK, got %v", res.Severity)
		}
	})

	t.Run("error", func(t *testing.T) {
		deps := RuntimeDeps{RedisPing: func(ctx context.Context) error { return errors.New("timeout") }}
		res := checkRedis(context.Background(), deps)
		if res.Severity != SeverityError {
			t.Errorf("expected Error, got %v", res.Severity)
		}
	})
}

func TestCheckIDP(t *testing.T) {
	t.Run("ok", func(t *testing.T) {
		deps := RuntimeDeps{IDPCheck: func(ctx context.Context) error { return nil }}
		res := checkIDP(context.Background(), deps)
		if res.Severity != SeverityOK {
			t.Errorf("expected OK, got %v", res.Severity)
		}
	})

	t.Run("error", func(t *testing.T) {
		deps := RuntimeDeps{IDPCheck: func(ctx context.Context) error { return errors.New("unreachable") }}
		res := checkIDP(context.Background(), deps)
		if res.Severity != SeverityError {
			t.Errorf("expected Error, got %v", res.Severity)
		}
	})
}

func TestCheckVault(t *testing.T) {
	t.Run("ok", func(t *testing.T) {
		deps := RuntimeDeps{VaultCheck: func(ctx context.Context) error { return nil }}
		res := checkVault(context.Background(), deps)
		if res.Severity != SeverityOK {
			t.Errorf("expected OK, got %v", res.Severity)
		}
	})

	t.Run("error", func(t *testing.T) {
		deps := RuntimeDeps{VaultCheck: func(ctx context.Context) error { return errors.New("sealed") }}
		res := checkVault(context.Background(), deps)
		if res.Severity != SeverityError {
			t.Errorf("expected Error, got %v", res.Severity)
		}
	})
}

func TestCheckVaultToken(t *testing.T) {
	t.Run("healthy", func(t *testing.T) {
		deps := RuntimeDeps{VaultTokenOK: func() bool { return true }}
		res := checkVaultToken(deps)
		if res.Severity != SeverityOK {
			t.Errorf("expected OK, got %v", res.Severity)
		}
	})

	t.Run("unhealthy", func(t *testing.T) {
		deps := RuntimeDeps{VaultTokenOK: func() bool { return false }}
		res := checkVaultToken(deps)
		if res.Severity != SeverityError {
			t.Errorf("expected Error, got %v", res.Severity)
		}
	})
}

func TestCheckDiskSpace(t *testing.T) {
	// Use the OS temp dir which should exist and have plenty of space.
	res := checkDiskSpace(t.TempDir())
	if res.Name != "disk_space" {
		t.Errorf("expected name disk_space, got %q", res.Name)
	}
	if res.Category != "runtime" {
		t.Errorf("expected runtime category, got %q", res.Category)
	}
	// On CI/local, temp dir should have plenty of space → OK.
	if res.Severity != SeverityOK {
		t.Logf("disk space check returned %v: %s (may be expected on low-space systems)", res.Severity, res.Message)
	}
}

func TestCheckDiskSpaceBadPath(t *testing.T) {
	res := checkDiskSpace("/nonexistent/path/that/should/fail")
	if res.Severity != SeverityError {
		t.Errorf("expected Error for inaccessible path, got %v", res.Severity)
	}
}

func TestCheckUpdateAvailable(t *testing.T) {
	t.Run("no update", func(t *testing.T) {
		deps := RuntimeDeps{
			UpdateVersion: func() *string { return nil },
			ServerVersion: "1.0.0",
		}
		res := checkUpdateAvailable(deps)
		if res.Severity != SeverityOK {
			t.Errorf("expected OK, got %v", res.Severity)
		}
	})

	t.Run("update available", func(t *testing.T) {
		v := "1.1.0"
		deps := RuntimeDeps{
			UpdateVersion: func() *string { return &v },
			ServerVersion: "1.0.0",
		}
		res := checkUpdateAvailable(deps)
		if res.Severity != SeverityInfo {
			t.Errorf("expected Info, got %v", res.Severity)
		}
	})
}

func TestRunDynamicChecks(t *testing.T) {
	deps := RuntimeDeps{
		DBPing:        func(ctx context.Context) error { return nil },
		DockerPing:    func(ctx context.Context) error { return nil },
		RedisPing:     func(ctx context.Context) error { return nil },
		IDPCheck:      func(ctx context.Context) error { return nil },
		VaultCheck:    func(ctx context.Context) error { return nil },
		VaultTokenOK:  func() bool { return true },
		StorePath:     t.TempDir(),
		UpdateVersion: func() *string { return nil },
		ServerVersion: "1.0.0",
	}

	report := runDynamicChecks(context.Background(), deps)
	if len(report.Results) == 0 {
		t.Error("expected results from dynamic checks")
	}

	// All should be OK with healthy deps.
	for _, r := range report.Results {
		if r.Severity > SeverityOK {
			t.Errorf("check %q: expected OK, got %v: %s", r.Name, r.Severity, r.Message)
		}
		if r.Category != "runtime" {
			t.Errorf("check %q: expected runtime category, got %q", r.Name, r.Category)
		}
	}
}
