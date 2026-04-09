//go:build !minimal || process_backend

package orchestrator

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
