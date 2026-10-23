package releaseintegrity

import (
	"crypto"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/pem"
	"os"
	"path/filepath"
	"strings"
)

const (
	TrustSchema = "urn:uip:release-trust:v1"
	// VendorKeyOfflinePolicy 是第一阶段的固定公钥策略，
	// 不要求也不接受在线透明日志或时间戳材料。
	VendorKeyOfflinePolicy = "vendor-key-offline-v1"
)

type TrustPolicy struct {
	Schema             string     `json:"schema"`
	Policy             string     `json:"policy"`
	TrustEpoch         int64      `json:"trust_epoch"`
	ReleaseKeys        []TrustKey `json:"release_keys"`
	BuildKeys          []TrustKey `json:"build_keys"`
	AllowedBuilders    []string   `json:"allowed_builders"`
	AllowedBuildTypes  []string   `json:"allowed_build_types"`
	MinReleaseSequence int64      `json:"min_release_sequence"`
	VerifierMinVersion string     `json:"verifier_min_version,omitempty"`
	VerifierSHA256     string     `json:"verifier_sha256,omitempty"`
}

type TrustKey struct {
	ID           string `json:"id"`
	Algorithm    string `json:"algorithm"`
	PublicKeyPEM string `json:"public_key_pem"`
}

type loadedKey struct {
	id        string
	algorithm string
	public    crypto.PublicKey
}

// KeyRing 是不可变的可信公钥集合，绝不从
// 待验证介质加载密钥。
type KeyRing struct {
	keys []loadedKey
}

// LoadTrustPolicy 从宿主机控制的目录读取 trust-policy.json。
// 此目录不得由介质或浏览器请求指定。
func LoadTrustPolicy(dir string) (*TrustPolicy, *KeyRing, *KeyRing, error) {
	path := filepath.Join(dir, "trust-policy.json")
	info, err := os.Lstat(path)
	if err != nil {
		return nil, nil, nil, fail(CodeTrustUnavailable, "信任策略不可用")
	}
	if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
		return nil, nil, nil, fail(CodeTrustUnavailable, "信任策略不是安全普通文件")
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, nil, nil, fail(CodeTrustUnavailable, "信任策略不可读")
	}
	var policy TrustPolicy
	if err := decodeStrictJSON(raw, &policy); err != nil {
		return nil, nil, nil, failDetails(CodeTrustUnavailable, "信任策略不是严格 JSON", map[string]any{"error": err.Error()})
	}
	if policy.Schema != TrustSchema || policy.Policy != VendorKeyOfflinePolicy {
		return nil, nil, nil, fail(CodeTrustUnavailable, "信任策略 schema/policy 不受支持")
	}
	if policy.TrustEpoch <= 0 {
		return nil, nil, nil, fail(CodeTrustUnavailable, "trust_epoch 必须是正整数")
	}
	if policy.MinReleaseSequence < 0 {
		return nil, nil, nil, fail(CodeTrustUnavailable, "min_release_sequence 不能为负数")
	}
	if len(policy.ReleaseKeys) == 0 || len(policy.BuildKeys) == 0 {
		return nil, nil, nil, fail(CodeTrustUnavailable, "发布/构建信任密钥不能为空")
	}
	if len(policy.AllowedBuilders) == 0 {
		return nil, nil, nil, fail(CodeTrustUnavailable, "allowed_builders 不能为空")
	}
	if len(policy.AllowedBuildTypes) == 0 {
		return nil, nil, nil, fail(CodeTrustUnavailable, "allowed_build_types 不能为空")
	}
	releaseRing, err := loadKeyRing(policy.ReleaseKeys)
	if err != nil {
		return nil, nil, nil, err
	}
	buildRing, err := loadKeyRing(policy.BuildKeys)
	if err != nil {
		return nil, nil, nil, err
	}
	return &policy, releaseRing, buildRing, nil
}

func loadKeyRing(keys []TrustKey) (*KeyRing, error) {
	ring := &KeyRing{keys: make([]loadedKey, 0, len(keys))}
	seen := make(map[string]bool, len(keys))
	for _, key := range keys {
		if !serviceIDRe.MatchString(key.ID) {
			return nil, fail(CodeTrustUnavailable, "信任密钥 id 不合法")
		}
		if seen[key.ID] {
			return nil, fail(CodeTrustUnavailable, "信任密钥 id 重复")
		}
		seen[key.ID] = true
		public, err := parseTrustPublicKey(key)
		if err != nil {
			return nil, err
		}
		ring.keys = append(ring.keys, loadedKey{id: key.ID, algorithm: key.Algorithm, public: public})
	}
	return ring, nil
}

func parseTrustPublicKey(key TrustKey) (crypto.PublicKey, error) {
	block, rest := pem.Decode([]byte(key.PublicKeyPEM))
	if block == nil || len(strings.TrimSpace(string(rest))) != 0 {
		return nil, fail(CodeTrustUnavailable, "信任公钥 PEM 不唯一")
	}
	var public crypto.PublicKey
	var err error
	switch block.Type {
	case "PUBLIC KEY":
		public, err = x509.ParsePKIXPublicKey(block.Bytes)
	case "CERTIFICATE":
		var certificate *x509.Certificate
		certificate, err = x509.ParseCertificate(block.Bytes)
		if err == nil {
			public = certificate.PublicKey
		}
	default:
		return nil, fail(CodeTrustUnavailable, "信任公钥 PEM 类型不受支持")
	}
	if err != nil {
		return nil, fail(CodeTrustUnavailable, "信任公钥无法解析")
	}
	switch key.Algorithm {
	case "ecdsa-p256":
		value, ok := public.(*ecdsa.PublicKey)
		if !ok || value.Curve != elliptic.P256() {
			return nil, fail(CodeTrustUnavailable, "信任公钥算法与 ecdsa-p256 不匹配")
		}
	case "ed25519":
		if _, ok := public.(ed25519.PublicKey); !ok {
			return nil, fail(CodeTrustUnavailable, "信任公钥算法与 ed25519 不匹配")
		}
	case "rsa-pkcs1":
		if _, ok := public.(*rsa.PublicKey); !ok {
			return nil, fail(CodeTrustUnavailable, "信任公钥算法与 rsa-pkcs1 不匹配")
		}
	default:
		return nil, fail(CodeTrustUnavailable, "信任公钥算法不受支持")
	}
	return public, nil
}

// Verify 校验 message 上的签名，digest 为 SHA-256(message)，
// 返回成功验证签名的可信密钥标识。
func (ring *KeyRing) Verify(message, digest, signature []byte) (string, error) {
	if ring == nil || len(ring.keys) == 0 {
		return "", fail(CodeTrustUnavailable, "信任密钥环为空")
	}
	for _, key := range ring.keys {
		switch key.algorithm {
		case "ecdsa-p256":
			public, ok := key.public.(*ecdsa.PublicKey)
			if ok && ecdsa.VerifyASN1(public, digest, signature) {
				return key.id, nil
			}
		case "ed25519":
			public, ok := key.public.(ed25519.PublicKey)
			if ok && ed25519.Verify(public, message, signature) {
				return key.id, nil
			}
		case "rsa-pkcs1":
			public, ok := key.public.(*rsa.PublicKey)
			if ok && rsa.VerifyPKCS1v15(public, crypto.SHA256, digest, signature) == nil {
				return key.id, nil
			}
		}
	}
	return "", fail(CodeSignatureInvalid, "没有受信密钥能验证该签名")
}

// ParseKeyRingFromPolicy 用于已加载策略、需要在测试中
// 解析其中某个密钥集合的调用方。生产代码应使用
// LoadTrustPolicy。
func (policy *TrustPolicy) ReleaseRing() (*KeyRing, error) {
	if policy == nil {
		return nil, fail(CodeTrustUnavailable, "信任策略为空")
	}
	return loadKeyRing(policy.ReleaseKeys)
}

func (policy *TrustPolicy) BuildRing() (*KeyRing, error) {
	if policy == nil {
		return nil, fail(CodeTrustUnavailable, "信任策略为空")
	}
	return loadKeyRing(policy.BuildKeys)
}

// CertificateOrKeyFingerprint 是诊断辅助函数，不负责
// 信任判定，返回 DER 公钥的 SHA-256 摘要。
func (ring *KeyRing) Fingerprint(keyID string) (string, bool) {
	if ring == nil {
		return "", false
	}
	for _, key := range ring.keys {
		if key.id != keyID {
			continue
		}
		raw, err := x509.MarshalPKIXPublicKey(key.public)
		if err != nil {
			return "", false
		}
		sum := sha256.Sum256(raw)
		return base64.StdEncoding.EncodeToString(sum[:]), true
	}
	return "", false
}
