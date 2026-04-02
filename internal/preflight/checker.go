package preflight

import (
	"context"
	"sort"
	"sync"
	"time"
)

// RuntimeDeps provides the dependencies for dynamic (re-runnable) checks.
// All function fields are called on demand; nil means the check is skipped.
type RuntimeDeps struct {
	StorePath     string                          // pkg-store volume root (for disk space check)
	DBPing        func(ctx context.Context) error // database health
	DockerPing    func(ctx context.Context) error // Docker socket health
	RedisPing     func(ctx context.Context) error // Redis health (nil = no Redis)
	IDPCheck      func(ctx context.Context) error // OIDC IdP health (nil = no OIDC)
	VaultCheck    func(ctx context.Context) error // OpenBao health (nil = no vault)
	VaultTokenOK  func() bool                     // AppRole token health (nil = no AppRole)
	UpdateVersion func() *string                  // latest available version (nil = not checked)
	ServerVersion string
}

// Checker runs system checks and caches the latest report. It
// separates static (startup) checks from dynamic (re-runnable) checks.
type Checker struct {
	deps RuntimeDeps

	mu           sync.RWMutex
	staticReport *Report // populated once at Init, never changes
	latest       *Report // latest full report (static + dynamic)
}

// NewChecker creates a Checker with the given runtime dependencies.
// Call Init to populate static results and run the first dynamic check.
func NewChecker(deps RuntimeDeps) *Checker {
	return &Checker{deps: deps}
}

// Init records the startup check results (config + docker) as static,
// then runs a first dynamic check pass. This should be called once
// during server startup, after all dependencies are initialized.
func (c *Checker) Init(ctx context.Context, configReport, dockerReport *Report) {
	static := &Report{RanAt: time.Now().UTC()}
	if configReport != nil {
		static.Results = append(static.Results, configReport.Results...)
	}
	if dockerReport != nil {
		static.Results = append(static.Results, dockerReport.Results...)
	}
	static.recount()

	c.mu.Lock()
	c.staticReport = static
	c.mu.Unlock()

	// Run first dynamic pass so the system page has data immediately.
	c.RunDynamic(ctx)
}

// RunDynamic re-runs only the dynamic checks and updates the cached
// report. Returns the combined (static + fresh dynamic) report.
func (c *Checker) RunDynamic(ctx context.Context) *Report {
	dynamic := runDynamicChecks(ctx, c.deps)

	c.mu.RLock()
	static := c.staticReport
	c.mu.RUnlock()

	combined := &Report{RanAt: time.Now().UTC()}
	if static != nil {
		combined.Results = append(combined.Results, static.Results...)
	}
	combined.Results = append(combined.Results, dynamic.Results...)
	// Sort by severity descending (errors first), then by name.
	sort.Slice(combined.Results, func(i, j int) bool {
		if combined.Results[i].Severity != combined.Results[j].Severity {
			return combined.Results[i].Severity > combined.Results[j].Severity
		}
		return combined.Results[i].Name < combined.Results[j].Name
	})
	combined.recount()

	c.mu.Lock()
	c.latest = combined
	c.mu.Unlock()

	return combined
}

// Latest returns the most recent cached report. Returns nil if Init
// has not been called yet.
func (c *Checker) Latest() *Report {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.latest
}
