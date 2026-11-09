package security

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"encoding/pem"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// writeApplicationJWTKeyPair 把给定的公钥和私钥写成 PKCS#8 / PKIX PEM，供加载自检用例
// 组合出“匹配”和“不匹配”的配置。公钥与私钥可以来自不同密钥对，用来模拟配置错误。
func writeApplicationJWTKeyPair(t *testing.T, public ed25519.PublicKey, private ed25519.PrivateKey) (string, string) {
	t.Helper()
	privateDER, err := x509.MarshalPKCS8PrivateKey(private)
	if err != nil {
		t.Fatalf("marshal application JWT private key: %v", err)
	}
	publicDER, err := x509.MarshalPKIXPublicKey(public)
	if err != nil {
		t.Fatalf("marshal application JWT public key: %v", err)
	}
	dir := t.TempDir()
	privatePath := filepath.Join(dir, "application-private.pem")
	publicPath := filepath.Join(dir, "application-public.pem")
	if err := os.WriteFile(privatePath, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: privateDER}), 0o600); err != nil {
		t.Fatalf("write application JWT private key: %v", err)
	}
	if err := os.WriteFile(publicPath, pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: publicDER}), 0o600); err != nil {
		t.Fatalf("write application JWT public key: %v", err)
	}
	return privatePath, publicPath
}

// 私钥派生公钥与配置公钥不配对时，签发实例签出的机器 Token 没有任何验证方能够接受；
// 这种配置错误必须在加载阶段失败，并指出是应用 JWT 的密钥不匹配。
func TestLoadApplicationJWTManagerRejectsMismatchedKeyPair(t *testing.T) {
	_, signingPrivate, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generate application JWT signing key: %v", err)
	}
	otherPublic, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generate unrelated application JWT public key: %v", err)
	}
	privatePath, publicPath := writeApplicationJWTKeyPair(t, otherPublic, signingPrivate)
	manager, err := LoadApplicationJWTManager("application-issuer", "application-audience", privatePath, publicPath)
	if err == nil {
		t.Fatal("LoadApplicationJWTManager accepted a mismatched private/public key pair")
	}
	if manager != nil {
		t.Fatalf("LoadApplicationJWTManager returned a manager alongside error: %v", manager)
	}
	if !strings.Contains(err.Error(), "does not match") || !strings.Contains(err.Error(), "application JWT") {
		t.Fatalf("mismatched key error is not diagnosable: %v", err)
	}
}

// 自检不得破坏正常路径：配对密钥应能加载，签发与验证仍自洽，独立验证方只加载公钥也可用。
func TestLoadApplicationJWTManagerAcceptsMatchingKeyPair(t *testing.T) {
	public, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generate application JWT key pair: %v", err)
	}
	privatePath, publicPath := writeApplicationJWTKeyPair(t, public, private)
	manager, err := LoadApplicationJWTManager("application-issuer", "application-audience", privatePath, publicPath)
	if err != nil {
		t.Fatalf("LoadApplicationJWTManager rejected a matching key pair: %v", err)
	}
	now := time.Now().UTC().Truncate(time.Second)
	claims := ApplicationTokenClaims{
		OAuthClientID: "oauth-client", ClientID: "client", TenantID: "tenant",
		ApplicationID: "application", ApplicationCode: "contract_management",
		EnvironmentID: "environment", EnvironmentCode: "prod", Scopes: []string{"license.runtime"},
		IssuedAt: now, ExpiresAt: now.Add(5 * time.Minute),
	}
	token, err := manager.Issue(claims)
	if err != nil {
		t.Fatalf("issue application JWT: %v", err)
	}
	parsed, err := manager.Verify(token, now)
	if err != nil {
		t.Fatalf("verify application JWT: %v", err)
	}
	if parsed.ClientID != claims.ClientID || parsed.OAuthClientID != claims.OAuthClientID || parsed.TenantID != claims.TenantID {
		t.Fatalf("verified claims drifted from issued claims: %+v", parsed)
	}
	verifier, err := LoadApplicationJWTVerifier("application-issuer", "application-audience", publicPath)
	if err != nil {
		t.Fatalf("LoadApplicationJWTVerifier rejected a matching public key: %v", err)
	}
	if _, err := verifier.Verify(token, now); err != nil {
		t.Fatalf("independent verifier rejected a token signed by the matching private key: %v", err)
	}
}
