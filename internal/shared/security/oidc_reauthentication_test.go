package security

import (
	"crypto/ed25519"
	"crypto/rand"
	"testing"
	"time"
)

// TestOIDCReauthenticationProof 验证重新认证凭据绑定原请求且不能跨用途使用。
// 参数：t为签名与固定时钟测试上下文。
// 返回值：无；篡改、错请求、过期或访问令牌替代被接受时失败。
func TestOIDCReauthenticationProof(t *testing.T) {
	public, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	manager := &OIDCJWTManager{issuer: "https://platform.example", keyID: "test", privateKey: private, publicKey: public}
	now := time.Now().UTC().Truncate(time.Second)
	proof, err := manager.IssueReauthenticationProof("request-hash", "old-session", now)
	if err != nil {
		t.Fatal(err)
	}
	old, after, err := manager.VerifyReauthenticationProof(proof, "request-hash", now.Add(time.Minute))
	if err != nil || old != "old-session" || !after.Equal(now) {
		t.Fatal("valid proof rejected", err)
	}
	for _, tc := range []struct {
		token, hash string
		at          time.Time
	}{{proof + "x", "request-hash", now}, {proof, "different-request", now}, {proof, "request-hash", now.Add(5 * time.Minute)}} {
		if _, _, err := manager.VerifyReauthenticationProof(tc.token, tc.hash, tc.at); err == nil {
			t.Fatal("invalid proof accepted")
		}
	}
	if _, err := manager.VerifyAccessToken(proof, "request-hash", now); err == nil {
		t.Fatal("reauthentication proof accepted as access token")
	}
}
