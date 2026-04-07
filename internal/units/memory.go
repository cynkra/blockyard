// Package units provides parsers for human-readable input units (e.g.
// memory sizes) used by the API and the backends.
package units

import (
	"strconv"
	"strings"
)

// ParseMemoryLimit converts human-readable memory strings like "512m",
// "1g", "256mb" to bytes. Returns (bytes, true) on success, (0, false)
// on parse error or empty input. Suffixes are case-insensitive; bare
// numbers are treated as bytes.
func ParseMemoryLimit(s string) (int64, bool) {
	s = strings.TrimSpace(strings.ToLower(s))
	var numStr string
	var multiplier int64

	switch {
	case strings.HasSuffix(s, "gb"):
		numStr = strings.TrimSuffix(s, "gb")
		multiplier = 1024 * 1024 * 1024
	case strings.HasSuffix(s, "g"):
		numStr = strings.TrimSuffix(s, "g")
		multiplier = 1024 * 1024 * 1024
	case strings.HasSuffix(s, "mb"):
		numStr = strings.TrimSuffix(s, "mb")
		multiplier = 1024 * 1024
	case strings.HasSuffix(s, "m"):
		numStr = strings.TrimSuffix(s, "m")
		multiplier = 1024 * 1024
	case strings.HasSuffix(s, "kb"):
		numStr = strings.TrimSuffix(s, "kb")
		multiplier = 1024
	case strings.HasSuffix(s, "k"):
		numStr = strings.TrimSuffix(s, "k")
		multiplier = 1024
	default:
		numStr = s
		multiplier = 1 // assume bytes
	}

	n, err := strconv.ParseInt(strings.TrimSpace(numStr), 10, 64)
	if err != nil {
		return 0, false
	}
	return n * multiplier, true
}
