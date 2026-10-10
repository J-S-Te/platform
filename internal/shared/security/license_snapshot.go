package security

import (
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
	"errors"

	runtime "github.com/J-S-Te/license-core/runtime"
)

// Sign keeps the platform signing key inside the existing key owner. Purpose
// separation is enforced by runtime's exact JWS typ, never by the HTTP caller.
func (manager *ApplicationJWTManager) Sign(snapshot runtime.PlatformSnapshot) (string, error) {
	if manager == nil || len(manager.privateKey) != ed25519.PrivateKeySize {
		return "", errors.New("platform snapshot signing key unavailable")
	}
	return runtime.SignSnapshot(snapshot, manager.LicenseSnapshotKeyID(), manager.privateKey)
}

// LicenseSnapshotKeyID identifies the deployment's platform public key, not
// the vendor's independently trusted commercial signing key.
func (manager *ApplicationJWTManager) LicenseSnapshotKeyID() string {
	if manager == nil || len(manager.publicKey) != ed25519.PublicKeySize {
		return ""
	}
	hash := sha256.Sum256(manager.publicKey)
	return "platform-" + hex.EncodeToString(hash[:16])
}

// VerifyLicenseSnapshot uses the platform's own configured public key and a
// separately supplied vendor trust set; neither key set comes from the JWS.
func (manager *ApplicationJWTManager) VerifyLicenseSnapshot(raw string, binding runtime.Binding, vendorKeys map[string]ed25519.PublicKey) (runtime.PlatformSnapshot, error) {
	if manager == nil || len(manager.publicKey) != ed25519.PublicKeySize {
		return runtime.PlatformSnapshot{}, errors.New("platform snapshot verification key unavailable")
	}
	return runtime.VerifySnapshot(raw, binding, map[string]ed25519.PublicKey{manager.LicenseSnapshotKeyID(): manager.publicKey}, vendorKeys)
}
