package credentialcrypto

import "strings"

// OpenRequest validates the envelope against server expectations and returns the
// original body bytes plus the independent response AES key.
func (ring *KeyRing) OpenRequest(env RequestEnvelope, expected EnvelopeExpectation) ([]byte, []byte, error) {
	if ring == nil || ring.ActiveKey() == nil {
		return nil, nil, ErrEncryptionFailed
	}
	if err := validateMeta(env, env.KID); err != nil {
		return nil, nil, err
	}
	if err := validateExpectation(env, expected); err != nil {
		return nil, nil, err
	}
	if expected.ContentType == "application/json" && env.ContentType != "application/json" {
		return nil, nil, ErrInvalidEnvelope
	}
	if expected.ContentType == "multipart/form-data" && !strings.HasPrefix(env.ContentType, "multipart/form-data; boundary=") {
		return nil, nil, ErrInvalidEnvelope
	}
	key, ok := ring.Key(env.KID)
	if !ok || key == nil {
		return nil, nil, ErrInvalidEnvelope
	}
	material, err := decodeKeyMaterial(env.EncryptedKey, key.PrivateKey)
	if err != nil {
		return nil, nil, envelopeError(err)
	}
	requestKey := material[:AESKeySize]
	responseKey := material[AESKeySize:]
	iv, err := decodeURL(env.IV)
	if err != nil {
		return nil, nil, ErrInvalidEnvelope
	}
	ciphertext, err := decodeURL(env.Ciphertext)
	if err != nil {
		return nil, nil, ErrInvalidEnvelope
	}
	tag, err := decodeURL(env.Tag)
	if err != nil {
		return nil, nil, ErrInvalidEnvelope
	}
	aad, err := AAD(DirectionRequest, env.KID, env.ChallengeID, env.Purpose, env.ResourceID, env.ContentType)
	if err != nil {
		return nil, nil, ErrInvalidEnvelope
	}
	plain, err := open(requestKey, iv, ciphertext, tag, aad)
	if err != nil {
		return nil, nil, envelopeError(err)
	}
	return plain, responseKey, nil
}

// SealResponse encrypts the complete original response body with K_resp.
func SealResponse(kid, challengeID, purpose, resourceID string, responseKey, plaintext []byte) (ResponseEnvelope, error) {
	if len(responseKey) != AESKeySize || len(plaintext) == 0 || len(plaintext) > 8<<20 {
		return ResponseEnvelope{}, ErrEncryptionFailed
	}
	iv, err := randomBytes(IVSize)
	if err != nil {
		return ResponseEnvelope{}, ErrEncryptionFailed
	}
	aad, err := AAD(DirectionResponse, kid, challengeID, purpose, resourceID, "application/json")
	if err != nil {
		return ResponseEnvelope{}, ErrInvalidEnvelope
	}
	ciphertext, tag, err := seal(responseKey, iv, plaintext, aad)
	if err != nil {
		return ResponseEnvelope{}, ErrEncryptionFailed
	}
	return ResponseEnvelope{
		Protocol: Protocol, KID: kid, ChallengeID: challengeID, Purpose: purpose,
		ResourceID: resourceID, ContentType: "application/json",
		IV: encodeURL(iv), Ciphertext: encodeURL(ciphertext), Tag: encodeURL(tag),
	}, nil
}

// PublicKeyPEM returns the active SPKI PEM for challenge responses.
func (ring *KeyRing) PublicKeyPEM() string {
	if ring == nil || ring.ActiveKey() == nil {
		return ""
	}
	return ring.ActiveKey().PublicKeyPEM
}

// OpenResponse is used by protocol tests and server-side integration tests.
func OpenResponse(env ResponseEnvelope, kid string, responseKey []byte) ([]byte, error) {
	if err := validateResponseMeta(env, kid); err != nil {
		return nil, err
	}
	iv, err := decodeURL(env.IV)
	if err != nil {
		return nil, ErrInvalidEnvelope
	}
	ciphertext, err := decodeURL(env.Ciphertext)
	if err != nil {
		return nil, ErrInvalidEnvelope
	}
	tag, err := decodeURL(env.Tag)
	if err != nil {
		return nil, ErrInvalidEnvelope
	}
	aad, err := AAD(DirectionResponse, env.KID, env.ChallengeID, env.Purpose, env.ResourceID, env.ContentType)
	if err != nil {
		return nil, ErrInvalidEnvelope
	}
	return open(responseKey, iv, ciphertext, tag, aad)
}
