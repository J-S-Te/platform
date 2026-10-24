package security

import (
	"crypto/ed25519"
	"crypto/rand"
	"testing"
	"time"

	runtime "github.com/J-S-Te/license-core/runtime"
)

func TestLicenseSnapshotPurposeAndReadOnlySigner(t *testing.T) {
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	vendor, _, _ := ed25519.GenerateKey(rand.Reader)
	manager := &ApplicationJWTManager{issuer: "platform", audience: "machines", privateKey: priv, publicKey: pub}
	snapshot := runtime.PlatformSnapshot{Protocol: 1, InstanceID: "instance", Environment: "test", Application: "contract_management", ServiceID: "api", Revision: 1, EnforcementState: runtime.Pending, SnapshotIssuedAt: time.Now().Unix()}
	raw, err := manager.Sign(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	keys := map[string]ed25519.PublicKey{"vendor": vendor}
	parsed, err := manager.VerifyLicenseSnapshot(raw, snapshot.Binding(), keys)
	if err != nil || parsed.Revision != 1 {
		t.Fatal(err)
	}
	if _, err = manager.Verify(raw, time.Now()); err == nil {
		t.Fatal("runtime snapshot became machine token")
	}
	if manager.LicenseSnapshotKeyID() == "" {
		t.Fatal("key identity absent")
	}
	readOnly := &ApplicationJWTManager{publicKey: pub}
	if _, err = readOnly.Sign(snapshot); err == nil {
		t.Fatal("verifier signed")
	}
	if _, err = readOnly.VerifyLicenseSnapshot(raw, snapshot.Binding(), keys); err != nil {
		t.Fatal(err)
	}
}
