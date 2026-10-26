package credentialcrypto

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/pem"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func writeTestKeyring(t *testing.T) (*KeyRing, *rsa.PrivateKey, string) {
	t.Helper()
	privateKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	privDER, err := x509.MarshalPKCS8PrivateKey(privateKey)
	if err != nil {
		t.Fatal(err)
	}
	pubDER, err := x509.MarshalPKIXPublicKey(&privateKey.PublicKey)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "private.pem"), pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: privDER}), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "public.pem"), pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: pubDER}), 0o644); err != nil {
		t.Fatal(err)
	}
	kid, err := KIDForPublicKey(&privateKey.PublicKey)
	if err != nil {
		t.Fatal(err)
	}
	metadata := `{"active_kid":"` + kid + `","keys":[{"kid":"` + kid + `","private_key_file":"private.pem","public_key_file":"public.pem"}]}`
	path := filepath.Join(dir, "keyring.json")
	if err := os.WriteFile(path, []byte(metadata), 0o600); err != nil {
		t.Fatal(err)
	}
	ring, err := LoadKeyRing(path, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	return ring, privateKey, kid
}

func sealTestRequest(t *testing.T, publicKey *rsa.PublicKey, kid, purpose, resourceID, contentType string, plaintext []byte) (RequestEnvelope, []byte) {
	t.Helper()
	material := make([]byte, KeyMaterialSize)
	if _, err := rand.Read(material); err != nil {
		t.Fatal(err)
	}
	encrypted, err := rsa.EncryptOAEP(sha256.New(), rand.Reader, publicKey, material, []byte(OAEPLabel))
	if err != nil {
		t.Fatal(err)
	}
	block, err := aes.NewCipher(material[:AESKeySize])
	if err != nil {
		t.Fatal(err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		t.Fatal(err)
	}
	iv := make([]byte, IVSize)
	if _, err := rand.Read(iv); err != nil {
		t.Fatal(err)
	}
	aad, err := AAD(DirectionRequest, kid, "challenge-1", purpose, resourceID, contentType)
	if err != nil {
		t.Fatal(err)
	}
	sealed := gcm.Seal(nil, iv, plaintext, aad)
	return RequestEnvelope{
		Protocol: Protocol, KID: kid, ChallengeID: "challenge-1", Purpose: purpose,
		ResourceID: resourceID, ContentType: contentType,
		EncryptedKey: base64.RawURLEncoding.EncodeToString(encrypted),
		IV:           base64.RawURLEncoding.EncodeToString(iv),
		Ciphertext:   base64.RawURLEncoding.EncodeToString(sealed[:len(sealed)-TagSize]),
		Tag:          base64.RawURLEncoding.EncodeToString(sealed[len(sealed)-TagSize:]),
	}, material[AESKeySize:]
}

func TestOpenRequestAndSealResponse(t *testing.T) {
	ring, privateKey, kid := writeTestKeyring(t)
	body := []byte(`{"account":"中文账号","password":"p@ss-口令"}`)
	env, responseKey := sealTestRequest(t, &privateKey.PublicKey, kid, "LOGIN", "", "application/json", body)
	plain, gotResponseKey, err := ring.OpenRequest(env, EnvelopeExpectation{Purpose: "LOGIN", RouteKey: "auth.login", HTTPMethod: "POST", ContentType: "application/json"})
	if err != nil {
		t.Fatal(err)
	}
	if string(plain) != string(body) {
		t.Fatalf("plaintext = %q", plain)
	}
	if string(gotResponseKey) != string(responseKey) {
		t.Fatal("response key mismatch")
	}
	response, err := SealResponse(kid, env.ChallengeID, env.Purpose, env.ResourceID, gotResponseKey, []byte(`{"code":"OK","data":{"token":"redacted"}}`))
	if err != nil {
		t.Fatal(err)
	}
	opened, err := OpenResponse(response, kid, responseKey)
	if err != nil {
		t.Fatal(err)
	}
	if string(opened) != `{"code":"OK","data":{"token":"redacted"}}` {
		t.Fatalf("response = %s", opened)
	}
}

func TestOpenRequestRejectsTamperingAndBindingMismatch(t *testing.T) {
	ring, privateKey, kid := writeTestKeyring(t)
	env, _ := sealTestRequest(t, &privateKey.PublicKey, kid, "LOGIN", "", "application/json", []byte(`{"account":"a"}`))
	expect := EnvelopeExpectation{Purpose: "LOGIN", RouteKey: "auth.login", HTTPMethod: "POST", ContentType: "application/json"}

	tampered := env
	tampered.Tag = env.Tag[:len(env.Tag)-2] + "AA"
	if _, _, err := ring.OpenRequest(tampered, expect); err == nil {
		t.Fatal("tampered tag accepted")
	}

	crossPurpose := env
	crossPurpose.Purpose = "RESET_PASSWORD"
	if _, _, err := ring.OpenRequest(crossPurpose, EnvelopeExpectation{Purpose: "RESET_PASSWORD", ResourceID: "account-1", RouteKey: "account.reset", HTTPMethod: "POST", ContentType: "application/json"}); err == nil {
		t.Fatal("cross-purpose envelope accepted")
	}

	wrongResource := env
	wrongResource.ResourceID = "account-1"
	if _, _, err := ring.OpenRequest(wrongResource, expect); err == nil {
		t.Fatal("resource mismatch accepted")
	}
}

func TestOpenRequestRejectsInvalidContentTypeAndPadding(t *testing.T) {
	ring, privateKey, kid := writeTestKeyring(t)
	env, _ := sealTestRequest(t, &privateKey.PublicKey, kid, "LOGIN", "", "application/json", []byte(`{}`))
	env.ContentType = "text/plain"
	if _, _, err := ring.OpenRequest(env, EnvelopeExpectation{Purpose: "LOGIN", RouteKey: "auth.login", ContentType: "application/json"}); err == nil {
		t.Fatal("content type mismatch accepted")
	}
	env, _ = sealTestRequest(t, &privateKey.PublicKey, kid, "LOGIN", "", "application/json", []byte(`{}`))
	env.Ciphertext += "="
	if _, _, err := ring.OpenRequest(env, EnvelopeExpectation{Purpose: "LOGIN", RouteKey: "auth.login", ContentType: "application/json"}); err == nil {
		t.Fatal("padded base64 accepted")
	}
}

func TestLoadKeyRingRejectsKIDMismatch(t *testing.T) {
	ring, _, kid := writeTestKeyring(t)
	if ring.ActiveKID() != kid {
		t.Fatal("active kid mismatch")
	}
	dir := t.TempDir()
	privateKey, _ := rsa.GenerateKey(rand.Reader, 2048)
	privDER, _ := x509.MarshalPKCS8PrivateKey(privateKey)
	pubDER, _ := x509.MarshalPKIXPublicKey(&privateKey.PublicKey)
	_ = os.WriteFile(filepath.Join(dir, "private.pem"), pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: privDER}), 0o600)
	_ = os.WriteFile(filepath.Join(dir, "public.pem"), pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: pubDER}), 0o644)
	path := filepath.Join(dir, "keyring.json")
	_ = os.WriteFile(path, []byte(`{"active_kid":"not-the-kid","keys":[{"kid":"not-the-kid","private_key_file":"private.pem","public_key_file":"public.pem"}]}`), 0o600)
	if _, err := LoadKeyRing(path, time.Now()); err == nil {
		t.Fatal("kid mismatch accepted")
	}
}

func TestKeyRingAcceptsRetainedOldKeyForInFlightChallenge(t *testing.T) {
	dir := t.TempDir()
	makeKey := func(name string) (*rsa.PrivateKey, string) {
		key, err := rsa.GenerateKey(rand.Reader, 2048)
		if err != nil {
			t.Fatal(err)
		}
		privDER, _ := x509.MarshalPKCS8PrivateKey(key)
		pubDER, _ := x509.MarshalPKIXPublicKey(&key.PublicKey)
		_ = os.WriteFile(filepath.Join(dir, name+"-private.pem"), pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: privDER}), 0o600)
		_ = os.WriteFile(filepath.Join(dir, name+"-public.pem"), pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: pubDER}), 0o644)
		kid, _ := KIDForPublicKey(&key.PublicKey)
		return key, kid
	}
	oldKey, oldKID := makeKey("old")
	newKey, newKID := makeKey("new")
	_ = newKey
	metadata := `{"active_kid":"` + newKID + `","keys":[{"kid":"` + oldKID + `","private_key_file":"old-private.pem","public_key_file":"old-public.pem"},{"kid":"` + newKID + `","private_key_file":"new-private.pem","public_key_file":"new-public.pem"}]}`
	path := filepath.Join(dir, "keyring.json")
	_ = os.WriteFile(path, []byte(metadata), 0o600)
	ring, err := LoadKeyRing(path, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	env, _ := sealTestRequest(t, &oldKey.PublicKey, oldKID, "LOGIN", "", "application/json", []byte(`{"account":"old"}`))
	if _, _, err := ring.OpenRequest(env, EnvelopeExpectation{Purpose: "LOGIN", RouteKey: "auth.login", ContentType: "application/json"}); err != nil {
		t.Fatalf("retained old key rejected: %v", err)
	}
	env.KID = "unknown-kid"
	if _, _, err := ring.OpenRequest(env, EnvelopeExpectation{Purpose: "LOGIN", RouteKey: "auth.login", ContentType: "application/json"}); err == nil {
		t.Fatal("unknown kid accepted")
	}
}
