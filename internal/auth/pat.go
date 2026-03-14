package auth

import (
	"crypto/rand"
	"crypto/sha256"
	"math/big"
)

const (
	patPrefix    = "by_"
	patRandBytes = 32
)

// base62 alphabet for token encoding.
var base62Chars = []byte("0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz")

// GeneratePAT creates a new personal access token with the by_ prefix.
// Returns the plaintext token (shown once to the user) and its SHA-256
// hash (stored in the database).
func GeneratePAT() (plaintext string, hash []byte, err error) {
	b := make([]byte, patRandBytes)
	if _, err := rand.Read(b); err != nil {
		return "", nil, err
	}

	// Base62-encode the random bytes.
	encoded := base62Encode(b)
	plaintext = patPrefix + encoded

	h := sha256.Sum256([]byte(plaintext))
	return plaintext, h[:], nil
}

// HashPAT computes the SHA-256 hash of a plaintext PAT.
func HashPAT(plaintext string) []byte {
	h := sha256.Sum256([]byte(plaintext))
	return h[:]
}

// base62Encode converts arbitrary bytes to a base62 string.
func base62Encode(data []byte) string {
	// Convert bytes to a big integer, then repeatedly divide by 62.
	num := new(big.Int).SetBytes(data)
	base := big.NewInt(62)
	zero := big.NewInt(0)
	mod := new(big.Int)

	var result []byte
	for num.Cmp(zero) > 0 {
		num.DivMod(num, base, mod)
		result = append(result, base62Chars[mod.Int64()])
	}

	// Pad for leading zero bytes in input.
	for _, b := range data {
		if b != 0 {
			break
		}
		result = append(result, base62Chars[0])
	}

	// Reverse.
	for i, j := 0, len(result)-1; i < j; i, j = i+1, j-1 {
		result[i], result[j] = result[j], result[i]
	}

	return string(result)
}
