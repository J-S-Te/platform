package http

import (
	"errors"
	"strings"
	"testing"

	"github.com/J-S-Te/Basic-Platform/internal/platform/audit/application"
	"github.com/J-S-Te/Basic-Platform/internal/platform/audit/domain"
)

// 统一审计方案 §3.4：change_summary 是合法审计内容，不能因为出现敏感字段就整条拒绝
// （否则密码重置类事件无法留痕），但 before/after 必须打码且保留 field 便于追溯。
func TestSanitizeChangeSummaryMasksSensitiveField(t *testing.T) {
	for _, field := range []string{"password", "Password", "new_password", "otp", "otp_code", "pin", "jwt", "bearer_token", "signature", "private_key", "client_secret", "credential"} {
		sanitized, err := sanitizeChangeSummary([]domain.FieldChange{{Field: field, Before: "plain-text-secret", After: "next-plain-text-secret"}})
		if err != nil {
			t.Fatalf("field %q 打码失败：%v", field, err)
		}
		if len(sanitized) != 1 {
			t.Fatalf("field %q 打码后条数 = %d, want 1", field, len(sanitized))
		}
		if sanitized[0].Field != field {
			t.Fatalf("field 名称必须保留便于审计：got %q, want %q", sanitized[0].Field, field)
		}
		if sanitized[0].Before != sensitiveRedaction || sanitized[0].After != sensitiveRedaction {
			t.Fatalf("field %q before/after = %v/%v, want %q/%q", field, sanitized[0].Before, sanitized[0].After, sensitiveRedaction, sensitiveRedaction)
		}
	}
}

// 普通业务字段的变更值必须原样保留，打码不能误伤审计事实。
func TestSanitizeChangeSummaryKeepsBusinessFields(t *testing.T) {
	sanitized, err := sanitizeChangeSummary([]domain.FieldChange{
		{Field: "display_name", Before: "张三", After: "李四"},
		{Field: "status", Before: "ACTIVE", After: "DISABLED"},
	})
	if err != nil {
		t.Fatalf("普通字段不应被拒绝：%v", err)
	}
	if sanitized[0].Field != "display_name" || sanitized[0].Before != "张三" || sanitized[0].After != "李四" {
		t.Fatalf("业务字段被打码或丢失：%+v", sanitized[0])
	}
	if sanitized[1].Before != "ACTIVE" || sanitized[1].After != "DISABLED" {
		t.Fatalf("业务字段被打码：%+v", sanitized[1])
	}
}

// 嵌套对象/数组中的敏感键同样必须打码：只有 field 名检查会被
// {"field":"user_payload","after":{"password":"x"}} 绕过。
func TestSanitizeChangeSummaryMasksNestedSensitiveValues(t *testing.T) {
	sanitized, err := sanitizeChangeSummary([]domain.FieldChange{
		{Field: "user_payload", Before: map[string]any{"password": "a", "amount_minor": 100}, After: []any{map[string]any{"token": "b", "role": "ADMIN"}}},
	})
	if err != nil {
		t.Fatalf("嵌套值打码失败：%v", err)
	}
	before, ok := sanitized[0].Before.(map[string]any)
	if !ok || before["password"] != sensitiveRedaction || before["amount_minor"] != 100 {
		t.Fatalf("嵌套 before 打码结果 = %#v", sanitized[0].Before)
	}
	after, ok := sanitized[0].After.([]any)
	if !ok || len(after) != 1 {
		t.Fatalf("嵌套 after 打码结果 = %#v", sanitized[0].After)
	}
	item, ok := after[0].(map[string]any)
	if !ok || item["token"] != sensitiveRedaction || item["role"] != "ADMIN" {
		t.Fatalf("数组内 after 打码结果 = %#v", after[0])
	}
}

// 条数超限必须整条拒绝（HTTP 422），不得静默截断成前 N 条。
func TestSanitizeChangeSummaryRejectsTooManyEntries(t *testing.T) {
	changes := make([]domain.FieldChange, application.MaxChangeSummaryEntries+1)
	for index := range changes {
		changes[index] = domain.FieldChange{Field: "status", Before: "A", After: "B"}
	}
	_, err := sanitizeChangeSummary(changes)
	if !errors.Is(err, application.ErrValidation) {
		t.Fatalf("超条数 err = %v, want ErrValidation", err)
	}
	if !strings.Contains(err.Error(), "change_summary") {
		t.Fatalf("错误信息应指出 change_summary：%v", err)
	}
	// 恰好等于上限必须放行，避免把常量理解成"上限-1"。
	if _, err := sanitizeChangeSummary(changes[:application.MaxChangeSummaryEntries]); err != nil {
		t.Fatalf("等于上限的条数应放行：%v", err)
	}
}

// 单值长度超限必须整条拒绝，不得静默截断。
func TestSanitizeChangeSummaryRejectsOversizedValue(t *testing.T) {
	oversized := strings.Repeat("a", application.MaxChangeSummaryValueRunes+1)
	for name, change := range map[string]domain.FieldChange{
		"before": {Field: "display_name", Before: oversized},
		"after":  {Field: "display_name", After: oversized},
	} {
		if _, err := sanitizeChangeSummary([]domain.FieldChange{change}); !errors.Is(err, application.ErrValidation) {
			t.Fatalf("%s 超长 err = %v, want ErrValidation", name, err)
		}
	}
	// 嵌套对象按序列化长度计，同样不能绕过。
	nested := domain.FieldChange{Field: "user_payload", Before: map[string]any{"note": oversized}}
	if _, err := sanitizeChangeSummary([]domain.FieldChange{nested}); !errors.Is(err, application.ErrValidation) {
		t.Fatalf("嵌套超长 err = %v, want ErrValidation", err)
	}
}

// 敏感字段的超长原文先被打码，不应反过来拒绝整条事件（也避免"值很大"成为信号）。
func TestSanitizeChangeSummaryMasksOversizedSensitiveValue(t *testing.T) {
	sanitized, err := sanitizeChangeSummary([]domain.FieldChange{
		{Field: "password", Before: strings.Repeat("p", application.MaxChangeSummaryValueRunes+1)},
	})
	if err != nil {
		t.Fatalf("敏感超长值应打码而非拒绝：%v", err)
	}
	if sanitized[0].Before != sensitiveRedaction {
		t.Fatalf("before = %v, want %q", sanitized[0].Before, sensitiveRedaction)
	}
}

// 修复前的漏洞：metadata 只递归 map，数组内对象会整体绕过敏感键检查。
func TestCopyMetadataRejectsSensitiveFieldsInsideArrays(t *testing.T) {
	cases := map[string]map[string]any{
		"array of objects": {"items": []any{map[string]any{"password": "x"}}},
		"nested arrays":    {"groups": []any{[]any{map[string]any{"token": "x"}}}},
		"array under map":  {"payload": map[string]any{"headers": []any{map[string]any{"Authorization": "Bearer x"}}}},
	}
	for name, metadata := range cases {
		if _, err := copyMetadata(metadata); err == nil {
			t.Fatalf("%s：数组内敏感键必须被拒绝", name)
		} else if !errors.Is(err, application.ErrValidation) {
			t.Fatalf("%s：错误类型不对：%v", name, err)
		}
	}
}

// 数组内的普通业务值必须放行：数组递归不能变成"含数组就拒绝"。
func TestCopyMetadataKeepsBusinessValuesInsideArrays(t *testing.T) {
	metadata, err := copyMetadata(map[string]any{"items": []any{map[string]any{"sku": "A-1", "quantity": 2}}})
	if err != nil {
		t.Fatalf("数组内业务字段不应被拒绝：%v", err)
	}
	items, ok := metadata["items"].([]any)
	if !ok || len(items) != 1 {
		t.Fatalf("数组被改写：%#v", metadata["items"])
	}
}

// 词表不能过度扩张：signature_verified / otp_required 这类业务标志不是凭据，
// 误判会让合法字段被打码，反而损害审计可读性。
func TestIsSensitiveMetadataKeyKeepsBusinessFlags(t *testing.T) {
	for _, key := range []string{"signature_verified", "otp_required", "credential_generation", "pin_policy", "authorization_revision"} {
		if isSensitiveMetadataKey(key) {
			t.Fatalf("业务标志 %q 不应被判定为敏感键", key)
		}
	}
	for _, key := range []string{"signature", "otp_code", "pin", "jwt", "bearer", "credential", "private_key", "user_password", "id_token", "signing_key"} {
		if !isSensitiveMetadataKey(key) {
			t.Fatalf("凭据键 %q 必须被判定为敏感键", key)
		}
	}
}
