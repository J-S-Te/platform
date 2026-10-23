// Package trust 保存编译进经过评审的客户发布物中的厂商公钥。
// 导入的许可证和可变的运行时设置都无权新增受信密钥。
package trust

import (
	"crypto/ed25519"
	"crypto/x509"
	"encoding/pem"
)

// 厂商公钥，由授权所有方提供。kid 与签发工具默认值一致。
// SPKI SHA256: fcb4c05eea187300eb2c27c23fc1cab2b446a266038fda3c997fbe4d2ba365bd.
const vendorPublicKey = `-----BEGIN PUBLIC KEY-----
MCowBQYDK2VwAyEAaKc7dKpa6IPeM3+dRf9fai2jD8LvwKssmm68Up++un8=
-----END PUBLIC KEY-----`

// Keys 返回防御性副本。厂商公钥必须通过代码评审变更，绝不通过可变的运行时配置调整。
// 不信任任何测试签名密钥。
func Keys() map[string]ed25519.PublicKey {
	block, _ := pem.Decode([]byte(vendorPublicKey))
	if block == nil {
		panic("invalid compiled vendor public key")
	}
	parsed, err := x509.ParsePKIXPublicKey(block.Bytes)
	if err != nil {
		panic("invalid compiled vendor public key")
	}
	key, ok := parsed.(ed25519.PublicKey)
	if !ok {
		panic("compiled vendor key is not Ed25519")
	}
	return map[string]ed25519.PublicKey{"vendor-v1": append(ed25519.PublicKey(nil), key...)}
}
