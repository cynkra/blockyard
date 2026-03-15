package config

import "github.com/BurntSushi/toml"

// Secret wraps a secret string. Its String() and GoString() methods
// return "[REDACTED]" to prevent accidental logging. Use Expose() to
// retrieve the actual value.
type Secret struct {
	value string
}

func NewSecret(s string) Secret {
	return Secret{value: s}
}

func (s Secret) Expose() string { return s.value }
func (s Secret) IsEmpty() bool  { return s.value == "" }
func (s Secret) String() string { return "[REDACTED]" }

// GoString implements fmt.GoStringer for %#v formatting.
func (s Secret) GoString() string { return "[REDACTED]" }

// MarshalJSON implements json.Marshaler to prevent secret leakage.
func (s Secret) MarshalJSON() ([]byte, error) {
	return []byte(`"[REDACTED]"`), nil
}

// MarshalText implements encoding.TextMarshaler to prevent secret leakage.
func (s Secret) MarshalText() ([]byte, error) {
	return []byte("[REDACTED]"), nil
}

// UnmarshalText implements encoding.TextUnmarshaler so TOML
// decoding writes the raw string into the Secret wrapper.
func (s *Secret) UnmarshalText(text []byte) error {
	s.value = string(text)
	return nil
}

// Verify interface compliance.
var _ toml.TextUnmarshaler = (*Secret)(nil)
