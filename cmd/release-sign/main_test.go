package main

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"os"
	"path/filepath"
	"testing"

	"github.com/J-S-Te/Basic-Platform/internal/shared/releaseintegrity"
)

func writeKey(t *testing.T, dir string) (string, string) {
	t.Helper()
	private, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	privateDER, err := x509.MarshalPKCS8PrivateKey(private)
	if err != nil {
		t.Fatal(err)
	}
	publicDER, err := x509.MarshalPKIXPublicKey(&private.PublicKey)
	if err != nil {
		t.Fatal(err)
	}
	keyPath := filepath.Join(dir, "signing.key")
	if err := os.WriteFile(keyPath, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: privateDER}), 0o600); err != nil {
		t.Fatal(err)
	}
	return keyPath, string(pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: publicDER}))
}

func trustRing(t *testing.T, public string) *releaseintegrity.KeyRing {
	t.Helper()
	dir := t.TempDir()
	policy := map[string]any{
		"schema": "urn:uip:release-trust:v1", "policy": "vendor-key-offline-v1", "trust_epoch": 1,
		"release_keys":         []any{map[string]string{"id": "signer", "algorithm": "ecdsa-p256", "public_key_pem": public}},
		"build_keys":           []any{map[string]string{"id": "signer", "algorithm": "ecdsa-p256", "public_key_pem": public}},
		"allowed_builders":     []string{"builder://test"},
		"allowed_build_types":  []string{"https://uip.dev/buildtypes/container-v1"},
		"min_release_sequence": 1,
	}
	raw, _ := json.Marshal(policy)
	path := filepath.Join(dir, "trust-policy.json")
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	_, ring, _, err := releaseintegrity.LoadTrustPolicy(dir)
	if err != nil {
		t.Fatal(err)
	}
	return ring
}

func TestSignBlobAndDSSEVerify(t *testing.T) {
	dir := t.TempDir()
	keyPath, public := writeKey(t, dir)
	input := filepath.Join(dir, "input.bin")
	if err := os.WriteFile(input, []byte("release payload"), 0o600); err != nil {
		t.Fatal(err)
	}
	blobBundle := filepath.Join(dir, "blob.bundle.json")
	if err := run("blob", []string{"--key", keyPath, "--input", input, "--output", blobBundle}); err != nil {
		t.Fatalf("blob sign: %v", err)
	}
	bundle, err := os.ReadFile(blobBundle)
	if err != nil {
		t.Fatal(err)
	}
	ring := trustRing(t, public)
	if _, err := releaseintegrity.VerifyBlobBundle(bundle, []byte("release payload"), ring); err != nil {
		t.Fatalf("blob verify: %v", err)
	}
	if _, err := releaseintegrity.VerifyBlobBundle(bundle, []byte("tampered"), ring); err == nil {
		t.Fatal("tampered blob must fail verification")
	}

	statement := filepath.Join(dir, "statement.json")
	if err := os.WriteFile(statement, []byte(`{"_type":"https://in-toto.io/Statement/v1","predicateType":"https://slsa.dev/provenance/v1","subject":[],"predicate":{}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	dsseBundle := filepath.Join(dir, "dsse.bundle.json")
	if err := run("dsse", []string{"--key", keyPath, "--input", statement, "--output", dsseBundle, "--key-id", "builder"}); err != nil {
		t.Fatalf("dsse sign: %v", err)
	}
	rawBundle, err := os.ReadFile(dsseBundle)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := releaseintegrity.VerifyDSSEBundle(rawBundle, []byte(`{"_type":"https://in-toto.io/Statement/v1","predicateType":"https://slsa.dev/provenance/v1","subject":[],"predicate":{}}`), ring); err != nil {
		t.Fatalf("dsse verify: %v", err)
	}
}

func TestRejectWideKeyPermissions(t *testing.T) {
	dir := t.TempDir()
	keyPath, _ := writeKey(t, dir)
	if err := os.Chmod(keyPath, 0o644); err != nil {
		t.Fatal(err)
	}
	input := filepath.Join(dir, "input")
	if err := os.WriteFile(input, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := run("blob", []string{"--key", keyPath, "--input", input, "--output", filepath.Join(dir, "out")}); err == nil {
		t.Fatal("wide key permissions must be rejected")
	}
}

func TestRejectEncryptedKey(t *testing.T) {
	dir := t.TempDir()
	keyPath := filepath.Join(dir, "encrypted.key")
	if err := os.WriteFile(keyPath, pem.EncodeToMemory(&pem.Block{Type: "ENCRYPTED PRIVATE KEY", Bytes: []byte("not-a-real-key")}), 0o600); err != nil {
		t.Fatal(err)
	}
	input := filepath.Join(dir, "input")
	if err := os.WriteFile(input, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := run("blob", []string{"--key", keyPath, "--input", input, "--output", filepath.Join(dir, "out")}); err == nil {
		t.Fatal("encrypted key must be rejected by the reference signer")
	}
}
