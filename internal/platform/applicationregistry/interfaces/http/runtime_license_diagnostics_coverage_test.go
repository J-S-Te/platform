package http

import (
	"strings"
	"testing"
)

// TestRuntimeLicenseDiagnosticsCoverEveryPersistedLicenseCode 要求诊断映射覆盖部署状态里
// 可能持久化的全部许可证/运行批准错误码，并且每类给出不同、可执行且不泄露内部细节的指引。
func TestRuntimeLicenseDiagnosticsCoverEveryPersistedLicenseCode(t *testing.T) {
	cases := map[string]string{
		"LICENSE_RUNTIME_DOCKER_UNAVAILABLE":   "docker",
		"LICENSE_RUNTIME_STARTUP_FAILED":       "运行",
		"LICENSE_RUNTIME_APPROVAL_UNAVAILABLE": "批准",
		"LICENSE_RUNTIME_APPROVAL_MISMATCH":    "不匹配",
		"LICENSE_RUNTIME_NOT_READY":            "确认",
		"LICENSE_RUNTIME_ENROLLMENT_FAILED":    "登记",
		"LICENSE_RUNTIME_CONFIRMATION_FAILED":  "确认",
		"LICENSE_RUNTIME_BINDING_FAILED":       "绑定",
		"LICENSE_NOT_INITIALIZED":              "许可",
		"LICENSE_CONFIRMATION_REQUIRED":        "确认",
		"LICENSE_BINDING_MISMATCH":             "绑定",
		"LICENSE_EVIDENCE_UNAVAILABLE":         "证据",
		"LICENSE_CLOCK_RECOVERY_REQUIRED":      "时钟",
	}
	seenMessage := map[string]string{}
	for code, keyword := range cases {
		message, nextAction := runtimeLicenseApprovalDiagnostic(code)
		if message == "" || nextAction == "" {
			t.Fatalf("code %s has no user-facing diagnosis", code)
		}
		if !strings.Contains(strings.ToLower(message), strings.ToLower(keyword)) {
			t.Fatalf("code %s message %q must mention %q", code, message, keyword)
		}
		// docker-socket-proxy 是已对外可见的组件名，不属于泄露；禁止的是命令注入与凭据/路径细节。
		for _, forbidden := range []string{"docker --", "credential", "client_secret", "/private/"} {
			if strings.Contains(message, forbidden) || strings.Contains(nextAction, forbidden) {
				t.Fatalf("code %s leaks %q", code, forbidden)
			}
		}
		if previous, ok := seenMessage[message]; ok {
			t.Fatalf("codes %s and %s share the same message %q", previous, code, message)
		}
		seenMessage[message] = code
	}
}
