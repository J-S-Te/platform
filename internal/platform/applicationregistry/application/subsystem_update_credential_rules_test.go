package application

import "testing"

// 轮换规则：ADOPT/RETRY 总是换新密钥；普通更新仅在 rotate 标记时换。
func TestShouldRotateSubsystemUpdateSecret(t *testing.T) {
	cases := []struct {
		operation string
		rotate    bool
		want      bool
	}{
		{"ONBOARD", false, false},
		{"UPDATE", false, false},
		{"UPDATE", true, true},
		{"ADOPT", false, true},
		{"RETRY", false, true},
		{"adopt", false, true},
		{" retry ", false, true},
	}
	for _, item := range cases {
		if got := ShouldRotateSubsystemUpdateSecret(item.operation, item.rotate); got != item.want {
			t.Fatalf("rotate(%q, %v) = %v, want %v", item.operation, item.rotate, got, item.want)
		}
	}
}

// 范围修复：不一致即需整体替换；期望为空表示不约束（不触发修复）。
func TestShouldRepairSubsystemUpdateScopes(t *testing.T) {
	if ShouldRepairSubsystemUpdateScopes([]string{"a"}, []string{"a"}) {
		t.Fatal("identical scopes must not trigger repair")
	}
	if !ShouldRepairSubsystemUpdateScopes([]string{"a"}, []string{"a", "b"}) {
		t.Fatal("missing scope must trigger repair")
	}
	if !ShouldRepairSubsystemUpdateScopes([]string{"a", "b"}, []string{"a"}) {
		t.Fatal("extra scope must trigger repair")
	}
	if ShouldRepairSubsystemUpdateScopes([]string{"a"}, nil) {
		t.Fatal("empty expectation must not trigger repair")
	}
}

// 范围派生：文件网关写入器必须含 bind/download；其它用途保持单一范围。
func TestSubsystemUpdateServiceCredentialScopes(t *testing.T) {
	gateway := SubsystemUpdateServiceCredentialRequirement{Purpose: ServiceCredentialFileGatewayWrite, Scopes: []string{"platform:file:upload"}}
	scopes := SubsystemUpdateServiceCredentialScopes(gateway)
	if len(scopes) != 3 || scopes[0] != "platform:file:upload" {
		t.Fatalf("file gateway scopes = %v", scopes)
	}
	other := SubsystemUpdateServiceCredentialRequirement{Purpose: ServiceCredentialAuditIngest, Scopes: []string{"audit.ingest"}}
	if got := SubsystemUpdateServiceCredentialScopes(other); len(got) != 1 || got[0] != "audit.ingest" {
		t.Fatalf("non-gateway scopes = %v", got)
	}
}
