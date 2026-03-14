package auth

import (
	"crypto/sha256"
	"strings"
	"testing"
)

func TestGeneratePAT(t *testing.T) {
	plaintext, hash, err := GeneratePAT()
	if err != nil {
		t.Fatalf("GeneratePAT() returned error: %v", err)
	}

	if !strings.HasPrefix(plaintext, "by_") {
		t.Errorf("plaintext should have prefix 'by_', got %q", plaintext)
	}

	if plaintext == "by_" {
		t.Error("plaintext should contain characters after the prefix")
	}

	// SHA-256 produces 32 bytes.
	if len(hash) != 32 {
		t.Errorf("hash length = %d, want 32", len(hash))
	}

	// Hash should be consistent with re-hashing the plaintext.
	recomputed := sha256.Sum256([]byte(plaintext))
	for i := range hash {
		if hash[i] != recomputed[i] {
			t.Fatalf("hash mismatch at byte %d: got %02x, want %02x", i, hash[i], recomputed[i])
		}
	}
}

func TestGeneratePATUniqueness(t *testing.T) {
	p1, h1, err := GeneratePAT()
	if err != nil {
		t.Fatalf("first GeneratePAT() error: %v", err)
	}

	p2, h2, err := GeneratePAT()
	if err != nil {
		t.Fatalf("second GeneratePAT() error: %v", err)
	}

	if p1 == p2 {
		t.Error("two generated PATs should not have the same plaintext")
	}

	if string(h1) == string(h2) {
		t.Error("two generated PATs should not have the same hash")
	}
}

func TestHashPAT(t *testing.T) {
	t.Run("matches manual sha256", func(t *testing.T) {
		input := "by_sometesttoken"
		got := HashPAT(input)
		want := sha256.Sum256([]byte(input))

		if len(got) != 32 {
			t.Fatalf("HashPAT length = %d, want 32", len(got))
		}
		for i := range got {
			if got[i] != want[i] {
				t.Fatalf("mismatch at byte %d: got %02x, want %02x", i, got[i], want[i])
			}
		}
	})

	t.Run("matches GeneratePAT hash", func(t *testing.T) {
		plaintext, hash, err := GeneratePAT()
		if err != nil {
			t.Fatalf("GeneratePAT() error: %v", err)
		}

		got := HashPAT(plaintext)
		for i := range hash {
			if got[i] != hash[i] {
				t.Fatalf("mismatch at byte %d: got %02x, want %02x", i, got[i], hash[i])
			}
		}
	})
}

func TestBase62Encode(t *testing.T) {
	tests := []struct {
		name  string
		input []byte
		want  string
	}{
		{
			name:  "empty input",
			input: []byte{},
			want:  "",
		},
		{
			name:  "single zero byte",
			input: []byte{0},
			want:  "0",
		},
		{
			name:  "leading zero bytes",
			input: []byte{0, 0, 1},
			want:  "001",
		},
		{
			name:  "single byte 1",
			input: []byte{1},
			want:  "1",
		},
		{
			name:  "byte value 62",
			input: []byte{62},
			want:  "10",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := base62Encode(tt.input)
			if got != tt.want {
				t.Errorf("base62Encode(%v) = %q, want %q", tt.input, got, tt.want)
			}
		})
	}
}

func TestBase62EncodeNonZero(t *testing.T) {
	// Feed in some non-trivial data and verify all output chars are base62.
	data := []byte{0xff, 0xab, 0x01, 0x00, 0x7c, 0x3e}
	encoded := base62Encode(data)

	if len(encoded) == 0 {
		t.Fatal("base62Encode returned empty string for non-empty input")
	}

	const base62Set = "0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz"
	for i, c := range encoded {
		if !strings.ContainsRune(base62Set, c) {
			t.Errorf("character at index %d is %q, which is not in base62 alphabet", i, string(c))
		}
	}
}
