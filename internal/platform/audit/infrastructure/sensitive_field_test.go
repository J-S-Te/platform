package infrastructure

import "testing"

// 存储层的 sensitiveField 必须覆盖 HTTP 层同源词表，并保留历史任意包含匹配，
// 否则平台内部适配器（不经 HTTP 摄取）会把凭据明文写进只追加审计存储。
func TestSensitiveFieldCoversSharedWordlistAndLegacySubstring(t *testing.T) {
	sensitive := []string{"private_key", "otp", "pin_code", "jwt_token", "bearer_token", "signing_key", "device_credential", "my_password_hash", "internal_token_cache"}
	for _, field := range sensitive {
		if !sensitiveField(field) {
			t.Errorf("sensitiveField(%q) = false, want true", field)
		}
	}
	for _, field := range []string{"signature_verified", "otp_required", "credential_generation", "actor_name"} {
		if sensitiveField(field) {
			t.Errorf("sensitiveField(%q) = true, want false", field)
		}
	}
}
