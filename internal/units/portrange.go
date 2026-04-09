package units

import (
	"fmt"
	"strconv"
	"strings"
)

// ListenPort extracts the port from a Go bind address like
// "host:port" or "[::1]:port". Falls back to "8080" when the string
// contains no colon at all — a defensive path for malformed input,
// not a real configuration mode (operators go through config
// validation before reaching this helper).
//
// Shared between cmd/blockyard (passes a closure into
// NewDockerFactory) and internal/orchestrator tests so both sides
// exercise the same parsing. The orchestrator package intentionally
// does not depend on config, so this lives in internal/units where
// both callers can reach it without introducing a cycle.
func ListenPort(bind string) string {
	if idx := strings.LastIndex(bind, ":"); idx != -1 {
		return bind[idx+1:]
	}
	return "8080"
}

// ParsePortRange parses a "start-end" port range string into its
// two integer endpoints. Both endpoints are inclusive. Returns an
// error for malformed input, out-of-range values (0 or > 65535), or
// reversed ordering (start > end).
//
// Used by the process orchestrator for the update.alt_bind_range
// config field. The worker port range on ProcessConfig stays as two
// int fields because its long-lived bitset allocator has different
// lifetime requirements and operators already configure it as
// separate start/end fields.
func ParsePortRange(s string) (first, last int, err error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, 0, fmt.Errorf("empty port range")
	}
	parts := strings.SplitN(s, "-", 2)
	if len(parts) != 2 {
		return 0, 0, fmt.Errorf("port range %q: expected \"start-end\"", s)
	}
	first, err = strconv.Atoi(strings.TrimSpace(parts[0]))
	if err != nil {
		return 0, 0, fmt.Errorf("port range %q: start: %w", s, err)
	}
	last, err = strconv.Atoi(strings.TrimSpace(parts[1]))
	if err != nil {
		return 0, 0, fmt.Errorf("port range %q: end: %w", s, err)
	}
	if first <= 0 || first > 65535 {
		return 0, 0, fmt.Errorf("port range %q: start out of range 1..65535", s)
	}
	if last <= 0 || last > 65535 {
		return 0, 0, fmt.Errorf("port range %q: end out of range 1..65535", s)
	}
	if first > last {
		return 0, 0, fmt.Errorf("port range %q: start > end", s)
	}
	return first, last, nil
}
