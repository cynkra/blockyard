package main

import (
	"encoding/hex"
	"net"
	"strings"
	"testing"

	"github.com/cynkra/blockyard/internal/config"
)

// runProbe is security-critical: checkWorkerEgress trusts its exit
// code as the signal for whether a worker can reach a sensitive
// endpoint. A regression that returned nil on dial failure would
// silently flip the egress warning from "alerting on real
// reachability" to "always quiet", so the exit-code semantics need
// a regression guard.
func TestRunProbeSuccess(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			conn.Close()
		}
	}()
	if err := runProbe([]string{"--tcp", ln.Addr().String(), "--timeout", "2s"}); err != nil {
		t.Errorf("runProbe: %v", err)
	}
}

func TestRunProbeFailure(t *testing.T) {
	// Port 1 is privileged; nothing is listening.
	err := runProbe([]string{"--tcp", "127.0.0.1:1", "--timeout", "200ms"})
	if err == nil {
		t.Error("expected runProbe to fail against an unreachable target")
	}
}

func TestRunProbeRequiresTCPFlag(t *testing.T) {
	err := runProbe(nil)
	if err == nil {
		t.Fatal("expected error when --tcp is omitted")
	}
	if !strings.Contains(err.Error(), "--tcp") {
		t.Errorf("error should mention --tcp: %v", err)
	}
}

func TestRunProbeRejectsUnknownFlag(t *testing.T) {
	err := runProbe([]string{"--bogus"})
	if err == nil {
		t.Error("expected parse error for unknown flag")
	}
}

// TestRunBwrapSmokeMissingBinary — bwrap path is "/nonexistent" so
// exec fails. The function must surface the error (not swallow it);
// the standalone apparmor-smoke CI job relies on a non-zero exit
// for the negative assertion.
func TestRunBwrapSmokeMissingBinary(t *testing.T) {
	err := runBwrapSmoke([]string{"--bwrap", "/nonexistent/bwrap"})
	if err == nil {
		t.Error("expected error when bwrap binary is missing")
	}
}

// TestRunBwrapSmokeUnknownFlag — fresh FlagSet enforces the
// interface; unknown flag should surface as a parse error without
// triggering an exec.
func TestRunBwrapSmokeUnknownFlag(t *testing.T) {
	err := runBwrapSmoke([]string{"--bogus"})
	if err == nil {
		t.Error("expected parse error for unknown flag")
	}
}

func TestRandomNonceHex(t *testing.T) {
	// Length output = 2*n hex chars; decodes back to n bytes.
	for _, n := range []int{1, 4, 8, 16} {
		s := randomNonceHex(n)
		if len(s) != 2*n {
			t.Errorf("randomNonceHex(%d) len = %d, want %d", n, len(s), 2*n)
		}
		if _, err := hex.DecodeString(s); err != nil {
			t.Errorf("randomNonceHex(%d) = %q not hex: %v", n, s, err)
		}
	}

	// Two draws should differ with overwhelming probability. Colliding
	// would mean crypto/rand returned the same bytes twice — a signal
	// something is very wrong, not a flake to tolerate. Bound each call
	// to its own local so staticcheck does not flag this as SA4000.
	a, b := randomNonceHex(8), randomNonceHex(8)
	if a == b {
		t.Errorf("two 8-byte nonces collided: %q", a)
	}
}

func TestMaskRedisPassword(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{
			name: "plain — no credentials, passthrough",
			in:   "redis://localhost:6379/0",
			want: "redis://localhost:6379/0",
		},
		{
			name: "user only — no password to mask",
			in:   "redis://user@localhost:6379/0",
			want: "redis://user@localhost:6379/0",
		},
		{
			// net/url percent-encodes reserved chars in the password
			// position — *** → %2A%2A%2A. Downstream consumers render
			// this via slog so the encoded form is what operators see.
			name: "user+password — password redacted, user preserved",
			in:   "redis://user:secret@localhost:6379/0",
			want: "redis://user:%2A%2A%2A@localhost:6379/0",
		},
		{
			name: "empty-user password — edge case still masked",
			in:   "redis://:secret@localhost:6379/0",
			want: "redis://:%2A%2A%2A@localhost:6379/0",
		},
		{
			name: "unparseable — returned verbatim rather than erroring",
			in:   "not a url :: at all",
			want: "not a url :: at all",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := maskRedisPassword(tc.in)
			if got != tc.want {
				t.Errorf("maskRedisPassword(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

// finishIdleWaitForBackend branches on the concrete backend type. The
// nil-backend case covers the fall-through arm; the process-backend
// cases are covered in drainer_process_test.go where a real
// *process.ProcessBackend is in scope.
func TestFinishIdleWaitForBackend_NilReturnsZero(t *testing.T) {
	cfg := &config.Config{}
	if got := finishIdleWaitForBackend(nil, cfg); got != 0 {
		t.Errorf("finishIdleWaitForBackend(nil, _) = %v, want 0", got)
	}
}

// newServerFactory asks each registered candidate whether it matches
// the backend; nil matches no one, so the function should return nil.
// Proves the dispatch loop terminates cleanly instead of panicking on
// an empty type assertion.
func TestNewServerFactory_NilBackendReturnsNil(t *testing.T) {
	cfg := &config.Config{}
	if got := newServerFactory(nil, cfg, nil); got != nil {
		t.Errorf("newServerFactory(_, _, nil) = %v, want nil", got)
	}
}
