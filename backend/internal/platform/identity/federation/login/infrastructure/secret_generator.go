package infrastructure

import (
	"crypto/rand"
	"encoding/base64"
	"fmt"
)

// SecretGenerator produces 256-bit URL-safe protocol values for state, nonce and PKCE verifier.
type SecretGenerator struct{}

// NewSecret returns a cryptographically random, URL-safe value without padding.
func (SecretGenerator) NewSecret() (string, error) {
	bytes := make([]byte, 32)
	if _, err := rand.Read(bytes); err != nil {
		return "", fmt.Errorf("read external login random bytes: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(bytes), nil
}
