package infrastructure

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
)

// 审计事件 payload_hash 的算法标签（持久化在 audit_event.payload_hash_alg）。
//
// 安全理由（审计 P0 · 防篡改）：旧实现对规范输入计算无键 SHA-256，任何能写库的人都能
// 改写事件并重算哈希，摘要不构成防篡改证据。现在在配置了平台侧密钥时改为 HMAC-SHA256
// （Vault 审计设备同一模式）：没有密钥就无法伪造与存储摘要一致的篡改内容，
// 且可通过 /api/v1/audit/events/{event_id}/hash-check 以同一密钥重算比对。
// 历史行（迁移前）保留 SHA-256 标签，验证时按标签选择算法，不混用。
const (
	PayloadHashAlgorithmSHA256     = "SHA-256"
	PayloadHashAlgorithmHMACSHA256 = "HMAC-SHA256"

	// PayloadHashKeyEnv 是平台侧 HMAC 密钥的环境变量：标准 base64（允许无填充），解码后至少 32 字节。
	PayloadHashKeyEnv      = "PLATFORM_AUDIT_HASH_KEY"
	minPayloadHashKeyBytes = 32
)

// ErrPayloadHashKeyRequired 在审计必开模式下缺少 HMAC 密钥时由构造函数返回：
// 必开模式要求审计摘要具备防篡改能力，不能静默退回无键 SHA-256。
var ErrPayloadHashKeyRequired = errors.New("audit payload hash key is required when audit is mandatory (" + PayloadHashKeyEnv + ")")

// ErrPayloadHashKeyUnavailable 表示存储行使用 HMAC 摘要，但当前进程没有密钥可供重算。
var ErrPayloadHashKeyUnavailable = errors.New("audit payload hash key is not configured; cannot verify HMAC digest")

// payloadHasher 计算与验证审计事件 payload 摘要。key 为空时退回无键 SHA-256（仅非必开模式允许）。
type payloadHasher struct{ key []byte }

// Algorithm 返回当前写入新事件所用的算法标签。
func (h payloadHasher) Algorithm() string {
	if len(h.key) > 0 {
		return PayloadHashAlgorithmHMACSHA256
	}
	return PayloadHashAlgorithmSHA256
}

// Sum 以当前算法计算摘要（32 字节）。
func (h payloadHasher) Sum(payload []byte) []byte {
	if len(h.key) > 0 {
		mac := hmac.New(sha256.New, h.key)
		mac.Write(payload)
		return mac.Sum(nil)
	}
	digest := sha256.Sum256(payload)
	return digest[:]
}

// SumChain 计算哈希链摘要：H(prev_chain_hash || payload_hash)。
// 链首以全零 prev 起链；密钥语义与 Sum 一致（配置密钥时为 HMAC）。
func (h payloadHasher) SumChain(prevChainHash, payloadHash []byte) []byte {
	return h.Sum(append(append([]byte{}, prevChainHash...), payloadHash...))
}

// Verify 按存储行的算法标签重算候选 payload 的摘要并做常量时间比对。
// HMAC 行在无密钥时返回 ErrPayloadHashKeyUnavailable，而不是报“不匹配”误导取证。
func (h payloadHasher) Verify(algorithm string, payload, stored []byte) (computed []byte, matched bool, err error) {
	switch strings.ToUpper(strings.TrimSpace(algorithm)) {
	case PayloadHashAlgorithmHMACSHA256:
		if len(h.key) == 0 {
			return nil, false, ErrPayloadHashKeyUnavailable
		}
		mac := hmac.New(sha256.New, h.key)
		mac.Write(payload)
		computed = mac.Sum(nil)
	case PayloadHashAlgorithmSHA256, "":
		digest := sha256.Sum256(payload)
		computed = digest[:]
	default:
		return nil, false, fmt.Errorf("unsupported audit payload hash algorithm %q", algorithm)
	}
	return computed, hmac.Equal(computed, stored), nil
}

// parsePayloadHashKey 解析 base64 密钥；空值表示未配置。
func parsePayloadHashKey(raw string) ([]byte, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, nil
	}
	key, err := base64.StdEncoding.DecodeString(raw)
	if err != nil {
		key, err = base64.RawStdEncoding.DecodeString(raw)
		if err != nil {
			return nil, fmt.Errorf("%s must be standard base64: %w", PayloadHashKeyEnv, err)
		}
	}
	if len(key) < minPayloadHashKeyBytes {
		return nil, fmt.Errorf("%s must decode to at least %d bytes, got %d", PayloadHashKeyEnv, minPayloadHashKeyBytes, len(key))
	}
	return key, nil
}

// auditRequiredFromEnv 与传输层/子系统的必开判定保持同一语义：PLATFORM_AUDIT_REQUIRED 显式优先，
// 否则 APP_ENV=production 默认必开。
func auditRequiredFromEnv() bool {
	raw := strings.TrimSpace(os.Getenv("PLATFORM_AUDIT_REQUIRED"))
	if raw != "" {
		required, err := strconv.ParseBool(raw)
		return err == nil && required
	}
	return strings.EqualFold(strings.TrimSpace(os.Getenv("APP_ENV")), "production")
}

// payloadHasherFromEnv 读取环境中的密钥；必开模式缺密钥时失败关闭。
func payloadHasherFromEnv() (payloadHasher, error) {
	key, err := parsePayloadHashKey(os.Getenv(PayloadHashKeyEnv))
	if err != nil {
		return payloadHasher{}, err
	}
	if len(key) == 0 && auditRequiredFromEnv() {
		return payloadHasher{}, ErrPayloadHashKeyRequired
	}
	return payloadHasher{key: key}, nil
}
