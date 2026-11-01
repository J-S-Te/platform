package infrastructure

import (
	"testing"

	"github.com/J-S-Te/Basic-Platform/internal/platform/applicationregistry/application"
)

// TestManagedSessionRuntimeValues 验证下发值来自平台受控策略且门户使用独立前缀。
// 参数：t为运行配置生成测试上下文。
// 返回值：无；入口漏接、策略不一致或旧请求覆盖配置时失败。
func TestManagedSessionRuntimeValues(t *testing.T) {
	policy, err := application.NewCentralSessionRuntimePolicy(true, "https://platform.example")
	if err != nil {
		t.Fatal(err)
	}
	for _, code := range []string{"contract_management", "project_management", "settlement", "customer_and_opportunity", "data_analysis", "customer_portal"} {
		values, err := managedSessionEnvironment(policy, code)
		if err != nil {
			t.Fatal(err)
		}
		prefix := ""
		if code == "customer_portal" {
			prefix = "PORTAL_"
		}
		if values[prefix+"CENTRAL_SESSION_ENABLED"] != "true" || values[prefix+"CENTRAL_SESSION_PUBLIC_BASE_URL"] != "https://platform.example" {
			t.Fatalf("missing platform policy for %s", code)
		}
	}
	values, err := managedSessionEnvironment(nil, "contract_management")
	if err != nil || len(values) != 0 {
		t.Fatal("legacy signed requests must preserve retained policy")
	}
	if _, err := application.NewCentralSessionRuntimePolicy(true, "https://user:secret@evil.example"); err == nil {
		t.Fatal("invalid platform policy origin accepted")
	}
}
