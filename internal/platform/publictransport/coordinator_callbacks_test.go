package publictransport

import "testing"

// 公共传输域名迁移必须覆盖 Back-Channel Logout 登记表：否则域名切换后登录仍可用，
// 但所有 logout_token 都会投递到旧源域而全部失败（BIZ-XEVT2-07）。
func TestManagedCallbackTablesCoverBackchannelLogout(t *testing.T) {
	plans := map[string]callbackTablePlan{}
	for _, plan := range managedCallbackTables {
		plans[plan.ResourceType] = plan
	}
	for _, kind := range []string{"OAUTH_REDIRECT", "OAUTH_POST_LOGOUT", "OAUTH_BACKCHANNEL_LOGOUT"} {
		plan, ok := plans[kind]
		if !ok {
			t.Fatalf("resource kind %s is not covered by managedCallbackTables", kind)
		}
		if plan.Table == "" || plan.Column == "" {
			t.Fatalf("resource kind %s has incomplete plan: %#v", kind, plan)
		}
	}
	backchannel := plans["OAUTH_BACKCHANNEL_LOGOUT"]
	if backchannel.Table != "platform_oauth_backchannel_logout_uri" || backchannel.Column != "logout_uri" {
		t.Fatalf("back-channel plan targets %s.%s", backchannel.Table, backchannel.Column)
	}
	// 每个客户端只有一行的登记表无法双写：准备阶段不得插入目标值，必须由应用阶段
	// 原地改写（ApplyDeletesSource=false 即"改写而非删源行"）。
	if backchannel.PrepareWritesTarget || backchannel.ApplyDeletesSource {
		t.Fatalf("single-row registration must be rewritten in place: %#v", backchannel)
	}
	// 多行回调表保持"准备阶段双写 + 应用阶段删源行"的既有语义。
	for _, kind := range []string{"OAUTH_REDIRECT", "OAUTH_POST_LOGOUT"} {
		if plan := plans[kind]; !plan.PrepareWritesTarget || !plan.ApplyDeletesSource {
			t.Fatalf("multi-row callback plan changed semantics: %#v", plan)
		}
	}
}
