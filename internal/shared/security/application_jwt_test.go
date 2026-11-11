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

// writeApplicationJWTKeyPair 在临时目录中写入签名密钥夹具。
// 参数：t为测试上下文，public/private可来自相同或不同密钥对。
// 返回值：私钥和公钥的临时文件路径；写入失败时终止测试。
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

// TestLoadApplicationJWTManagerRejectsMismatchedKeyPair 验证错配密钥在启动加载时被拒绝。
// 参数：t为测试上下文。
// 返回值：无；加载未失败或错误无法定位密钥错配时失败。
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

// TestLoadApplicationJWTManagerAcceptsMatchingKeyPair 验证配对密钥支持签发及独立验签。
// 参数：t为测试上下文。
// 返回值：无；加载、签发或任一验证路径异常时失败。
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
