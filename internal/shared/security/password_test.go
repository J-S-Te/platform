package security

import (
	"crypto/rand"
	"testing"
)

func TestVerifyPasswordAcceptsMatchingArgon2idCredential(t *testing.T) {
	salt := make([]byte, 16)
	if _, err := rand.Read(salt); err != nil {
		t.Fatalf("generate salt: %v", err)
	}
	params := DefaultArgon2idParams(salt)
	params.MemoryKiB = 8 * 1024
	params.Iterations = 1
	params.Parallelism = 1

	digest, metadata, err := HashPassword("correct horse battery staple", params)
	if err != nil {
		t.Fatalf("hash password: %v", err)
	}
	matched, err := VerifyPassword("correct horse battery staple", "argon2id", digest, metadata)
	if err != nil {
		t.Fatalf("verify matching password: %v", err)
	}
	if !matched {
		t.Fatal("matching password was rejected")
	}

	matched, err = VerifyPassword("wrong password", "argon2id", digest, metadata)
	if err != nil {
		t.Fatalf("verify non-matching password: %v", err)
	}
	if matched {
		t.Fatal("non-matching password was accepted")
	}
}
