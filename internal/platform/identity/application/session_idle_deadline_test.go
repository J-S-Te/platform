package application

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/J-S-Te/Basic-Platform/internal/platform/identity/domain"
)

// TestSessionIdleDeadlineSurvivesPrincipalConversion 验证内部期限贯通且不暴露会话信息。
// 参数：t 为测试运行上下文。
// 返回值：无；期限遗失或 JSON 泄漏时报告失败。
func TestSessionIdleDeadlineSurvivesPrincipalConversion(t *testing.T) {
	deadline := time.Date(2026, 10, 30, 8, 30, 0, 0, time.UTC)
	principal := domain.Principal{SessionID: "hidden-session", SessionIdleDeadline: deadline}
	converted := toAuthContextPrincipal(principal)
	if !converted.SessionIdleDeadline.Equal(deadline) {
		t.Fatal("idle deadline lost")
	}
	for _, value := range []any{principal, converted} {
		encoded, err := json.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(encoded), "hidden-session") || strings.Contains(string(encoded), "2026-10-30") {
			t.Fatalf("internal state leaked: %s", encoded)
		}
	}
}
