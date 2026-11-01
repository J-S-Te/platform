package http

import "testing"

// TestCentralRealmSessionCeiling 避免IdP的独立空闲计时提前切断跨系统活跃用户。
// 参数：t为测试上下文。
// 返回值：无；中央模式下四个技术期限均应为八小时，旧模式保持原策略。
func TestCentralRealmSessionCeiling(t *testing.T) {
	realm := map[string]any{"ssoSessionIdleTimeout": 1800, "ssoSessionMaxLifespan": 36000, "clientSessionIdleTimeout": 0, "clientSessionMaxLifespan": 0}
	changed, payload := centralRealmSessionPatch(realm, true)
	if !changed {
		t.Fatal("old independent provider timer was retained")
	}
	for _, key := range []string{"ssoSessionIdleTimeout", "ssoSessionMaxLifespan", "clientSessionIdleTimeout", "clientSessionMaxLifespan"} {
		if !asInt64Equal(payload[key], 28800) {
			t.Fatalf("%s not aligned to central ceiling", key)
		}
	}
	if changed, _ := centralRealmSessionPatch(payload, true); changed {
		t.Fatal("converged session policy drifted")
	}
	if changed, _ := centralRealmSessionPatch(realm, false); changed {
		t.Fatal("legacy mode was silently changed")
	}
}
