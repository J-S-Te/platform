package middleware

import (
	"bytes"
	"context"
	"crypto"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/J-S-Te/Basic-Platform/internal/shared/httperror"
	"github.com/J-S-Te/Basic-Platform/internal/shared/httpresponse"
	"github.com/J-S-Te/Basic-Platform/internal/shared/keycloakctx"
	"github.com/gin-gonic/gin"
)

const (
	maxKeycloakBrokerJWTLength = 16 * 1024

	// Keycloak JWKS 正常情况下仅在签名密钥轮换时变化。常规缓存保持较短使轮换快速收敛；
	// 过期兜底窗口只允许作用于“此前已验证过的密钥集”，且明显更短。该兜底窗口不是
	// 认证旁路：令牌仍必须由缓存中的已知密钥签名并通过全部常规声明校验。
	defaultKeycloakJWKSCacheTTL = 5 * time.Minute
	defaultKeycloakJWKSMaxStale = time.Minute
)

// KeycloakBrokerJWTVerifier 只校验唯一配置的 Keycloak realm 签发的终端用户 JWT，
// 有意不作为可复用的平台认证机制。
type KeycloakBrokerJWTVerifier struct {
	issuer   string
	jwksURL  string
	client   *http.Client
	now      func() time.Time
	cacheTTL time.Duration
	maxStale time.Duration
	cache    keycloakJWKSCache
}

// KeycloakAuthorizationAccessTokenClaims 是 authorization-context 端点使用的严格、
// 应用绑定令牌视图。broker 校验维持较窄的兼容契约，而授权查询必须额外证明该令牌
// 是 access token，且其授权方（azp）出现在调用方 Client 的 audience 中。
type KeycloakAuthorizationAccessTokenClaims struct {
	keycloakctx.BrokerClaims
	TokenUse string
}

// keycloakJWKSCache 合并并发刷新且不持锁跨越网络请求。完成的抓取以整体替换 map
// 而非原地修改，返回给并发校验的密钥因此保持不可变。
type keycloakJWKSCache struct {
	mu         sync.Mutex
	keys       map[string]*rsa.PublicKey
	freshUntil time.Time
	staleUntil time.Time
	refreshing bool
	done       chan struct{}
}

// NewKeycloakBrokerJWTVerifier 以 issuer 同时作为令牌信任边界与 JWKS 抓取地址构造校验器。
func NewKeycloakBrokerJWTVerifier(issuer string, client *http.Client) (*KeycloakBrokerJWTVerifier, error) {
	return NewKeycloakBrokerJWTVerifierWithBackchannel(issuer, issuer, client)
}

// NewKeycloakBrokerJWTVerifierWithBackchannel 以浏览器可见的 issuer 作为令牌信任边界，
// 而从单独配置的容器可达 issuer URL 抓取签名密钥。当公网 Keycloak 域名仅能被用户
// 浏览器解析（如 Docker 发布的 localhost 端口）时必须使用此构造。
func NewKeycloakBrokerJWTVerifierWithBackchannel(issuer, backchannelIssuer string, client *http.Client) (*KeycloakBrokerJWTVerifier, error) {
	issuer = strings.TrimRight(strings.TrimSpace(issuer), "/")
	if !validKeycloakIssuerURL(issuer) {
		return nil, errors.New("Keycloak broker issuer must be an absolute URL without credentials, query or fragment")
	}
	backchannelIssuer = strings.TrimRight(strings.TrimSpace(backchannelIssuer), "/")
	if !validKeycloakIssuerURL(backchannelIssuer) {
		return nil, errors.New("Keycloak broker backchannel issuer must be an absolute URL without credentials, query or fragment")
	}
	if client == nil {
		client = &http.Client{Timeout: 5 * time.Second}
	}
	return &KeycloakBrokerJWTVerifier{
		issuer: issuer, jwksURL: backchannelIssuer + "/protocol/openid-connect/certs", client: client, now: time.Now,
		cacheTTL: defaultKeycloakJWKSCacheTTL, maxStale: defaultKeycloakJWKSMaxStale,
	}, nil
}

func validKeycloakIssuerURL(value string) bool {
	parsed, err := url.ParseRequestURI(value)
	return err == nil && (parsed.Scheme == "http" || parsed.Scheme == "https") && parsed.Host != "" && parsed.User == nil && parsed.RawQuery == "" && parsed.Fragment == ""
}

// KeycloakBrokerAuthentication 仅服务于专用的 broker 校验路由，接受 bearer token。
// 校验使用有界缓存的“已成功解析”Keycloak JWKS 文档：Keycloak 短暂故障不会让所有
// 在途 broker 校验同时失败，但缓存绝不接受未知密钥，也不会超出其短暂过期窗口。
func KeycloakBrokerAuthentication(verifier *KeycloakBrokerJWTVerifier) gin.HandlerFunc {
	return func(c *gin.Context) {
		if verifier == nil {
			c.Abort()
			httpresponse.WriteError(c.Writer, c.Request, http.StatusInternalServerError, httperror.Internal)
			return
		}
		token, ok := bearerToken(c.GetHeader("Authorization"))
		if !ok {
			c.Abort()
			httpresponse.WriteError(c.Writer, c.Request, http.StatusUnauthorized, httperror.Unauthenticated)
			return
		}
		claims, err := verifier.Verify(c.Request.Context(), token)
		if err != nil {
			c.Abort()
			httpresponse.WriteError(c.Writer, c.Request, http.StatusUnauthorized, httperror.Unauthenticated)
			return
		}
		c.Request = c.Request.WithContext(keycloakctx.WithBrokerClaims(c.Request.Context(), claims))
		c.Next()
	}
}

// Verify 验签并校验一个 Keycloak broker JWT，返回收窄后的最小声明集。
// 签名密钥只来自有界缓存的 Keycloak JWKS；任何校验失败都返回错误且不产生身份。
func (verifier *KeycloakBrokerJWTVerifier) Verify(ctx context.Context, raw string) (keycloakctx.BrokerClaims, error) {
	if verifier == nil || len(raw) == 0 || len(raw) > maxKeycloakBrokerJWTLength {
		return keycloakctx.BrokerClaims{}, errors.New("invalid Keycloak broker JWT")
	}
	parts := strings.Split(raw, ".")
	if len(parts) != 3 || parts[0] == "" || parts[1] == "" || parts[2] == "" {
		return keycloakctx.BrokerClaims{}, errors.New("invalid Keycloak broker JWT serialization")
	}
	var header struct {
		Algorithm string          `json:"alg"`
		KeyID     string          `json:"kid"`
		Critical  json.RawMessage `json:"crit"`
	}
	if err := decodeKeycloakJSONObject(parts[0], &header); err != nil || header.Algorithm != "RS256" || strings.TrimSpace(header.KeyID) == "" || len(header.Critical) != 0 {
		return keycloakctx.BrokerClaims{}, errors.New("unsupported Keycloak broker JWT header")
	}
	key, err := verifier.keyFor(ctx, header.KeyID)
	if err != nil {
		return keycloakctx.BrokerClaims{}, fmt.Errorf("fetch Keycloak JWKS: %w", err)
	}
	signature, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil || rsa.VerifyPKCS1v15(key, crypto.SHA256, sha256Digest(parts[0]+"."+parts[1]), signature) != nil {
		return keycloakctx.BrokerClaims{}, errors.New("Keycloak broker JWT signature is invalid")
	}
	var payload struct {
		Issuer          string          `json:"iss"`
		Subject         string          `json:"sub"`
		Audience        json.RawMessage `json:"aud"`
		ExpiresAt       json.RawMessage `json:"exp"`
		IssuedAt        json.RawMessage `json:"iat"`
		NotBefore       json.RawMessage `json:"nbf"`
		SessionID       string          `json:"sid"`
		TenantID        string          `json:"tenant_id"`
		IdentityID      string          `json:"identity_id"`
		AuthorizedParty string          `json:"azp"`
	}
	if err := decodeKeycloakJSONObject(parts[1], &payload); err != nil {
		return keycloakctx.BrokerClaims{}, errors.New("invalid Keycloak broker JWT claims")
	}
	// Keycloak 保留的 `sub` 仍是 issuer 原生主体；平台 identity_id mapper 是独立的
	// 必需声明，专用于授权，二者不可混用。
	payload.Subject = strings.TrimSpace(payload.Subject)
	payload.IdentityID = strings.TrimSpace(payload.IdentityID)
	audience, ok := keycloakAudience(payload.Audience)
	expiresAt, okExp := keycloakNumericDate(payload.ExpiresAt)
	issuedAt, okIat := keycloakNumericDate(payload.IssuedAt)
	if !ok || !okExp || !okIat || payload.Issuer != verifier.issuer || payload.Subject == "" || strings.TrimSpace(payload.SessionID) == "" || strings.TrimSpace(payload.TenantID) == "" || payload.IdentityID == "" {
		return keycloakctx.BrokerClaims{}, errors.New("required Keycloak broker JWT claims are invalid")
	}
	now := verifier.now().UTC()
	if !expiresAt.After(now) || issuedAt.After(now.Add(time.Minute)) {
		return keycloakctx.BrokerClaims{}, errors.New("Keycloak broker JWT is outside its valid time window")
	}
	if len(payload.NotBefore) != 0 {
		notBefore, valid := keycloakNumericDate(payload.NotBefore)
		if !valid || notBefore.After(now.Add(time.Minute)) {
			return keycloakctx.BrokerClaims{}, errors.New("Keycloak broker JWT is not yet valid")
		}
	}
	return keycloakctx.BrokerClaims{Issuer: payload.Issuer, Subject: payload.Subject, SessionID: payload.SessionID, TenantID: payload.TenantID, IdentityID: payload.IdentityID, AuthorizedParty: strings.TrimSpace(payload.AuthorizedParty), Audience: audience}, nil
}

// KeycloakIDTokenClaims 是平台自身 OIDC 登录回调使用的严格 ID-token 视图。
// broker 校验器先完成签名认证；本层再证明该令牌是绑定平台 OIDC client 且
// 携带预期 nonce 的 ID token。
type KeycloakIDTokenClaims struct {
	keycloakctx.BrokerClaims
	Nonce string
}

// VerifyIDToken 在 ID token 完成平台 OIDC 登录前追加令牌用途、nonce 与 Client
// 绑定校验。签名的负载只在 Verify 认证通过后才再次解码，未认证值不会到达调用方。
func (verifier *KeycloakBrokerJWTVerifier) VerifyIDToken(ctx context.Context, raw, expectedNonce, expectedClientID string) (KeycloakIDTokenClaims, error) {
	claims, err := verifier.Verify(ctx, raw)
	if err != nil {
		return KeycloakIDTokenClaims{}, err
	}
	parts := strings.Split(raw, ".")
	if len(parts) != 3 {
		return KeycloakIDTokenClaims{}, errors.New("invalid Keycloak ID token serialization")
	}
	var payload struct {
		Nonce    string `json:"nonce"`
		TokenUse string `json:"token_use"`
	}
	if err := decodeKeycloakJSONObject(parts[1], &payload); err != nil {
		return KeycloakIDTokenClaims{}, errors.New("invalid Keycloak ID token claims")
	}
	nonce := strings.TrimSpace(payload.Nonce)
	tokenUse := strings.TrimSpace(payload.TokenUse)
	clientID := strings.TrimSpace(expectedClientID)
	if tokenUse != "id_token" || clientID == "" || !keycloakAudienceContains(claims.Audience, clientID) {
		return KeycloakIDTokenClaims{}, errors.New("Keycloak ID token is not bound to the platform OIDC client")
	}
	if expectedNonce == "" || nonce == "" || nonce != expectedNonce {
		return KeycloakIDTokenClaims{}, errors.New("Keycloak ID token nonce does not match")
	}
	if claims.AuthorizedParty != "" && claims.AuthorizedParty != clientID {
		return KeycloakIDTokenClaims{}, errors.New("Keycloak ID token authorized party does not match")
	}
	return KeycloakIDTokenClaims{BrokerClaims: claims, Nonce: nonce}, nil
}

// VerifyAuthorizationAccessToken 在 Keycloak 令牌可用于选择应用授权上下文前追加
// 令牌用途与 Client audience 校验。签名的负载只在 Verify 认证通过后才再次解码；
// 任何未认证的值都不会到达调用方。
func (verifier *KeycloakBrokerJWTVerifier) VerifyAuthorizationAccessToken(ctx context.Context, raw string) (KeycloakAuthorizationAccessTokenClaims, error) {
	claims, err := verifier.Verify(ctx, raw)
	if err != nil {
		return KeycloakAuthorizationAccessTokenClaims{}, err
	}
	parts := strings.Split(raw, ".")
	if len(parts) != 3 {
		return KeycloakAuthorizationAccessTokenClaims{}, errors.New("invalid Keycloak access token serialization")
	}
	var payload struct {
		TokenUse string `json:"token_use"`
	}
	if err := decodeKeycloakJSONObject(parts[1], &payload); err != nil {
		return KeycloakAuthorizationAccessTokenClaims{}, errors.New("invalid Keycloak access token claims")
	}
	tokenUse := strings.TrimSpace(payload.TokenUse)
	clientID := strings.TrimSpace(claims.AuthorizedParty)
	if tokenUse != "access_token" || clientID == "" || !keycloakAudienceContains(claims.Audience, clientID) {
		return KeycloakAuthorizationAccessTokenClaims{}, errors.New("Keycloak access token is not bound to its authorized Client")
	}
	return KeycloakAuthorizationAccessTokenClaims{BrokerClaims: claims, TokenUse: tokenUse}, nil
}

// keyFor 返回经过校验的 Keycloak 签名密钥。缓存命中仅在新鲜期内使用；kid 未知时
// 即使缓存仍新鲜也强制刷新一次，使 Keycloak 签名密钥轮换立即收敛。刷新失败时
// 只允许使用已知的旧密钥，且仅限上次成功抓取建立的有界过期窗口之内。
func (verifier *KeycloakBrokerJWTVerifier) keyFor(ctx context.Context, keyID string) (*rsa.PublicKey, error) {
	if verifier == nil {
		return nil, errors.New("Keycloak broker verifier is not initialized")
	}
	for {
		now := verifier.now().UTC()
		verifier.cache.mu.Lock()
		key := verifier.cache.keys[keyID]
		if key != nil && now.Before(verifier.cache.freshUntil) {
			verifier.cache.mu.Unlock()
			return key, nil
		}
		if verifier.cache.refreshing {
			done := verifier.cache.done
			verifier.cache.mu.Unlock()
			select {
			case <-done:
				continue
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}

		// 释放锁之前先捕获过期密钥。下方的缓存替换从不原地修改 map，
		// 抓取失败时继续使用它是安全的。
		staleKey := key
		staleUntil := verifier.cache.staleUntil
		verifier.cache.refreshing = true
		verifier.cache.done = make(chan struct{})
		done := verifier.cache.done
		verifier.cache.mu.Unlock()

		keys, err := verifier.fetchKeys(ctx)
		completedAt := verifier.now().UTC()

		verifier.cache.mu.Lock()
		if err == nil {
			verifier.cache.keys = keys
			verifier.cache.freshUntil = completedAt.Add(verifier.cacheTTL)
			verifier.cache.staleUntil = verifier.cache.freshUntil.Add(verifier.maxStale)
		}
		verifier.cache.refreshing = false
		close(done)
		verifier.cache.done = nil
		verifier.cache.mu.Unlock()

		if err != nil {
			if staleKey != nil && completedAt.Before(staleUntil) {
				return staleKey, nil
			}
			return nil, err
		}
		key = keys[keyID]
		if key == nil {
			return nil, errors.New("Keycloak broker JWT signing key is unknown")
		}
		return key, nil
	}
}

func (verifier *KeycloakBrokerJWTVerifier) fetchKeys(ctx context.Context) (map[string]*rsa.PublicKey, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, verifier.jwksURL, nil)
	if err != nil {
		return nil, err
	}
	request.Header.Set("Accept", "application/json")
	response, err := verifier.client.Do(request)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("unexpected JWKS status %d", response.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, 1<<20))
	if err != nil {
		return nil, err
	}
	var document struct {
		Keys []struct {
			KeyID     string `json:"kid"`
			KeyType   string `json:"kty"`
			Algorithm string `json:"alg"`
			Use       string `json:"use"`
			Modulus   string `json:"n"`
			Exponent  string `json:"e"`
		} `json:"keys"`
	}
	if err := json.Unmarshal(body, &document); err != nil {
		return nil, err
	}
	keys := make(map[string]*rsa.PublicKey)
	for _, item := range document.Keys {
		if item.KeyType != "RSA" || item.Algorithm != "RS256" || (item.Use != "" && item.Use != "sig") || item.KeyID == "" {
			continue
		}
		n, nErr := base64.RawURLEncoding.DecodeString(item.Modulus)
		e, eErr := base64.RawURLEncoding.DecodeString(item.Exponent)
		if nErr != nil || eErr != nil || len(n) < 256 || len(e) == 0 {
			continue
		}
		exponent := new(big.Int).SetBytes(e)
		if !exponent.IsInt64() || exponent.Int64() < 3 || exponent.Int64()%2 == 0 {
			continue
		}
		publicKey := &rsa.PublicKey{N: new(big.Int).SetBytes(n), E: int(exponent.Int64())}
		if publicKey.N.BitLen() < 2048 || publicKey.N.Bit(0) == 0 {
			continue
		}
		if _, duplicate := keys[item.KeyID]; duplicate {
			return nil, errors.New("duplicate Keycloak JWKS kid")
		}
		keys[item.KeyID] = publicKey
	}
	if len(keys) == 0 {
		return nil, errors.New("Keycloak JWKS contains no acceptable signing keys")
	}
	return keys, nil
}

func decodeKeycloakJSONObject(part string, destination any) error {
	decoded, err := base64.RawURLEncoding.DecodeString(part)
	if err != nil {
		return err
	}
	decoder := json.NewDecoder(bytes.NewReader(decoded))
	var raw map[string]json.RawMessage
	if err := decoder.Decode(&raw); err != nil {
		return err
	}
	if len(raw) == 0 {
		return errors.New("JSON object required")
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		return errors.New("unexpected JSON data")
	}
	return json.Unmarshal(decoded, destination)
}

func sha256Digest(value string) []byte { digest := sha256.Sum256([]byte(value)); return digest[:] }

func keycloakAudience(raw json.RawMessage) ([]string, bool) {
	var one string
	if json.Unmarshal(raw, &one) == nil && strings.TrimSpace(one) != "" {
		return []string{one}, true
	}
	var many []string
	if json.Unmarshal(raw, &many) != nil || len(many) == 0 {
		return nil, false
	}
	seen := make(map[string]struct{}, len(many))
	for _, value := range many {
		if strings.TrimSpace(value) == "" {
			return nil, false
		}
		if _, exists := seen[value]; exists {
			return nil, false
		}
		seen[value] = struct{}{}
	}
	return many, true
}

func keycloakAudienceContains(audience []string, expected string) bool {
	for _, value := range audience {
		if value == expected {
			return true
		}
	}
	return false
}

func keycloakNumericDate(raw json.RawMessage) (time.Time, bool) {
	var seconds json.Number
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	if decoder.Decode(&seconds) != nil {
		return time.Time{}, false
	}
	value, err := seconds.Int64()
	if err != nil || value <= 0 {
		return time.Time{}, false
	}
	return time.Unix(value, 0).UTC(), true
}
