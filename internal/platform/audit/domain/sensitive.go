package domain

import "strings"

// IsSensitiveAuditField 判断审计载荷里的字段名是否可能承载凭据材料。
//
// 摄取边界（HTTP change_summary / metadata 打码）与存储层（changes/metadata 落库前打码）
// 共用同一份词表：平台内部适配器不一定经过 HTTP 摄取边界，若两份词表漂移，
// private_key/otp/pin/jwt/signature 这类字段仍可能以明文进入只追加审计存储。
//
// 匹配规则有意收敛：精确词、已知后缀与少量前缀；不做宽泛前缀匹配，避免把
// signature_verified、otp_required、credential_generation、pin_policy 这类业务标志误判。
func IsSensitiveAuditField(field string) bool {
	normalized := strings.ToLower(strings.TrimSpace(field))
	normalized = strings.NewReplacer("-", "_", ".", "_").Replace(normalized)
	switch normalized {
	case "password", "passwd", "token", "secret", "authorization", "cookie", "set_cookie",
		"dsn", "private_key", "api_key", "apikey", "session_key", "client_secret",
		"access_token", "refresh_token", "id_token",
		"credential", "credentials", "otp", "one_time_password", "otp_code", "otp_secret",
		"pin", "pin_code", "jwt", "jwt_assertion", "jwt_token", "bearer", "bearer_token",
		"signature", "signing_key":
		return true
	}
	for _, suffix := range []string{"_password", "_passwd", "_token", "_secret", "_api_key", "_apikey", "_private_key", "_session_key", "_dsn", "_credential", "_credentials", "_otp", "_pin", "_jwt", "_bearer", "_signature", "_signing_key"} {
		if strings.HasSuffix(normalized, suffix) {
			return true
		}
	}
	for _, prefix := range []string{"password_", "passwd_", "token_", "secret_"} {
		if strings.HasPrefix(normalized, prefix) {
			return true
		}
	}
	return false
}
