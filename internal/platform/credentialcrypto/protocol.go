// Package credentialcrypto implements the uip-credential/v1 application-layer
// credential envelope. It protects credential request/response bodies in transit;
// it is not a replacement for HTTPS, browser code integrity or session security.
package credentialcrypto

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"strings"
)

const (
	Protocol        = "uip-credential/v1"
	OAEPLabel       = "uip-credential-v1"
	KeyMaterialSize = 64
	AESKeySize      = 32
	IVSize          = 12
	TagSize         = 16
	// MaxEnvelopeField bounds every opaque/meta field before base64 decoding.
	MaxEnvelopeField = 256
)

var (
	ErrInvalidEnvelope  = errors.New("CREDENTIAL_ENVELOPE_INVALID")
	ErrDecryptFailed    = errors.New("CREDENTIAL_DECRYPT_FAILED")
	ErrEncryptionFailed = errors.New("CREDENTIAL_ENCRYPTION_FAILED")
)

// Direction is the AAD-bound direction of an envelope.
type Direction string

const (
	DirectionRequest  Direction = "request"
	DirectionResponse Direction = "response"
)

// EnvelopeExpectation is server-side context that is not trusted from the client.
type EnvelopeExpectation struct {
	Purpose     string
	ResourceID  string
	RouteKey    string
	HTTPMethod  string
	ContentType string
}

// RequestEnvelope mirrors the JSON contract. Fields are validated before any
// cryptographic operation to avoid ambiguous parsing and header injection.
type RequestEnvelope struct {
	Protocol     string `json:"protocol"`
	KID          string `json:"kid"`
	ChallengeID  string `json:"challenge_id"`
	Purpose      string `json:"purpose"`
	ResourceID   string `json:"resource_id"`
	ContentType  string `json:"content_type"`
	EncryptedKey string `json:"encrypted_key"`
	IV           string `json:"iv"`
	Ciphertext   string `json:"ciphertext"`
	Tag          string `json:"tag"`
}

type ResponseEnvelope struct {
	Protocol    string `json:"protocol"`
	KID         string `json:"kid"`
	ChallengeID string `json:"challenge_id"`
	Purpose     string `json:"purpose"`
	ResourceID  string `json:"resource_id"`
	ContentType string `json:"content_type"`
	IV          string `json:"iv"`
	Ciphertext  string `json:"ciphertext"`
	Tag         string `json:"tag"`
}

// AAD returns the exact UTF-8 authenticated data with one newline between fields.
func AAD(direction Direction, kid, challengeID, purpose, resourceID, contentType string) ([]byte, error) {
	fields := []string{Protocol, string(direction), kid, challengeID, purpose, resourceID, contentType}
	for _, field := range fields {
		if strings.ContainsAny(field, "\r\n") {
			return nil, ErrInvalidEnvelope
		}
	}
	return []byte(strings.Join(fields, "\n")), nil
}

func validText(value string, max int) bool {
	return len(value) <= max && !strings.ContainsAny(value, "\r\n")
}

func decodeURL(value string) ([]byte, error) {
	if value == "" || strings.Contains(value, "=") {
		return nil, ErrInvalidEnvelope
	}
	decoded, err := base64.RawURLEncoding.DecodeString(value)
	if err != nil {
		return nil, ErrInvalidEnvelope
	}
	return decoded, nil
}

func encodeURL(value []byte) string { return base64.RawURLEncoding.EncodeToString(value) }

func decodeKeyMaterial(encryptedKey string, privateKey *rsa.PrivateKey) ([]byte, error) {
	encrypted, err := decodeURL(encryptedKey)
	if err != nil || len(encrypted) > 1024 {
		return nil, ErrInvalidEnvelope
	}
	label := []byte(OAEPLabel)
	plain, err := rsa.DecryptOAEP(sha256.New(), rand.Reader, privateKey, encrypted, label)
	if err != nil || len(plain) != KeyMaterialSize {
		return nil, ErrDecryptFailed
	}
	return plain, nil
}

func newGCM(key []byte) (cipher.AEAD, error) {
	if len(key) != AESKeySize {
		return nil, ErrInvalidEnvelope
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, ErrEncryptionFailed
	}
	gcm, err := cipher.NewGCMWithTagSize(block, TagSize)
	if err != nil {
		return nil, ErrEncryptionFailed
	}
	if gcm.NonceSize() != IVSize {
		return nil, ErrEncryptionFailed
	}
	return gcm, nil
}

func seal(key, iv, plaintext, aad []byte) (ciphertext, tag []byte, err error) {
	gcm, err := newGCM(key)
	if err != nil {
		return nil, nil, err
	}
	if len(iv) != IVSize {
		return nil, nil, ErrInvalidEnvelope
	}
	sealed := gcm.Seal(nil, iv, plaintext, aad)
	if len(sealed) < TagSize {
		return nil, nil, ErrEncryptionFailed
	}
	return sealed[:len(sealed)-TagSize], sealed[len(sealed)-TagSize:], nil
}

func open(key, iv, ciphertext, tag, aad []byte) ([]byte, error) {
	gcm, err := newGCM(key)
	if err != nil {
		return nil, ErrDecryptFailed
	}
	if len(iv) != IVSize || len(tag) != TagSize {
		return nil, ErrInvalidEnvelope
	}
	sealed := make([]byte, 0, len(ciphertext)+len(tag))
	sealed = append(sealed, ciphertext...)
	sealed = append(sealed, tag...)
	plain, err := gcm.Open(nil, iv, sealed, aad)
	if err != nil {
		return nil, ErrDecryptFailed
	}
	return plain, nil
}

func randomBytes(size int) ([]byte, error) {
	value := make([]byte, size)
	if _, err := io.ReadFull(rand.Reader, value); err != nil {
		return nil, ErrEncryptionFailed
	}
	return value, nil
}

func validateMeta(env RequestEnvelope, currentKID string) error {
	if env.Protocol != Protocol || env.KID != currentKID ||
		!validText(env.KID, MaxEnvelopeField) || !validText(env.ChallengeID, MaxEnvelopeField) ||
		!validText(env.Purpose, 32) || !validText(env.ResourceID, 64) ||
		!validText(env.ContentType, MaxEnvelopeField) {
		return ErrInvalidEnvelope
	}
	if env.ChallengeID == "" || !strings.HasPrefix(env.ContentType, "application/json") && !strings.HasPrefix(env.ContentType, "multipart/form-data; boundary=") {
		return ErrInvalidEnvelope
	}
	return nil
}

func validateResponseMeta(env ResponseEnvelope, currentKID string) error {
	if env.Protocol != Protocol || env.KID != currentKID ||
		!validText(env.KID, MaxEnvelopeField) || !validText(env.ChallengeID, MaxEnvelopeField) ||
		!validText(env.Purpose, 32) || !validText(env.ResourceID, 64) ||
		!validText(env.ContentType, MaxEnvelopeField) {
		return ErrInvalidEnvelope
	}
	if env.ChallengeID == "" || !strings.HasPrefix(env.ContentType, "application/json") {
		return ErrInvalidEnvelope
	}
	return nil
}

func validateContentType(actual, expected string) error {
	if expected == "" || actual != expected {
		return ErrInvalidEnvelope
	}
	return nil
}

func validateExpectation(env RequestEnvelope, expected EnvelopeExpectation) error {
	if expected.Purpose == "" || env.Purpose != expected.Purpose || env.ResourceID != expected.ResourceID {
		return ErrInvalidEnvelope
	}
	if expected.HTTPMethod != "" && expected.HTTPMethod != "POST" {
		return ErrInvalidEnvelope
	}
	if expected.RouteKey == "" {
		return ErrInvalidEnvelope
	}
	if expected.ContentType != "" {
		return validateContentType(env.ContentType, expected.ContentType)
	}
	return nil
}

func errorMessage(err error) string {
	if errors.Is(err, ErrInvalidEnvelope) {
		return "凭据请求格式不正确，请刷新页面后重试。"
	}
	return "凭据请求无法解密，请刷新页面后重试。"
}

func envelopeError(err error) error {
	if errors.Is(err, ErrInvalidEnvelope) {
		return fmt.Errorf("%w", ErrInvalidEnvelope)
	}
	return fmt.Errorf("%w", ErrDecryptFailed)
}
