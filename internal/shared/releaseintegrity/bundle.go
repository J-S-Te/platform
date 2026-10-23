package releaseintegrity

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"
)

// sigstoreBundle 是固定密钥离线策略使用的 Sigstore 包子集。
// 检查 verificationMaterial 只为
// 拒绝本策略不信任的材料，如透明日志条目、
// 证书链和时间戳；信任权威始终是 trust-policy.json。
type sigstoreBundle struct {
	MediaType            string                `json:"mediaType"`
	VerificationMaterial *verificationMaterial `json:"verificationMaterial"`
	MessageSignature     *messageSignature     `json:"messageSignature"`
	DSSEEnvelope         *dsseEnvelope         `json:"dsseEnvelope"`
}

type verificationMaterial struct {
	PublicKey                 json.RawMessage   `json:"publicKey"`
	TlogEntries               []json.RawMessage `json:"tlogEntries"`
	TimestampVerificationData json.RawMessage   `json:"timestampVerificationData"`
	X509CertificateChain      json.RawMessage   `json:"x509CertificateChain"`
}

type messageSignature struct {
	MessageDigest *bundleDigest `json:"messageDigest"`
	Signature     string        `json:"signature"`
}

type bundleDigest struct {
	Algorithm string `json:"algorithm"`
	Digest    string `json:"digest"`
}

type dsseEnvelope struct {
	PayloadType string          `json:"payloadType"`
	Payload     string          `json:"payload"`
	Signatures  []dsseSignature `json:"signatures"`
}

type dsseSignature struct {
	KeyID string `json:"keyid"`
	Sig   string `json:"sig"`
}

func parseBundle(raw []byte) (*sigstoreBundle, error) {
	if err := rejectDuplicateKeys(raw); err != nil {
		return nil, fail(CodeSignatureInvalid, "Sigstore bundle 不是严格 JSON")
	}
	var bundle sigstoreBundle
	if err := json.Unmarshal(raw, &bundle); err != nil {
		return nil, fail(CodeSignatureInvalid, "Sigstore bundle 无法解析")
	}
	if !strings.HasPrefix(bundle.MediaType, "application/vnd.dev.sigstore.bundle") {
		return nil, fail(CodeSignatureInvalid, "Sigstore bundle mediaType 不受支持")
	}
	if bundle.VerificationMaterial != nil {
		material := bundle.VerificationMaterial
		if len(material.TlogEntries) > 0 {
			return nil, fail(CodeTrustUnavailable, "vendor-key-offline-v1 不接受透明日志材料")
		}
		if len(bytes.TrimSpace(material.TimestampVerificationData)) > 0 {
			return nil, fail(CodeTrustUnavailable, "vendor-key-offline-v1 不接受时间戳材料")
		}
		if len(bytes.TrimSpace(material.X509CertificateChain)) > 0 {
			return nil, fail(CodeTrustUnavailable, "vendor-key-offline-v1 不接受证书链")
		}
	}
	if bundle.MessageSignature != nil && bundle.DSSEEnvelope != nil {
		return nil, fail(CodeSignatureInvalid, "Sigstore bundle 同时包含 blob 与 DSSE 签名")
	}
	return &bundle, nil
}

// VerifyBlobBundle 验证载荷上兼容 Cosign 的 blob 签名。
// 包中的摘要必须匹配载荷，且签名必须通过
// 可信发布密钥验证；不访问网络或透明日志。
func VerifyBlobBundle(bundleRaw, payload []byte, ring *KeyRing) (string, error) {
	bundle, err := parseBundle(bundleRaw)
	if err != nil {
		return "", err
	}
	if bundle.MessageSignature == nil || bundle.DSSEEnvelope != nil {
		return "", fail(CodeSignatureInvalid, "bundle 不是 blob 签名")
	}
	message := bundle.MessageSignature
	if message.MessageDigest == nil {
		return "", fail(CodeSignatureInvalid, "bundle 缺少消息摘要")
	}
	algorithm := strings.ToUpper(strings.ReplaceAll(strings.ReplaceAll(message.MessageDigest.Algorithm, "-", ""), "_", ""))
	if algorithm != "SHA2256" && algorithm != "SHA256" {
		return "", fail(CodeSignatureInvalid, "bundle 摘要算法不受支持")
	}
	expected, err := base64.StdEncoding.DecodeString(message.MessageDigest.Digest)
	if err != nil || len(expected) != sha256.Size {
		return "", fail(CodeSignatureInvalid, "bundle 消息摘要格式不正确")
	}
	actual := sha256.Sum256(payload)
	if !bytes.Equal(expected, actual[:]) {
		return "", fail(CodeSignatureInvalid, "签名载荷摘要与 bundle 不一致")
	}
	signature, err := base64.StdEncoding.DecodeString(message.Signature)
	if err != nil || len(signature) == 0 {
		return "", fail(CodeSignatureInvalid, "bundle 签名格式不正确")
	}
	keyID, err := ring.Verify(payload, actual[:], signature)
	if err != nil {
		return "", fail(CodeSignatureInvalid, "发布清单签名无效")
	}
	return keyID, nil
}

// VerifyDSSEBundle 验证与 Cosign 兼容的 DSSE 证明包，
// 要求签名载荷与可读取的 in-toto 声明原始字节完全相同；
// 验证通过后返回可信密钥标识及签名载荷。
func VerifyDSSEBundle(bundleRaw, statement []byte, ring *KeyRing) (string, error) {
	bundle, err := parseBundle(bundleRaw)
	if err != nil {
		return "", err
	}
	if bundle.DSSEEnvelope == nil || bundle.MessageSignature != nil {
		return "", fail(CodeProvenanceInvalid, "bundle 不是 DSSE 证明")
	}
	envelope := bundle.DSSEEnvelope
	if envelope.PayloadType != "application/vnd.in-toto+json" {
		return "", fail(CodeProvenanceInvalid, "DSSE payloadType 不是 in-toto Statement")
	}
	payload, err := base64.StdEncoding.DecodeString(envelope.Payload)
	if err != nil {
		return "", fail(CodeProvenanceInvalid, "DSSE payload 编码不正确")
	}
	if !bytes.Equal(payload, statement) {
		return "", fail(CodeProvenanceInvalid, "可读 Statement 与 DSSE payload 不一致")
	}
	if len(envelope.Signatures) == 0 {
		return "", fail(CodeProvenanceInvalid, "DSSE 缺少签名")
	}
	pae := dssePAE(envelope.PayloadType, payload)
	digest := sha256.Sum256(pae)
	for _, signature := range envelope.Signatures {
		raw, err := base64.StdEncoding.DecodeString(signature.Sig)
		if err != nil || len(raw) == 0 {
			continue
		}
		keyID, err := ring.Verify(pae, digest[:], raw)
		if err == nil {
			return keyID, nil
		}
	}
	return "", fail(CodeProvenanceInvalid, "DSSE 签名没有受信构建密钥")
}

func dssePAE(payloadType string, payload []byte) []byte {
	return []byte(fmt.Sprintf("DSSEv1 %d %s %d %s", len(payloadType), payloadType, len(payload), payload))
}

// Statement 是发布完整性策略使用的 in-toto Statement v1
// 严格子集。完整谓词保留为原始数据，以便未来验证 SLSA
// 字段时不会接受未经验证的数据。
type Statement struct {
	Type          string          `json:"_type"`
	PredicateType string          `json:"predicateType"`
	Subject       []Subject       `json:"subject"`
	Predicate     json.RawMessage `json:"predicate"`
}

type Subject struct {
	Name   string            `json:"name"`
	Digest map[string]string `json:"digest"`
}

func ParseStatement(raw []byte) (*Statement, error) {
	if err := rejectDuplicateKeys(raw); err != nil {
		return nil, fail(CodeProvenanceInvalid, "in-toto Statement 不是严格 JSON")
	}
	var statement Statement
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&statement); err != nil {
		return nil, fail(CodeProvenanceInvalid, "in-toto Statement 无法解析")
	}
	if statement.Type != "https://in-toto.io/Statement/v1" {
		return nil, fail(CodeProvenanceInvalid, "in-toto Statement _type 不支持")
	}
	if statement.PredicateType != "https://slsa.dev/provenance/v1" {
		return nil, fail(CodeProvenanceInvalid, "predicateType 不是 SLSA provenance v1")
	}
	if len(statement.Subject) == 0 {
		return nil, fail(CodeProvenanceInvalid, "in-toto Statement 没有 subject")
	}
	for _, subject := range statement.Subject {
		if subject.Name == "" || len(subject.Digest) == 0 {
			return nil, fail(CodeProvenanceInvalid, "in-toto subject 缺少名称或摘要")
		}
		for algorithm, digest := range subject.Digest {
			if algorithm != "sha256" || !sha256HexRe.MatchString(digest) {
				return nil, fail(CodeProvenanceInvalid, "in-toto subject 摘要算法/格式不支持")
			}
		}
	}
	return &statement, nil
}
