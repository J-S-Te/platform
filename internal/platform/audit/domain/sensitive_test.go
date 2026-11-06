package domain

import "testing"

// 敏感字段词表是 HTTP 摄取边界与只追加存储层的共同来源；这里锁定精确词、后缀、
// 前缀与“不要把业务标志误判为凭据”的边界。
func TestIsSensitiveAuditField(t *testing.T) {
	sensitive := []string{
		"password", "Password", " password ", "passwd", "token", "secret", "authorization",
		"cookie", "set-cookie", "dsn", "private_key", "private-key", "api_key", "apikey",
		"session_key", "client_secret", "access_token", "refresh_token", "id_token",
		"credential", "credentials", "otp", "one_time_password", "otp_code", "otp_secret",
		"pin", "pin_code", "jwt", "jwt_assertion", "jwt_token", "bearer", "bearer_token",
		"signature", "signing_key",
		// 后缀与前缀形态
		"user_password", "legacy_secret", "some_otp", "device_pin", "id_jwt", "request_signature",
		"password_hash", "token_ciphertext", "secret_ref", "password_policy_name", "token_count",
	}
	for _, field := range sensitive {
		if !IsSensitiveAuditField(field) {
			t.Errorf("IsSensitiveAuditField(%q) = false, want true", field)
		}
	}

	// 业务标志不能因为包含相近词根就被误打码。
	benign := []string{
		"signature_verified", "otp_required", "credential_generation", "pin_policy",
		"authorization_revision", "otel_trace_id",
		"actor_name", "resource_name", "request_id",
	}
	for _, field := range benign {
		if IsSensitiveAuditField(field) {
			t.Errorf("IsSensitiveAuditField(%q) = true, want false", field)
		}
	}
}
