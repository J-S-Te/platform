// Package ulid generates ULID-compatible identifiers without a database sequence.
package ulid

import (
	"crypto/rand"
	"fmt"
	"time"
)

const alphabet = "0123456789ABCDEFGHJKMNPQRSTVWXYZ"

// New returns a 26-character Crockford Base32 ULID using the supplied time and cryptographic
// randomness. The timestamp portion is UTC milliseconds so generated identifiers remain
// approximately time ordered.
func New(now time.Time) (string, error) {
	milliseconds := now.UTC().UnixMilli()
	if milliseconds < 0 || milliseconds > 0xFFFFFFFFFFFF {
		return "", fmt.Errorf("ULID timestamp is out of range")
	}

	var raw [16]byte
	raw[0] = byte(milliseconds >> 40)
	raw[1] = byte(milliseconds >> 32)
	raw[2] = byte(milliseconds >> 24)
	raw[3] = byte(milliseconds >> 16)
	raw[4] = byte(milliseconds >> 8)
	raw[5] = byte(milliseconds)
	if _, err := rand.Read(raw[6:]); err != nil {
		return "", fmt.Errorf("read ULID randomness: %w", err)
	}

	var encoded [26]byte
	for index := range encoded {
		var value byte
		for bit := 0; bit < 5; bit++ {
			value <<= 1
			sourceBit := index*5 + bit - 2 // ULID's first Base32 character has two leading zero bits.
			if sourceBit < 0 || sourceBit >= len(raw)*8 {
				continue
			}
			current := (raw[sourceBit/8] >> (7 - sourceBit%8)) & 1
			value |= current
		}
		encoded[index] = alphabet[value]
	}

	return string(encoded[:]), nil
}

// Generator adapts New to application dependencies that require an identifier generator.
type Generator struct{}

// New generates a ULID using the supplied timestamp.
func (Generator) New(now time.Time) (string, error) {
	return New(now)
}
