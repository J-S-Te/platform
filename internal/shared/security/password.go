// Package security 提供平台模块共享的底层安全原语。
package security

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"

	"golang.org/x/crypto/argon2"
)

const argon2idAlgorithm = "argon2id"

// Argon2idPasswordHasher 为受控开户和重置流程生成新凭据，返回原始摘要以及
// iam_password_credential 所需的参数元数据；盐不会与摘要混成不可升级的私有格式。
type Argon2idPasswordHasher struct{}

// Hash 使用密码学安全的随机 16 字节盐派生密码摘要。
func (Argon2idPasswordHasher) Hash(password string) ([]byte, []byte, error) {
	salt := make([]byte, 16)
	if _, err := rand.Read(salt); err != nil {
		return nil, nil, fmt.Errorf("generate Argon2id salt: %w", err)
	}
	return HashPassword(password, DefaultArgon2idParams(salt))
}

// Argon2idParams records the exact parameters needed to verify a password credential. The
// password digest is stored separately in iam_password_credential.password_hash.
type Argon2idParams struct {
	Version     int    `json:"version"`
	MemoryKiB   uint32 `json:"memory_kib"`
	Iterations  uint32 `json:"iterations"`
	Parallelism uint8  `json:"parallelism"`
	SaltBase64  string `json:"salt_base64"`
	KeyLength   uint32 `json:"key_length"`
}

// DefaultArgon2idParams 是新凭据的基线成本。验证始终读取每条凭据保存的参数，
// 因此提高默认成本不会让历史密码立即失效，可在后续成功登录时渐进升级。
func DefaultArgon2idParams(salt []byte) Argon2idParams {
	return Argon2idParams{
		Version:     argon2.Version,
		MemoryKiB:   64 * 1024,
		Iterations:  3,
		Parallelism: 2,
		SaltBase64:  base64.RawStdEncoding.EncodeToString(salt),
		KeyLength:   32,
	}
}

// HashPassword 生成原始 Argon2id 摘要并序列化验证参数。输入应来自已完成强度与长度校验的
// 应用流程，本原语不承担 HTTP 请求策略校验。
func HashPassword(password string, params Argon2idParams) ([]byte, []byte, error) {
	if err := validateArgon2idParams(params); err != nil {
		return nil, nil, err
	}
	salt, err := base64.RawStdEncoding.DecodeString(params.SaltBase64)
	if err != nil {
		return nil, nil, fmt.Errorf("decode Argon2id salt: %w", err)
	}
	digest := argon2.IDKey([]byte(password), salt, params.Iterations, params.MemoryKiB, params.Parallelism, params.KeyLength)
	metadata, err := json.Marshal(params)
	if err != nil {
		return nil, nil, fmt.Errorf("marshal Argon2id parameters: %w", err)
	}
	return digest, metadata, nil
}

// VerifyPassword 使用持久化参数重算摘要，并用恒定时间比较结果。格式或成本参数非法属于凭据错误，
// 与格式正确但密码不匹配的 false 结果分开返回。
func VerifyPassword(password string, algorithm string, digest, metadata []byte) (bool, error) {
	if !strings.EqualFold(algorithm, argon2idAlgorithm) {
		return false, fmt.Errorf("unsupported password hash algorithm %q", algorithm)
	}

	var params Argon2idParams
	if err := json.Unmarshal(metadata, &params); err != nil {
		return false, fmt.Errorf("decode Argon2id parameters: %w", err)
	}
	if err := validateArgon2idParams(params); err != nil {
		return false, err
	}

	salt, err := base64.RawStdEncoding.DecodeString(params.SaltBase64)
	if err != nil {
		return false, fmt.Errorf("decode Argon2id salt: %w", err)
	}
	candidate := argon2.IDKey([]byte(password), salt, params.Iterations, params.MemoryKiB, params.Parallelism, params.KeyLength)
	return subtle.ConstantTimeCompare(candidate, digest) == 1, nil
}

func validateArgon2idParams(params Argon2idParams) error {
	if params.Version != argon2.Version {
		return fmt.Errorf("unsupported Argon2id version %d", params.Version)
	}
	if params.MemoryKiB < 8*uint32(params.Parallelism) || params.MemoryKiB > 1024*1024 {
		return fmt.Errorf("Argon2id memory_kib is outside the supported range")
	}
	if params.Iterations == 0 || params.Iterations > 10 {
		return fmt.Errorf("Argon2id iterations is outside the supported range")
	}
	if params.Parallelism == 0 || params.Parallelism > 8 {
		return fmt.Errorf("Argon2id parallelism is outside the supported range")
	}
	if params.KeyLength < 16 || params.KeyLength > 64 {
		return fmt.Errorf("Argon2id key_length is outside the supported range")
	}
	salt, err := base64.RawStdEncoding.DecodeString(params.SaltBase64)
	if err != nil || len(salt) < 16 || len(salt) > 64 {
		return fmt.Errorf("Argon2id salt must be base64 encoded and 16 to 64 bytes")
	}
	return nil
}

// Argon2idPasswordVerifier adapts VerifyPassword to the identity application dependency.
type Argon2idPasswordVerifier struct{}

// Verify checks a stored Argon2id credential.
func (Argon2idPasswordVerifier) Verify(password, algorithm string, digest, metadata []byte) (bool, error) {
	return VerifyPassword(password, algorithm, digest, metadata)
}
