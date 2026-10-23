package releaseintegrity

import (
	"os"
	"testing"
)

// TestCosignBlobBundleInterop 是需显式启用的真实 Cosign CLI 门禁。将
// UIP_COSIGN_BUNDLE、UIP_COSIGN_BLOB 和 UIP_COSIGN_PUB 指向使用
// 禁用透明日志和 TSA 的签名配置生成的包；无需私钥，也不提交私钥。
func TestCosignBlobBundleInterop(t *testing.T) {
	bundlePath := os.Getenv("UIP_COSIGN_BUNDLE")
	blobPath := os.Getenv("UIP_COSIGN_BLOB")
	publicPath := os.Getenv("UIP_COSIGN_PUB")
	if bundlePath == "" || blobPath == "" || publicPath == "" {
		t.Skip("set UIP_COSIGN_BUNDLE/UIP_COSIGN_BLOB/UIP_COSIGN_PUB to run the real Cosign interop gate")
	}
	bundle, err := os.ReadFile(bundlePath)
	if err != nil {
		t.Fatal(err)
	}
	blob, err := os.ReadFile(blobPath)
	if err != nil {
		t.Fatal(err)
	}
	public, err := os.ReadFile(publicPath)
	if err != nil {
		t.Fatal(err)
	}
	ring, err := loadKeyRing([]TrustKey{{ID: "cosign-interop", Algorithm: "ecdsa-p256", PublicKeyPEM: string(public)}})
	if err != nil {
		t.Fatal(err)
	}
	keyID, err := VerifyBlobBundle(bundle, blob, ring)
	if err != nil {
		t.Fatalf("Cosign blob bundle did not verify: %v", err)
	}
	if keyID != "cosign-interop" {
		t.Fatalf("key id = %q", keyID)
	}
}

// TestCosignDSSEBundleInterop 验证真实 Cosign CLI 使用
// 禁用透明日志的签名配置生成的 DSSE 证明。将三个环境变量
// 分别设置为证明包、其准确解码的载荷和公钥。
func TestCosignDSSEBundleInterop(t *testing.T) {
	bundlePath := os.Getenv("UIP_COSIGN_DSSE_BUNDLE")
	statementPath := os.Getenv("UIP_COSIGN_DSSE_STATEMENT")
	publicPath := os.Getenv("UIP_COSIGN_PUB")
	if bundlePath == "" || statementPath == "" || publicPath == "" {
		t.Skip("set UIP_COSIGN_DSSE_BUNDLE/UIP_COSIGN_DSSE_STATEMENT/UIP_COSIGN_PUB to run the real Cosign DSSE gate")
	}
	bundle, err := os.ReadFile(bundlePath)
	if err != nil {
		t.Fatal(err)
	}
	statement, err := os.ReadFile(statementPath)
	if err != nil {
		t.Fatal(err)
	}
	public, err := os.ReadFile(publicPath)
	if err != nil {
		t.Fatal(err)
	}
	ring, err := loadKeyRing([]TrustKey{{ID: "cosign-interop", Algorithm: "ecdsa-p256", PublicKeyPEM: string(public)}})
	if err != nil {
		t.Fatal(err)
	}
	keyID, err := VerifyDSSEBundle(bundle, statement, ring)
	if err != nil {
		t.Fatalf("Cosign DSSE bundle did not verify: %v", err)
	}
	if keyID != "cosign-interop" {
		t.Fatalf("key id = %q", keyID)
	}
}
