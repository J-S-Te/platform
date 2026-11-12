package http

import "testing"

// TestDeriveGatewayFromRedirectURI 验证仅重建时从登记回调地址反推网关公开配置。
// 只接受规范 scheme://host/prefix/auth/callback；其余形态必须拒绝，保持"不下发浏览器 URL"语义。
// 参数：t为测试上下文。
// 返回值：推导值不符或该拒绝时放行即失败。
func TestDeriveGatewayFromRedirectURI(t *testing.T) {
	for _, tc := range []struct {
		name       string
		raw        string
		wantBase   string
		wantPrefix string
		wantOK     bool
	}{
		{"typical", "http://localhost:8081/reimbursement/auth/callback", "http://localhost:8081", "/reimbursement", true},
		{"nested_prefix", "https://example.com/a/b/auth/callback", "https://example.com", "/a/b", true},
		{"missing_callback", "http://localhost:8081/reimbursement", "", "", false},
		{"host_only", "http://localhost:8081/auth/callback", "", "", false},
		{"relative", "/reimbursement/auth/callback", "", "", false},
		{"blank", "", "", "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			base, prefix, ok := deriveGatewayFromRedirectURI(tc.raw)
			if ok != tc.wantOK {
				t.Fatalf("ok=%v; want %v (raw=%q)", ok, tc.wantOK, tc.raw)
			}
			if base != tc.wantBase || prefix != tc.wantPrefix {
				t.Fatalf("base=%q prefix=%q; want %q %q", base, prefix, tc.wantBase, tc.wantPrefix)
			}
		})
	}
}
