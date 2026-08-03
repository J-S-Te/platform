package security

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"strings"
)

// EnvelopeProtector 用 AES-256-GCM 保护落库前的小型敏感值，格式为 nonce || ciphertext。
// 密钥必须存放在数据库之外；不同信任域不应复用密钥，否则一个域的密钥泄露会扩大解密范围。
type EnvelopeProtector struct {
	key []byte
}

// NewEnvelopeProtector 只接受 Base64 编码的 32 字节密钥。空配置直接失败，禁止在加密配置缺失时
// 静默退化为明文持久化。
func NewEnvelopeProtector(encodedKey, environmentName string) (*EnvelopeProtector, error) {
	environmentName = strings.TrimSpace(environmentName)
	if environmentName == "" {
		environmentName = "encryption key"
	}
	if strings.TrimSpace(encodedKey) == "" {
		return nil, fmt.Errorf("%s must be configured", environmentName)
	}

	key, err := base64.StdEncoding.DecodeString(strings.TrimSpace(encodedKey))
	if err != nil {
		return nil, fmt.Errorf("decode %s: %w", environmentName, err)
	}
	if len(key) != 32 {
		return nil, fmt.Errorf("%s must decode to 32 bytes", environmentName)
	}
	return &EnvelopeProtector{key: key}, nil
}

// Encrypt 每次使用新的随机 nonce；返回值把 nonce 置于密文前供解密拆分。context 仅用于满足
// 应用层接口，单次本地密码运算本身不可取消。
func (protector *EnvelopeProtector) Encrypt(_ context.Context, plaintext []byte) ([]byte, error) {
	gcm, err := protector.gcm()
	if err != nil {
		return nil, err
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return nil, fmt.Errorf("generate encryption nonce: %w", err)
	}
	return gcm.Seal(nonce, nonce, plaintext, nil), nil
}

// Decrypt 在返回明文前先验证 GCM 标签，密文截断、篡改或使用错误密钥都会失败关闭。
func (protector *EnvelopeProtector) Decrypt(_ context.Context, ciphertext []byte) ([]byte, error) {
	gcm, err := protector.gcm()
	if err != nil {
		return nil, err
	}
	if len(ciphertext) < gcm.NonceSize() {
		return nil, errors.New("encrypted value is malformed")
	}
	plaintext, err := gcm.Open(nil, ciphertext[:gcm.NonceSize()], ciphertext[gcm.NonceSize():], nil)
	if err != nil {
		return nil, fmt.Errorf("decrypt encrypted value: %w", err)
	}
	return plaintext, nil
}

func (protector *EnvelopeProtector) gcm() (cipher.AEAD, error) {
	if protector == nil || len(protector.key) != 32 {
		return nil, errors.New("envelope encryption is not configured")
	}
	block, err := aes.NewCipher(protector.key)
	if err != nil {
		return nil, fmt.Errorf("create AES cipher: %w", err)
	}
	return cipher.NewGCM(block)
}
