package application

import (
	"crypto/rand"
	"encoding/base64"
	"fmt"
)

const opaqueSecretBytes = 32

// CryptographicSecretGenerator produces 256-bit URL-safe opaque protocol credentials.
type CryptographicSecretGenerator struct{}

// NewSecret returns a new random value suitable for a short-lived code or refresh token.
func (CryptographicSecretGenerator) NewSecret() (string, error) {
	bytes := make([]byte, opaqueSecretBytes)
	if _, err := rand.Read(bytes); err != nil {
		return "", fmt.Errorf("read cryptographic randomness: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(bytes), nil
}
