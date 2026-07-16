package security

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"encoding/pem"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestJWTManagerIssuesAndVerifiesEd25519Token(t *testing.T) {
	privatePath, publicPath := writeJWTKeyPair(t)
	manager, err := LoadJWTManager("issuer", "audience", privatePath, publicPath)
	if err != nil {
		t.Fatalf("load JWT manager: %v", err)
	}

	now := time.Date(2026, 7, 16, 8, 0, 0, 0, time.UTC)
	claims := TokenClaims{
		SessionID: "01J00000000000000000000010", UserID: "01J00000000000000000000011",
		TenantID: "01J00000000000000000000012", AccountID: "01J00000000000000000000013",
		IssuedAt: now, ExpiresAt: now.Add(time.Hour),
	}
	token, err := manager.Issue(claims)
	if err != nil {
		t.Fatalf("issue JWT: %v", err)
	}
	verified, err := manager.Verify(token, now.Add(time.Minute))
	if err != nil {
		t.Fatalf("verify JWT: %v", err)
	}
	if verified != claims {
		t.Fatalf("verified claims = %#v, want %#v", verified, claims)
	}
	if _, err := manager.Verify(token, now.Add(2*time.Hour)); err == nil {
		t.Fatal("expired JWT was accepted")
	}
}

func writeJWTKeyPair(t *testing.T) (string, string) {
	t.Helper()
	publicKey, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generate Ed25519 key pair: %v", err)
	}
	privateDER, err := x509.MarshalPKCS8PrivateKey(privateKey)
	if err != nil {
		t.Fatalf("marshal private key: %v", err)
	}
	publicDER, err := x509.MarshalPKIXPublicKey(publicKey)
	if err != nil {
		t.Fatalf("marshal public key: %v", err)
	}

	privatePath := filepath.Join(t.TempDir(), "jwt-private.pem")
	publicPath := filepath.Join(filepath.Dir(privatePath), "jwt-public.pem")
	if err := os.WriteFile(privatePath, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: privateDER}), 0o600); err != nil {
		t.Fatalf("write private key: %v", err)
	}
	if err := os.WriteFile(publicPath, pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: publicDER}), 0o600); err != nil {
		t.Fatalf("write public key: %v", err)
	}
	return privatePath, publicPath
}
