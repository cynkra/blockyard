package units

import "testing"

func TestParseMemoryLimit(t *testing.T) {
	tests := []struct {
		input string
		want  int64
		ok    bool
	}{
		{"512m", 512 * 1024 * 1024, true},
		{"1g", 1024 * 1024 * 1024, true},
		{"256mb", 256 * 1024 * 1024, true},
		{"100kb", 100 * 1024, true},
		{"1024", 1024, true},
		{"  2g  ", 2 * 1024 * 1024 * 1024, true},
		{"invalid", 0, false},
	}

	for _, tt := range tests {
		got, ok := ParseMemoryLimit(tt.input)
		if ok != tt.ok {
			t.Errorf("ParseMemoryLimit(%q) ok = %v, want %v", tt.input, ok, tt.ok)
			continue
		}
		if ok && got != tt.want {
			t.Errorf("ParseMemoryLimit(%q) = %d, want %d", tt.input, got, tt.want)
		}
	}
}

func TestParseMemoryLimitEdgeCases(t *testing.T) {
	tests := []struct {
		input  string
		want   int64
		wantOk bool
	}{
		{"512m", 512 * 1024 * 1024, true},
		{"1g", 1024 * 1024 * 1024, true},
		{"256k", 256 * 1024, true},
		{"1024", 1024, true},
		{"", 0, false},
		{"abc", 0, false},
		{"m", 0, false},
		{"512mb", 512 * 1024 * 1024, true},
		{"2gb", 2 * 1024 * 1024 * 1024, true},
		{"128kb", 128 * 1024, true},
		// Case insensitivity.
		{"512M", 512 * 1024 * 1024, true},
		{"1G", 1024 * 1024 * 1024, true},
		{"100KB", 100 * 1024, true},
		{"256MB", 256 * 1024 * 1024, true},
		// Whitespace handling.
		{"   ", 0, false},
		{" 512m ", 512 * 1024 * 1024, true},
		{" 1024 ", 1024, true},
		// Zero values.
		{"0", 0, true},
		{"0m", 0, true},
		{"0g", 0, true},
		// Negative values.
		{"-512m", -512 * 1024 * 1024, true},
		{"-1", -1, true},
		// Decimal values fail (ParseInt rejects them).
		{"1.5g", 0, false},
		{"0.5m", 0, false},
		// Invalid suffixes → treated as bytes, ParseInt fails.
		{"512x", 0, false},
		{"512p", 0, false},
		// Large value.
		{"100g", 100 * 1024 * 1024 * 1024, true},
	}

	for _, tt := range tests {
		got, ok := ParseMemoryLimit(tt.input)
		if ok != tt.wantOk {
			t.Errorf("ParseMemoryLimit(%q): ok=%v, want %v", tt.input, ok, tt.wantOk)
			continue
		}
		if ok && got != tt.want {
			t.Errorf("ParseMemoryLimit(%q) = %d, want %d", tt.input, got, tt.want)
		}
	}
}
