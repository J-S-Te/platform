package infrastructure

import (
	"crypto/rand"
	"encoding/base64"
	"errors"
)

// SecretGenerator creates 256-bit URL-safe random values for QR state and opaque session IDs.
type SecretGenerator struct{}

// NewSecret returns an unpadded base64url string backed by crypto/rand.
func (SecretGenerator) NewSecret() (string, error) {
	buffer := make([]byte, 32)
	if _, err := rand.Read(buffer); err != nil {
		return "", errors.New("generate dingtalk QR secret")
	}
	return base64.RawURLEncoding.EncodeToString(buffer), nil
}
