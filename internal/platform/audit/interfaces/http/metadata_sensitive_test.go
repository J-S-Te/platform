package http

import (
	"errors"
	"strings"
	"testing"

	"github.com/J-S-Te/Basic-Platform/internal/platform/audit/application"
)

// 统一审计方案 §3.4：发布端先过滤，平台摄取再防御一次。
// 含凭据类键的载荷必须整条拒绝，不能"静默截断后照单全收"。
func TestCopyMetadataRejectsCredentialFields(t *testing.T) {
	for _, key := range []string{"password", "Password_Hash", "access_token", "client_secret", "Authorization", "cookie", "db_dsn", "private_key", "x-api-key", "refresh_token", "session-secret"} {
		if _, err := copyMetadata(map[string]any{key: "leaked"}); err == nil {
			t.Fatalf("metadata %q 未被拒绝", key)
		} else if !errors.Is(err, application.ErrValidation) {
			t.Fatalf("metadata %q 错误类型不对：%v", key, err)
		}
	}
}

func TestCopyMetadataKeepsBusinessFields(t *testing.T) {
	// authorization_revision 是授权版本号，含 "authorization" 但不是凭据，必须放行。
	metadata, err := copyMetadata(map[string]any{"method": "POST", "path": "/api/v1/settlement/invoices", "amount_minor": 100, "authorization_revision": "rev-7", "content_hash": "abc"})
	if err != nil {
		t.Fatalf("普通业务字段不应被拒绝：%v", err)
	}
	if metadata["amount_minor"] != 100 || metadata["method"] != "POST" || metadata["authorization_revision"] != "rev-7" {
		t.Fatalf("业务字段丢失：%+v", metadata)
	}
}

func TestCopyMetadataRejectsNestedCredential(t *testing.T) {
	_, err := copyMetadata(map[string]any{"request": map[string]any{"headers": map[string]any{"Authorization": "Bearer x"}}})
	if err == nil {
		t.Fatal("嵌套凭据字段必须被拒绝")
	}
	if !strings.Contains(err.Error(), "sensitive field") {
		t.Fatalf("错误信息应指出敏感字段：%v", err)
	}
}
