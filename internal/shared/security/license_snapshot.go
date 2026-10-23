package security

import (
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
	"errors"

	runtime "github.com/J-S-Te/license-core/runtime"
)

// Sign 令平台签名密钥始终留在既有密钥持有者内部；用途隔离由 runtime 校验精确的
// JWS typ 实现，绝不信任 HTTP 调用方声明。
func (manager *ApplicationJWTManager) Sign(snapshot runtime.PlatformSnapshot) (string, error) {
	if manager == nil || len(manager.privateKey) != ed25519.PrivateKeySize {
		return "", errors.New("platform snapshot signing key unavailable")
	}
	return runtime.SignSnapshot(snapshot, manager.LicenseSnapshotKeyID(), manager.privateKey)
}

// LicenseSnapshotKeyID 标识部署自身的平台公钥，而非供应商独立信任的
// 商业签名密钥，两类密钥空间不得混用。
func (manager *ApplicationJWTManager) LicenseSnapshotKeyID() string {
	if manager == nil || len(manager.publicKey) != ed25519.PublicKeySize {
		return ""
	}
	hash := sha256.Sum256(manager.publicKey)
	return "platform-" + hex.EncodeToString(hash[:16])
}

// VerifyLicenseSnapshot 使用平台自身配置的公钥与单独传入的供应商信任集校验快照；
// 两套密钥都不取自 JWS 载荷本身。
func (manager *ApplicationJWTManager) VerifyLicenseSnapshot(raw string, binding runtime.Binding, vendorKeys map[string]ed25519.PublicKey) (runtime.PlatformSnapshot, error) {
	if manager == nil || len(manager.publicKey) != ed25519.PublicKeySize {
		return runtime.PlatformSnapshot{}, errors.New("platform snapshot verification key unavailable")
	}
	return runtime.VerifySnapshot(raw, binding, map[string]ed25519.PublicKey{manager.LicenseSnapshotKeyID(): manager.publicKey}, vendorKeys)
}
