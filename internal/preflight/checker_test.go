package preflight

import (
	"context"
	"errors"
	"sync"
	"testing"
)

func TestCheckerLatestNilBeforeInit(t *testing.T) {
	c := NewChecker(RuntimeDeps{})
	if c.Latest() != nil {
		t.Error("Latest should be nil before Init")
	}
}

func TestCheckerInitPopulatesLatest(t *testing.T) {
	c := NewChecker(RuntimeDeps{
		DBPing:     func(ctx context.Context) error { return nil },
		BackendPing: func(ctx context.Context) error { return nil },
	})

	configReport := &Report{}
	configReport.Add(Result{Name: "test_config", Severity: SeverityOK, Message: "ok", Category: "config"})

	c.Init(context.Background(), configReport, nil)

	report := c.Latest()
	if report == nil {
		t.Fatal("Latest should not be nil after Init")
	}

	// Should have static config check + dynamic checks.
	if len(report.Results) == 0 {
		t.Error("expected results after Init")
	}

	// Should include the config check from startup.
	found := false
	for _, r := range report.Results {
		if r.Name == "test_config" {
			found = true
			break
		}
	}
	if !found {
		t.Error("static config check should be present in combined report")
	}
}

func TestCheckerRunDynamicUpdatesLatest(t *testing.T) {
	callCount := 0
	c := NewChecker(RuntimeDeps{
		DBPing: func(ctx context.Context) error {
			callCount++
			return nil
		},
		BackendPing: func(ctx context.Context) error { return nil },
	})

	c.Init(context.Background(), &Report{}, nil)
	initialCount := callCount

	// Run dynamic again — should re-run checks.
	report := c.RunDynamic(context.Background())
	if callCount <= initialCount {
		t.Error("RunDynamic should re-run dynamic checks")
	}
	if report == nil {
		t.Error("RunDynamic should return a report")
	}
}

func TestCheckerConcurrentAccess(t *testing.T) {
	c := NewChecker(RuntimeDeps{
		DBPing:     func(ctx context.Context) error { return nil },
		BackendPing: func(ctx context.Context) error { return nil },
	})
	c.Init(context.Background(), &Report{}, nil)

	var wg sync.WaitGroup
	for range 10 {
		wg.Add(2)
		go func() {
			defer wg.Done()
			c.RunDynamic(context.Background())
		}()
		go func() {
			defer wg.Done()
			c.Latest()
		}()
	}
	wg.Wait()
}

func TestCheckerStaticResultsPreserved(t *testing.T) {
	c := NewChecker(RuntimeDeps{
		DBPing:     func(ctx context.Context) error { return nil },
		BackendPing: func(ctx context.Context) error { return nil },
	})

	configReport := &Report{}
	configReport.Add(Result{Name: "static_check", Severity: SeverityWarning, Message: "static", Category: "config"})
	c.Init(context.Background(), configReport, nil)

	// Run dynamic several times.
	for range 5 {
		c.RunDynamic(context.Background())
	}

	report := c.Latest()
	found := 0
	for _, r := range report.Results {
		if r.Name == "static_check" {
			found++
		}
	}
	if found != 1 {
		t.Errorf("expected exactly 1 static_check result, got %d", found)
	}
}

func TestCheckerRunDynamicCancelledContext(t *testing.T) {
	c := NewChecker(RuntimeDeps{
		DBPing: func(ctx context.Context) error {
			<-ctx.Done()
			return ctx.Err()
		},
		BackendPing: func(ctx context.Context) error { return nil },
	})
	c.Init(context.Background(), &Report{}, nil)

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // cancel immediately

	report := c.RunDynamic(ctx)
	if report == nil {
		t.Fatal("RunDynamic should return a report even with cancelled context")
	}

	// The database check should have failed due to cancellation.
	var dbResult *Result
	for i := range report.Results {
		if report.Results[i].Name == "database" {
			dbResult = &report.Results[i]
			break
		}
	}
	if dbResult == nil {
		t.Fatal("expected database result in report")
	}
	if dbResult.Severity != SeverityError {
		t.Errorf("expected database error with cancelled context, got %v", dbResult.Severity)
	}
}

func TestCheckerResultsSortedBySeverity(t *testing.T) {
	c := NewChecker(RuntimeDeps{
		DBPing:     func(ctx context.Context) error { return errors.New("down") },
		BackendPing: func(ctx context.Context) error { return nil },
	})

	// Config report with a warning.
	configReport := &Report{}
	configReport.Add(Result{Name: "warn_check", Severity: SeverityWarning, Message: "warn", Category: "config"})

	c.Init(context.Background(), configReport, nil)
	report := c.Latest()

	// First result should be the highest severity.
	if len(report.Results) < 2 {
		t.Fatal("expected at least 2 results")
	}
	for i := 1; i < len(report.Results); i++ {
		if report.Results[i].Severity > report.Results[i-1].Severity {
			t.Errorf("results not sorted by severity: %v at [%d] > %v at [%d]",
				report.Results[i].Severity, i, report.Results[i-1].Severity, i-1)
		}
	}
}
