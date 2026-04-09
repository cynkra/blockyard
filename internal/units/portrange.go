package units

import (
	"fmt"
	"strconv"
	"strings"
)

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
