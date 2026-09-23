package httptransport

import (
	"os"
	"strings"
	"testing"
)

// 安全审查 SEC-B8 路由表守卫：组织/岗位/任职的10条变更/读取路由必须在注册点显式
// 声明路由级 RequirePermission（与 handler 内 scope 检查同权限码）。真实会话链的
// 端到端403 依赖 DB 会话存储与未导出的 handler 构造接口（见任务 description 的
// 测试边界说明），本守卫直接锁定“路由不再自文档化授权要求”这一原始问题：
// 任何人删除/漏加权限码都会在此测试失败，强制重新评估。
func TestOrgPositionMembershipRoutesDeclareRouteLevelPermissions(t *testing.T) {
	source, err := os.ReadFile("router.go")
	if err != nil {
		t.Fatalf("read router.go: %v", err)
	}
	routes := []struct {
		method, path, permission string
	}{
		{method: "GET", path: "/org-units", permission: "platform:organization:read"},
		{method: "POST", path: "/org-units", permission: "platform:organization:create"},
		{method: "PATCH", path: "/org-units/:org_unit_id", permission: "platform:organization:update"},
		{method: "DELETE", path: "/org-units/:org_unit_id", permission: "platform:organization:delete"},
		{method: "GET", path: "/positions", permission: "platform:position:read"},
		{method: "POST", path: "/positions", permission: "platform:position:create"},
		{method: "DELETE", path: "/positions/:position_id", permission: "platform:position:delete"},
		{method: "GET", path: "/memberships", permission: "platform:membership:read"},
		{method: "POST", path: "/memberships", permission: "platform:membership:create"},
		{method: "PATCH", path: "/memberships/:membership_id", permission: "platform:membership:update"},
	}
	for _, route := range routes {
		matched := false
		for _, line := range strings.Split(string(source), "\n") {
			trimmed := strings.TrimSpace(line)
			if strings.HasPrefix(trimmed, "apiRouter."+route.method+"(\""+route.path+"\",") {
				matched = true
				if !strings.Contains(line, "middleware.RequirePermission(\""+route.permission+"\")") {
					t.Fatalf("route %s %s is missing RequirePermission(%q): %s", route.method, route.path, route.permission, trimmed)
				}
			}
		}
		if !matched {
			t.Fatalf("route %s %s not found in router.go", route.method, route.path)
		}
	}
}

// 安全审查 SEC-X5（task-33 平台侧重定义）：逐路由核对结论——所有依赖会话 Cookie 的
// 不安全方法路由都必须留在带 Origin/Sec-Fetch-Site 校验的分组或显式守卫之后。本守卫
// 锁定各分组中间件与两个 logout 守卫，防止未来重构把路由移出受保护分组或删除分组
// 中间件；中间件本身的行为测试见同包 security_test.go 与 middleware/same_origin_test.go。
// Cookie 不适用清单（Bearer/协议客户端认证，无 Cookie 可被跨站非自愿携带）：
// /oauth2/token、/oauth2/par、/oauth2/revoke、/oauth2/userinfo、keycloak broker、
// integration/*（scope Bearer）、bootstrap first-super-admin（无既有会话 Cookie）、/metrics（task-49 认证）。
func TestCookieBackedUnsafeRoutesStayBehindOriginGuards(t *testing.T) {
	source, err := os.ReadFile("router.go")
	if err != nil {
		t.Fatalf("read router.go: %v", err)
	}
	content := string(source)
	guards := []struct {
		owner  string
		needle string
	}{
		{owner: "authRouter 登录会话分组（login/refresh/activity/logout）", needle: "authRouter.Use(middleware.RequireAllowedOriginForUnsafeMethods("},
		{owner: "apiRouter 管理 API 分组（全部 /api/v1 不安全方法）", needle: "apiRouter.Use(middleware.RequireAllowedOriginForUnsafeMethods("},
		{owner: "catalogRouter Bearer 兼容分组", needle: "catalogRouter.Use(middleware.RequireAllowedOriginForUnsafeMethodsOrBearer("},
		{owner: "catalogRouter 纯会话回退分组", needle: "catalogRouter.Use(middleware.RequireAllowedOriginForUnsafeMethods("},
		{owner: "consentRouter 授权同意分组", needle: "consentRouter.Use(middleware.RequireSameOrigin("},
		{owner: "GET /oauth2/logout 导航来源守卫（SEC-B10）", needle: "router.GET(\"/oauth2/logout\", middleware.RequireSameOriginNavigation("},
		{owner: "POST /oauth2/logout 同源守卫", needle: "router.POST(\"/oauth2/logout\", middleware.RequireSameOrigin("},
	}
	for _, guard := range guards {
		if !strings.Contains(content, guard.needle) {
			t.Fatalf("%s 缺少来源校验（SEC-X5）：%s", guard.owner, guard.needle)
		}
	}
}
