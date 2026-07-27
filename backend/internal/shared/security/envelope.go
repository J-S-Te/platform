package security

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"strings"
)

// EnvelopeProtector encrypts small, sensitive values before persistence with AES-256-GCM.
// The stored format is nonce || ciphertext. It is suitable for sensitive values; callers
// must keep the base64-encoded key outside the database and must not reuse a key for unrelated
// trust domains.
type EnvelopeProtector struct {
	key []byte
}

// NewEnvelopeProtector creates a configured AES-256-GCM protector. The configuration value must
// be a base64-encoded, exactly 32-byte key; an empty value is rejected so sensitive data is never
// written in plaintext as a fallback.
func NewEnvelopeProtector(encodedKey, environmentName string) (*EnvelopeProtector, error) {
	environmentName = strings.TrimSpace(environmentName)
	if environmentName == "" {
		environmentName = "encryption key"
	}
	if strings.TrimSpace(encodedKey) == "" {
		return nil, fmt.Errorf("%s must be configured", environmentName)
	}

	key, err := base64.StdEncoding.DecodeString(strings.TrimSpace(encodedKey))
	if err != nil {
		return nil, fmt.Errorf("decode %s: %w", environmentName, err)
	}
	if len(key) != 32 {
		return nil, fmt.Errorf("%s must decode to 32 bytes", environmentName)
	}
	return &EnvelopeProtector{key: key}, nil
}

// Encrypt seals plaintext using a fresh random nonce. Context is accepted so this type satisfies
// application-layer protection interfaces; cryptographic work itself is not cancellable.
func (protector *EnvelopeProtector) Encrypt(_ context.Context, plaintext []byte) ([]byte, error) {
	gcm, err := protector.gcm()
	if err != nil {
		return nil, err
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return nil, fmt.Errorf("generate encryption nonce: %w", err)
	}
	return gcm.Seal(nonce, nonce, plaintext, nil), nil
}

// Decrypt authenticates and opens ciphertext created by Encrypt.
func (protector *EnvelopeProtector) Decrypt(_ context.Context, ciphertext []byte) ([]byte, error) {
	gcm, err := protector.gcm()
	if err != nil {
		return nil, err
	}
	if len(ciphertext) < gcm.NonceSize() {
		return nil, errors.New("encrypted value is malformed")
	}
	plaintext, err := gcm.Open(nil, ciphertext[:gcm.NonceSize()], ciphertext[gcm.NonceSize():], nil)
	if err != nil {
		return nil, fmt.Errorf("decrypt encrypted value: %w", err)
	}
	return plaintext, nil
}

func (protector *EnvelopeProtector) gcm() (cipher.AEAD, error) {
	if protector == nil || len(protector.key) != 32 {
		return nil, errors.New("envelope encryption is not configured")
	}
	block, err := aes.NewCipher(protector.key)
	if err != nil {
		return nil, fmt.Errorf("create AES cipher: %w", err)
	}
	return cipher.NewGCM(block)
}
