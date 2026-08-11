package application

import (
	"bytes"
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/sha512"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"strings"
	"time"

	"github.com/J-S-Te/Basic-Platform/internal/platform/oidc/domain"
)

const clientAssertionTypeJWTBearer = "urn:ietf:params:oauth:client-assertion-type:jwt-bearer"

// ErrClientAssertionReplay 表示同一客户端的 jti 已被持久化占用。协议边界统一映射为
// invalid_client，避免向攻击者区分签名、时间窗和重放校验中的具体失败点。
var ErrClientAssertionReplay = errors.New("OIDC client assertion was already used")

// ClientAssertionReplayRepository 保存已接受 jti 的摘要；数据库唯一约束负责跨实例防重放，
// 不能替换成仅对单进程有效的内存缓存。
type ClientAssertionReplayRepository interface {
	RecordClientAssertionReplay(ctx context.Context, oauthClientID string, jtiHash [32]byte, expiresAt, now time.Time) error
}

type compactJWT struct {
	Header       map[string]json.RawMessage
	Claims       map[string]json.RawMessage
	SigningInput []byte
	Signature    []byte
}

func authenticatePrivateKeyJWT(ctx context.Context, repo Repository, client domain.Client, auth ClientAuthentication, now time.Time) error {
	// 断言同时绑定 iss、sub、aud 与当前客户端，且有效期最多五分钟。签名正确并不足以证明
	// 断言可用于当前令牌端点，尤其不能把为其他 audience 签发的 JWT 拿来复用。
	if strings.TrimSpace(auth.ClientAssertionType) != clientAssertionTypeJWTBearer ||
		!validProtocolText(strings.TrimSpace(auth.ClientAssertionAudience), 2048) {
		return ErrInvalidClient
	}
	token, err := parseCompactJWT(auth.ClientAssertion)
	if err != nil || verifyCompactJWT(token, client.JWKs) != nil {
		return ErrInvalidClient
	}
	issuer, ok := jwtString(token.Claims, "iss")
	if !ok || issuer != client.ClientID {
		return ErrInvalidClient
	}
	subject, ok := jwtString(token.Claims, "sub")
	if !ok || subject != client.ClientID || !jwtAudienceContains(token.Claims["aud"], auth.ClientAssertionAudience) {
		return ErrInvalidClient
	}
	jti, ok := jwtString(token.Claims, "jti")
	if !ok || !validProtocolText(jti, 512) {
		return ErrInvalidClient
	}
	expiresAt, ok := jwtTime(token.Claims, "exp")
	if !ok || !expiresAt.After(now) || expiresAt.After(now.Add(5*time.Minute)) {
		return ErrInvalidClient
	}
	if issuedAt, present, timeErr := jwtOptionalTime(token.Claims, "iat"); timeErr != nil || (present && (issuedAt.After(now.Add(time.Minute)) || issuedAt.Before(now.Add(-5*time.Minute)))) {
		return ErrInvalidClient
	}
	if notBefore, present, timeErr := jwtOptionalTime(token.Claims, "nbf"); timeErr != nil || (present && notBefore.After(now.Add(time.Minute))) {
		return ErrInvalidClient
	}
	replays, ok := repo.(ClientAssertionReplayRepository)
	if !ok {
		return ErrInvalidClient
	}
	// 只有全部声明和签名通过后才占用 jti；保存摘要而非原文，减少认证材料在数据库中的暴露。
	if err := replays.RecordClientAssertionReplay(ctx, client.ID, sha256.Sum256([]byte(jti)), expiresAt.UTC(), now.UTC()); err != nil {
		if errors.Is(err, ErrClientAssertionReplay) {
			return ErrInvalidClient
		}
		return fmt.Errorf("record client assertion replay guard: %w", err)
	}
	return nil
}

func parseCompactJWT(value string) (compactJWT, error) {
	parts := strings.Split(value, ".")
	if len(parts) != 3 || len(value) > 16*1024 {
		return compactJWT{}, errors.New("invalid compact JWT")
	}
	decode := func(part string) ([]byte, error) { return base64.RawURLEncoding.DecodeString(part) }
	headerBytes, err := decode(parts[0])
	if err != nil {
		return compactJWT{}, err
	}
	claimBytes, err := decode(parts[1])
	if err != nil {
		return compactJWT{}, err
	}
	signature, err := decode(parts[2])
	if err != nil || len(signature) == 0 {
		return compactJWT{}, errors.New("invalid JWT signature")
	}
	header, err := decodeUniqueJSONObject(headerBytes)
	if err != nil {
		return compactJWT{}, errors.New("invalid JWT header")
	}
	claims, err := decodeUniqueJSONObject(claimBytes)
	if err != nil {
		return compactJWT{}, errors.New("invalid JWT claims")
	}
	return compactJWT{Header: header, Claims: claims, SigningInput: []byte(parts[0] + "." + parts[1]), Signature: signature}, nil
}

// decodeUniqueJSONObject 拒绝顶层重名字段。若允许重复的 JWT header/claim，不同 JSON
// 实现可能从同一签名输入选择不同值，造成“验签看到一个值、授权使用另一个值”的解析差异。
func decodeUniqueJSONObject(value []byte) (map[string]json.RawMessage, error) {
	decoder := json.NewDecoder(bytes.NewReader(value))
	token, err := decoder.Token()
	if err != nil {
		return nil, err
	}
	if delimiter, ok := token.(json.Delim); !ok || delimiter != '{' {
		return nil, errors.New("JSON object is required")
	}
	result := make(map[string]json.RawMessage)
	for decoder.More() {
		nameToken, err := decoder.Token()
		if err != nil {
			return nil, err
		}
		name, ok := nameToken.(string)
		if !ok || name == "" {
			return nil, errors.New("invalid JSON object name")
		}
		if _, exists := result[name]; exists {
			return nil, errors.New("duplicate JSON object name")
		}
		var raw json.RawMessage
		if err := decoder.Decode(&raw); err != nil {
			return nil, err
		}
		result[name] = raw
	}
	if token, err = decoder.Token(); err != nil || token != json.Delim('}') {
		return nil, errors.New("invalid JSON object terminator")
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		return nil, errors.New("unexpected JSON data")
	}
	return result, nil
}

func verifyCompactJWT(token compactJWT, keys []domain.ClientJWK) error {
	// 不支持的 crit 必须失败关闭；kid 必须唯一命中登记公钥，禁止尝试所有密钥或在同名键间任选，
	// 否则密钥轮换期间会产生算法与密钥选择歧义。
	if _, usesCriticalHeaders := token.Header["crit"]; usesCriticalHeaders {
		return errors.New("unsupported critical JWT header")
	}
	algorithm, ok := jwtString(token.Header, "alg")
	if !ok || !supportedJWTAlgorithm(algorithm) {
		return errors.New("unsupported JWT algorithm")
	}
	keyID, hasKeyID := jwtString(token.Header, "kid")
	if !hasKeyID || keyID == "" {
		return errors.New("JWT kid is required")
	}
	var matched *domain.ClientJWK
	for index := range keys {
		if subtle.ConstantTimeCompare([]byte(keyID), []byte(keys[index].KeyID)) == 1 {
			if matched != nil {
				return errors.New("ambiguous JWT key")
			}
			matched = &keys[index]
		}
	}
	if matched == nil {
		return errors.New("JWT key was not registered")
	}
	publicKey, registeredAlgorithm, err := parsePublicJWK(matched.PublicJWK)
	if err != nil || (registeredAlgorithm != "" && registeredAlgorithm != algorithm) {
		return errors.New("invalid registered JWK")
	}
	return verifyJWTSignature(algorithm, publicKey, token.SigningInput, token.Signature)
}

func parsePublicJWK(raw []byte) (crypto.PublicKey, string, error) {
	values, err := decodeUniqueJSONObject(raw)
	if err != nil {
		return nil, "", errors.New("invalid JWK")
	}
	for _, privateMember := range []string{"d", "k", "p", "q", "dp", "dq", "qi", "oth"} {
		if _, exists := values[privateMember]; exists {
			return nil, "", errors.New("private JWK member")
		}
	}
	value := func(name string) (string, bool) { return jwtString(values, name) }
	keyType, ok := value("kty")
	if !ok {
		return nil, "", errors.New("missing JWK kty")
	}
	algorithm, _ := value("alg")
	if use, present := value("use"); present && use != "sig" {
		return nil, "", errors.New("JWK is not a signing key")
	}
	if rawOperations, present := values["key_ops"]; present {
		var operations []string
		if json.Unmarshal(rawOperations, &operations) != nil || len(operations) == 0 || !hasJWTKeyOperation(operations, "verify") {
			return nil, "", errors.New("JWK does not allow signature verification")
		}
	}
	decode := func(encoded string) ([]byte, error) { return base64.RawURLEncoding.DecodeString(encoded) }
	switch keyType {
	case "OKP":
		curve, _ := value("crv")
		x, exists := value("x")
		if curve != "Ed25519" || !exists {
			return nil, "", errors.New("invalid OKP JWK")
		}
		bytes, err := decode(x)
		if err != nil || len(bytes) != ed25519.PublicKeySize {
			return nil, "", errors.New("invalid Ed25519 key")
		}
		return ed25519.PublicKey(bytes), algorithm, nil
	case "RSA":
		modulus, hasModulus := value("n")
		exponent, hasExponent := value("e")
		if !hasModulus || !hasExponent {
			return nil, "", errors.New("invalid RSA JWK")
		}
		modulusBytes, modulusErr := decode(modulus)
		exponentBytes, exponentErr := decode(exponent)
		if modulusErr != nil || exponentErr != nil || len(modulusBytes) < 256 || len(exponentBytes) == 0 {
			return nil, "", errors.New("invalid RSA key")
		}
		exponentNumber := new(big.Int).SetBytes(exponentBytes)
		if !exponentNumber.IsInt64() || exponentNumber.Int64() < 3 || exponentNumber.Int64()%2 == 0 {
			return nil, "", errors.New("invalid RSA exponent")
		}
		publicKey := &rsa.PublicKey{N: new(big.Int).SetBytes(modulusBytes), E: int(exponentNumber.Int64())}
		if publicKey.N.BitLen() < 2048 || publicKey.N.Bit(0) == 0 {
			return nil, "", errors.New("invalid RSA modulus")
		}
		return publicKey, algorithm, nil
	case "EC":
		curveName, _ := value("crv")
		x, hasX := value("x")
		y, hasY := value("y")
		curve, coordinateLength := curveForJWT(curveName)
		if !hasX || !hasY || curve == nil {
			return nil, "", errors.New("invalid EC JWK")
		}
		xBytes, xErr := decode(x)
		yBytes, yErr := decode(y)
		if xErr != nil || yErr != nil || len(xBytes) != coordinateLength || len(yBytes) != coordinateLength {
			return nil, "", errors.New("invalid EC key")
		}
		publicKey := &ecdsa.PublicKey{Curve: curve, X: new(big.Int).SetBytes(xBytes), Y: new(big.Int).SetBytes(yBytes)}
		if !curve.IsOnCurve(publicKey.X, publicKey.Y) {
			return nil, "", errors.New("invalid EC point")
		}
		return publicKey, algorithm, nil
	default:
		return nil, "", errors.New("unsupported JWK")
	}
}

func hasJWTKeyOperation(operations []string, expected string) bool {
	for _, operation := range operations {
		if operation == expected {
			return true
		}
	}
	return false
}

func curveForJWT(name string) (elliptic.Curve, int) {
	switch name {
	case "P-256":
		return elliptic.P256(), 32
	case "P-384":
		return elliptic.P384(), 48
	case "P-521":
		return elliptic.P521(), 66
	default:
		return nil, 0
	}
}

func verifyJWTSignature(algorithm string, publicKey crypto.PublicKey, signingInput, signature []byte) error {
	switch algorithm {
	case "EdDSA":
		key, ok := publicKey.(ed25519.PublicKey)
		if !ok || !ed25519.Verify(key, signingInput, signature) {
			return errors.New("invalid EdDSA signature")
		}
		return nil
	case "RS256", "RS384", "RS512":
		key, ok := publicKey.(*rsa.PublicKey)
		if !ok {
			return errors.New("RSA key required")
		}
		hash := hashForJWTAlgorithm(algorithm)
		return rsa.VerifyPKCS1v15(key, hash, jwtDigest(hash, signingInput), signature)
	case "PS256", "PS384", "PS512":
		key, ok := publicKey.(*rsa.PublicKey)
		if !ok {
			return errors.New("RSA key required")
		}
		hash := hashForJWTAlgorithm(algorithm)
		return rsa.VerifyPSS(key, hash, jwtDigest(hash, signingInput), signature, &rsa.PSSOptions{SaltLength: rsa.PSSSaltLengthEqualsHash, Hash: hash})
	case "ES256", "ES384", "ES512":
		key, ok := publicKey.(*ecdsa.PublicKey)
		if !ok {
			return errors.New("EC key required")
		}
		coordinateLength := map[string]int{"ES256": 32, "ES384": 48, "ES512": 66}[algorithm]
		if len(signature) != coordinateLength*2 {
			return errors.New("invalid ECDSA signature length")
		}
		hash := hashForJWTAlgorithm(algorithm)
		if !ecdsa.Verify(key, jwtDigest(hash, signingInput), new(big.Int).SetBytes(signature[:coordinateLength]), new(big.Int).SetBytes(signature[coordinateLength:])) {
			return errors.New("invalid ECDSA signature")
		}
		return nil
	default:
		return errors.New("unsupported JWT algorithm")
	}
}

func supportedJWTAlgorithm(algorithm string) bool {
	switch algorithm {
	case "EdDSA", "RS256", "RS384", "RS512", "PS256", "PS384", "PS512", "ES256", "ES384", "ES512":
		return true
	default:
		return false
	}
}

func hashForJWTAlgorithm(algorithm string) crypto.Hash {
	switch algorithm {
	case "RS256", "PS256", "ES256":
		return crypto.SHA256
	case "RS384", "PS384", "ES384":
		return crypto.SHA384
	default:
		return crypto.SHA512
	}
}

func jwtDigest(hash crypto.Hash, input []byte) []byte {
	switch hash {
	case crypto.SHA256:
		value := sha256.Sum256(input)
		return value[:]
	case crypto.SHA384:
		value := sha512.Sum384(input)
		return value[:]
	default:
		value := sha512.Sum512(input)
		return value[:]
	}
}

func jwtString(values map[string]json.RawMessage, name string) (string, bool) {
	raw, exists := values[name]
	if !exists {
		return "", false
	}
	var value string
	if json.Unmarshal(raw, &value) != nil || value == "" {
		return "", false
	}
	return value, true
}

func jwtTime(values map[string]json.RawMessage, name string) (time.Time, bool) {
	value, present, err := jwtOptionalTime(values, name)
	return value, present && err == nil
}

// jwtOptionalTime distinguishes an absent optional NumericDate from a malformed supplied value.
// Optional time claims must still be valid NumericDates when a client chooses to send them.
func jwtOptionalTime(values map[string]json.RawMessage, name string) (time.Time, bool, error) {
	raw, exists := values[name]
	if !exists {
		return time.Time{}, false, nil
	}
	var seconds json.Number
	if json.Unmarshal(raw, &seconds) != nil {
		return time.Time{}, true, errors.New("invalid NumericDate")
	}
	integer, err := seconds.Int64()
	if err != nil || integer <= 0 {
		return time.Time{}, true, errors.New("invalid NumericDate")
	}
	return time.Unix(integer, 0).UTC(), true, nil
}

func jwtAudienceContains(raw json.RawMessage, expected string) bool {
	var single string
	if json.Unmarshal(raw, &single) == nil {
		return subtle.ConstantTimeCompare([]byte(single), []byte(expected)) == 1
	}
	var multiple []string
	if json.Unmarshal(raw, &multiple) != nil || len(multiple) == 0 {
		return false
	}
	matched := 0
	for _, audience := range multiple {
		matched |= subtle.ConstantTimeCompare([]byte(audience), []byte(expected))
	}
	return matched == 1
}
