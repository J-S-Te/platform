package application

import (
	"crypto/rand"
	"encoding/base64"
	"fmt"
)

const opaqueSecretBytes = 32

// CryptographicSecretGenerator 生成 256 位 URL 安全的不透明协议凭据。
type CryptographicSecretGenerator struct{}

// NewSecret 返回一个新的随机值，适用于短时效授权码或 refresh token。
func (CryptographicSecretGenerator) NewSecret() (string, error) {
	bytes := make([]byte, opaqueSecretBytes)
	if _, err := rand.Read(bytes); err != nil {
		return "", fmt.Errorf("read cryptographic randomness: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(bytes), nil
}
