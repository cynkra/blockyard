//go:build !minimal || process_backend

package orchestrator

import "context"

// ExecutableFnForTest is the test seam that lets
// process_integration_test.go inject a pre-built blockyard binary
// path instead of the orchestrator looking up its own executable.
// The value is assigned from TestMain before any orchestrator code
// runs, so it effectively pins what CreateInstance execs.
var ExecutableFnForTest = func(fn func() (string, error)) {
	executableFn = fn
}

// AltBindRangeForTest exposes the factory's range parser for unit
// tests. The factory's internal pickAltPort calls it, but tests
// want to verify the range end-to-end without going through the
// listener probe.
func AltBindRangeForTest(f ServerFactory) (int, int, error) {
	pf, ok := f.(*processServerFactory)
	if !ok {
		return 0, 0, nil
	}
	return altBindRange(pf.cfg)
}

// LoopbackAddrForPollingForTest exposes the wildcard-rewrite helper.
var LoopbackAddrForPollingForTest = loopbackAddrForPolling

// ActiveInstanceForTest exposes the orchestrator's stashed
// activeInstance to end-to-end tests so they can retain a kill
// handle across Watchdog (which clears the field on return). The
// returned value is a minimal interface mirroring newServerInstance
// so external test packages can Kill it without naming the
// unexported type.
type TestServerInstance interface {
	ID() string
	Addr() string
	Kill(ctx context.Context)
}

func ActiveInstanceForTest(o *Orchestrator) TestServerInstance {
	if o.activeInstance == nil {
		return nil
	}
	return o.activeInstance
}
