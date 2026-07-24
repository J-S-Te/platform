// Package security provides low-level security primitives shared by platform modules.
package security

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"

	"golang.org/x/crypto/argon2"
)

const argon2idAlgorithm = "argon2id"

// Argon2idPasswordHasher creates fresh Argon2id credentials for controlled provisioning flows.
// It returns the raw digest and JSON verification metadata expected by iam_password_credential.
type Argon2idPasswordHasher struct{}

// Hash derives a password digest using a cryptographically random 16-byte salt.
func (Argon2idPasswordHasher) Hash(password string) ([]byte, []byte, error) {
	salt := make([]byte, 16)
	if _, err := rand.Read(salt); err != nil {
		return nil, nil, fmt.Errorf("generate Argon2id salt: %w", err)
	}
	return HashPassword(password, DefaultArgon2idParams(salt))
}

// Argon2idParams records the exact parameters needed to verify a password credential. The
// password digest is stored separately in iam_password_credential.password_hash.
type Argon2idParams struct {
	Version     int    `json:"version"`
	MemoryKiB   uint32 `json:"memory_kib"`
	Iterations  uint32 `json:"iterations"`
	Parallelism uint8  `json:"parallelism"`
	SaltBase64  string `json:"salt_base64"`
	KeyLength   uint32 `json:"key_length"`
}

// DefaultArgon2idParams returns the baseline parameters used by future password-provisioning
// flows. Verification always reads the stored parameters so planned upgrades remain possible.
func DefaultArgon2idParams(salt []byte) Argon2idParams {
	return Argon2idParams{
		Version:     argon2.Version,
		MemoryKiB:   64 * 1024,
		Iterations:  3,
		Parallelism: 2,
		SaltBase64:  base64.RawStdEncoding.EncodeToString(salt),
		KeyLength:   32,
	}
}

// HashPassword derives a raw Argon2id password digest and serializes its verification metadata.
// It is intentionally provided for future operator-driven account provisioning, not HTTP input.
func HashPassword(password string, params Argon2idParams) ([]byte, []byte, error) {
	if err := validateArgon2idParams(params); err != nil {
		return nil, nil, err
	}
	salt, err := base64.RawStdEncoding.DecodeString(params.SaltBase64)
	if err != nil {
		return nil, nil, fmt.Errorf("decode Argon2id salt: %w", err)
	}
	digest := argon2.IDKey([]byte(password), salt, params.Iterations, params.MemoryKiB, params.Parallelism, params.KeyLength)
	metadata, err := json.Marshal(params)
	if err != nil {
		return nil, nil, fmt.Errorf("marshal Argon2id parameters: %w", err)
	}
	return digest, metadata, nil
}

// VerifyPassword verifies an Argon2id credential stored as a raw digest plus JSON metadata.
// It returns false for a well-formed credential with a non-matching password.
func VerifyPassword(password string, algorithm string, digest, metadata []byte) (bool, error) {
	if !strings.EqualFold(algorithm, argon2idAlgorithm) {
		return false, fmt.Errorf("unsupported password hash algorithm %q", algorithm)
	}

	var params Argon2idParams
	if err := json.Unmarshal(metadata, &params); err != nil {
		return false, fmt.Errorf("decode Argon2id parameters: %w", err)
	}
	if err := validateArgon2idParams(params); err != nil {
		return false, err
	}

	salt, err := base64.RawStdEncoding.DecodeString(params.SaltBase64)
	if err != nil {
		return false, fmt.Errorf("decode Argon2id salt: %w", err)
	}
	candidate := argon2.IDKey([]byte(password), salt, params.Iterations, params.MemoryKiB, params.Parallelism, params.KeyLength)
	return subtle.ConstantTimeCompare(candidate, digest) == 1, nil
}

func validateArgon2idParams(params Argon2idParams) error {
	if params.Version != argon2.Version {
		return fmt.Errorf("unsupported Argon2id version %d", params.Version)
	}
	if params.MemoryKiB < 8*uint32(params.Parallelism) || params.MemoryKiB > 1024*1024 {
		return fmt.Errorf("Argon2id memory_kib is outside the supported range")
	}
	if params.Iterations == 0 || params.Iterations > 10 {
		return fmt.Errorf("Argon2id iterations is outside the supported range")
	}
	if params.Parallelism == 0 || params.Parallelism > 8 {
		return fmt.Errorf("Argon2id parallelism is outside the supported range")
	}
	if params.KeyLength < 16 || params.KeyLength > 64 {
		return fmt.Errorf("Argon2id key_length is outside the supported range")
	}
	salt, err := base64.RawStdEncoding.DecodeString(params.SaltBase64)
	if err != nil || len(salt) < 16 || len(salt) > 64 {
		return fmt.Errorf("Argon2id salt must be base64 encoded and 16 to 64 bytes")
	}
	return nil
}

// Argon2idPasswordVerifier adapts VerifyPassword to the identity application dependency.
type Argon2idPasswordVerifier struct{}

// Verify checks a stored Argon2id credential.
func (Argon2idPasswordVerifier) Verify(password, algorithm string, digest, metadata []byte) (bool, error) {
	return VerifyPassword(password, algorithm, digest, metadata)
}
