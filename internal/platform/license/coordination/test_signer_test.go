package coordination

import (
	"crypto/ed25519"

	runtime "github.com/J-S-Te/license-core/runtime"
)

// testSigner 是协调层测试共用的平台快照签名器；V1 审批测试移除后，
// 它由多个测试文件共享，集中放在这里避免重复定义。
type testSigner struct {
	priv   ed25519.PrivateKey
	pub    ed25519.PublicKey
	vendor map[string]ed25519.PublicKey
}

func (s testSigner) Sign(p runtime.PlatformSnapshot) (string, error) {
	return runtime.SignSnapshot(p, "test-platform", s.priv)
}

func (s testSigner) Verify(raw string, b runtime.Binding) (runtime.PlatformSnapshot, error) {
	return runtime.VerifySnapshot(raw, b, map[string]ed25519.PublicKey{"test-platform": s.pub}, s.vendor)
}
