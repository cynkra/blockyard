package main

import (
	"net"
	"strings"
	"testing"
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
