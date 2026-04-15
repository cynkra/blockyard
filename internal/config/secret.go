package config

import (
	"context"
	"fmt"
	"strings"

	"encoding"
)

// Secret wraps a secret string. Its String() and GoString() methods
// return "[REDACTED]" to prevent accidental logging. Use Expose() to
// retrieve the actual value.
type Secret struct {
	value string
}

// SecretResolver reads secrets from a vault-compatible KV v2 store
// using the caller's admin credentials. Implemented by
// integration.Client.
type SecretResolver interface {
	KVReadAdmin(ctx context.Context, path string) (map[string]any, error)
}

func NewSecret(s string) Secret {
	return Secret{value: s}
}

// Expose returns the secret value. It returns an error if the value
// is an unresolved vault reference (starts with "vault:"), preventing
// accidental use of the raw reference string as a secret.
func (s Secret) Expose() (string, error) {
	if strings.HasPrefix(s.value, "vault:") {
		return "", fmt.Errorf("secret contains unresolved vault reference: %s", s.value)
	}
	return s.value, nil
}

// MustExpose returns the secret value, panicking if the value is an
// unresolved vault reference. Use in tests and contexts where the
// value is known to be resolved.
func (s Secret) MustExpose() string {
	v, err := s.Expose()
	if err != nil {
		panic(err)
	}
	return v
}

func (s Secret) IsEmpty() bool  { return s.value == "" }
func (s Secret) String() string { return "[REDACTED]" }

// IsVaultRef reports whether the secret value is a vault reference
// (starts with "vault:").
func (s Secret) IsVaultRef() bool { return strings.HasPrefix(s.value, "vault:") }

// SetValue replaces the secret's internal value. Used for
// auto-generated secrets (e.g. session_secret).
func (s *Secret) SetValue(v string) { s.value = v }

// Resolve resolves a vault reference. If the value starts with
// "vault:", it parses the format "vault:{kv_path}#{key}", reads the
// secret from vault, and replaces the internal value. Non-vault
// values are left unchanged.
func (s *Secret) Resolve(ctx context.Context, r SecretResolver) error {
	if !strings.HasPrefix(s.value, "vault:") {
		return nil // literal value, nothing to resolve
	}

	ref := strings.TrimPrefix(s.value, "vault:")
	parts := strings.SplitN(ref, "#", 2)
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return fmt.Errorf("invalid vault reference %q: expected format vault:{kv_path}#{key}", s.value)
	}
	kvPath, key := parts[0], parts[1]

	data, err := r.KVReadAdmin(ctx, kvPath)
	if err != nil {
		return fmt.Errorf("resolve vault reference %q: %w", s.value, err)
	}

	val, ok := data[key]
	if !ok {
		return fmt.Errorf("resolve vault reference %q: key %q not found in secret", s.value, key)
	}

	str, ok := val.(string)
	if !ok {
		return fmt.Errorf("resolve vault reference %q: key %q is not a string", s.value, key)
	}

	s.value = str
	return nil
}

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
var _ encoding.TextUnmarshaler = (*Secret)(nil)
