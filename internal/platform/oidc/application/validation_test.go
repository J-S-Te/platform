package application

import (
	"crypto/sha256"
	"encoding/base64"
	"testing"
)

func TestPKCEAcceptsOnlyS256(t *testing.T) {
	verifier := "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789-._~"
	hash := sha256.Sum256([]byte(verifier))
	challenge := base64.RawURLEncoding.EncodeToString(hash[:])

	if _, _, err := validatePKCE(true, challenge, "S256"); err != nil {
		t.Fatalf("validatePKCE(S256) error = %v", err)
	}
	if !verifyPKCE(challenge, "S256", verifier) {
		t.Fatal("verifyPKCE(S256) = false, want true")
	}
	if _, _, err := validatePKCE(true, verifier, "plain"); err == nil {
		t.Fatal("validatePKCE(plain) error = nil, want rejection")
	}
	if verifyPKCE(verifier, "plain", verifier) {
		t.Fatal("verifyPKCE(plain) = true, want false")
	}
}
