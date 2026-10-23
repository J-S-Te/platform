package application

import "testing"

// 批次 3：登录失败事件必须进入采集，并按失败/高风险记录。
func TestSupportedKeycloakAuditEventAcceptsLoginFailure(t *testing.T) {
	event := KeycloakAuditEvent{Category: "LOGIN", Type: "LOGIN_ERROR", SubjectID: "user-1"}
	if !supportedKeycloakAuditEvent(event) {
		t.Fatal("LOGIN_ERROR 必须被采集，否则登录失败不入审计")
	}
	if got := keycloakAuditResult(event); got != "FAILURE" {
		t.Fatalf("Result = %q, want FAILURE", got)
	}
	if got := keycloakAuditRisk(event); got != "HIGH" {
		t.Fatalf("RiskLevel = %q, want HIGH", got)
	}
}

func TestSupportedKeycloakAuditEventStillAcceptsLoginAndLogout(t *testing.T) {
	for _, kind := range []string{"LOGIN", "LOGOUT"} {
		event := KeycloakAuditEvent{Category: "LOGIN", Type: kind, SubjectID: "user-1"}
		if !supportedKeycloakAuditEvent(event) {
			t.Errorf("%s 必须被采集", kind)
		}
		if got := keycloakAuditResult(event); got != "SUCCESS" {
			t.Errorf("%s Result = %q, want SUCCESS", kind, got)
		}
	}
}
