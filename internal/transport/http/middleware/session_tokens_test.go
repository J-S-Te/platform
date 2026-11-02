package middleware

import (
	"context"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// TestSessionTokensRejectUntrustedBinding 验证中央桥接仅接受同一可信登录的完整令牌对。
// 参数：t为测试上下文。
// 返回值：无；跨client/sid/身份/nonce或缺原始时间未被拒绝时失败。
func TestSessionTokensRejectUntrustedBinding(t *testing.T) {
	key := newTestKeycloakRSAKey(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		writeKeycloakJWKS(t, w, map[string]*rsa.PrivateKey{"current": key})
	}))
	defer server.Close()
	issuer := server.URL + "/realms/basic-platform"
	v, err := NewKeycloakBrokerJWTVerifier(issuer, server.Client())
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Second)
	v.now = func() time.Time { return now }
	sum := sha256.Sum256([]byte("browser-nonce"))
	challenge := base64.RawURLEncoding.EncodeToString(sum[:])
	for _, name := range []string{"valid", "missing auth_time", "future auth_time", "wrong sid", "wrong client", "wrong identity", "wrong nonce", "wrong token use"} {
		t.Run(name, func(t *testing.T) {
			access := validKeycloakBrokerClaims(issuer, now)
			access["azp"] = "client"
			access["token_use"] = "access_token"
			id := validKeycloakBrokerClaims(issuer, now)
			id["azp"] = "client"
			id["token_use"] = "id_token"
			id["nonce"] = "browser-nonce"
			id["auth_time"] = now.Add(-time.Hour).Unix()
			switch name {
			case "missing auth_time":
				delete(id, "auth_time")
			case "future auth_time":
				id["auth_time"] = now.Add(time.Minute).Unix()
			case "wrong sid":
				id["sid"] = "other-browser"
			case "wrong client":
				id["aud"] = "other-client"
				id["azp"] = "other-client"
			case "wrong identity":
				id["identity_id"] = "other-user"
			case "wrong nonce":
				id["nonce"] = "other-nonce"
			case "wrong token use":
				access["token_use"] = "id_token"
			}
			claims, err := v.VerifySessionTokens(context.Background(), signedKeycloakBrokerJWT(t, key, "current", access), signedKeycloakBrokerJWT(t, key, "current", id), "client", challenge)
			if name != "valid" {
				if err == nil {
					t.Fatal("invalid binding accepted")
				}
				return
			}
			if err != nil || !claims.AuthenticatedAt.Equal(now.Add(-time.Hour)) {
				t.Fatalf("original auth_time lost: %+v %v", claims, err)
			}
		})
	}
}
