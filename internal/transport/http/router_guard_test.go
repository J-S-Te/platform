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

// 安全审查 AUD-2026-010 路由表守卫：GET /login 与 GET /oidc/callback 每次调用都会
// 触发对身份源的出站协议动作（授权重定向 / token 交换 + userinfo + DB 查询），必须在
// 注册点声明 IP 固定窗口限流（60/min，与 /oauth2/token 同一中间件）。任何人删除/漏加
// 限流都会在此测试失败，强制重新评估出站放大面。
func TestOutboundAuthFlowRoutesStayBehindIPRateLimit(t *testing.T) {
	source, err := os.ReadFile("router.go")
	if err != nil {
		t.Fatalf("read router.go: %v", err)
	}
	routes := []struct{ method, path string }{
		{method: "GET", path: "/login"},
		{method: "GET", path: "/oidc/callback"},
	}
	content := string(source)
	// 限流器必须仍是 /oauth2/token 同款 IP 固定窗口（60/min）。
	if !strings.Contains(content, "authFlowRateLimit := middleware.FixedWindowRateLimit(60, time.Minute)") {
		t.Fatal("authFlowRateLimit must be defined as middleware.FixedWindowRateLimit(60, time.Minute)")
	}
	for _, route := range routes {
		matched := false
		for _, line := range strings.Split(content, "\n") {
			trimmed := strings.TrimSpace(line)
			if strings.HasPrefix(trimmed, "authRouter."+route.method+"(\""+route.path+"\",") {
				matched = true
				if !strings.Contains(line, "authFlowRateLimit") {
					t.Fatalf("route %s %s is missing the authFlowRateLimit IP rate limiter: %s", route.method, route.path, trimmed)
				}
			}
		}
		if !matched {
			t.Fatalf("route %s %s not found in router.go", route.method, route.path)
		}
	}
}

// 安全审查 M-9 路由表守卫：POST /api/v1/auth/credential-challenges 是匿名端点，每次
// 调用都会向 credential_encryption_challenge 插入一行。它必须挂在独立的 IP 固定窗口
// 限流上（120/min，与 /login 的 IP 兜底桶同量级），且不得与 authFlowRateLimit 共用
// 实例——共池会让打满挑战桶的攻击者连带耗尽登录跳转/回调额度。任何人删除或替换该
// 中间件都会在此失败，强制重新评估匿名写入放大面。
func TestCredentialChallengesRouteStaysBehindDedicatedIPRateLimit(t *testing.T) {
	source, err := os.ReadFile("router.go")
	if err != nil {
		t.Fatalf("read router.go: %v", err)
	}
	content := string(source)
	if !strings.Contains(content, "credentialChallengeRateLimit := middleware.FixedWindowRateLimit(120, time.Minute)") {
		t.Fatal("credentialChallengeRateLimit must be defined as middleware.FixedWindowRateLimit(120, time.Minute)")
	}
	matched := false
	for _, line := range strings.Split(content, "\n") {
		trimmed := strings.TrimSpace(line)
		if !strings.HasPrefix(trimmed, `authRouter.POST("/credential-challenges",`) {
			continue
		}
		matched = true
		if !strings.Contains(line, "credentialChallengeRateLimit") {
			t.Fatalf("route POST /credential-challenges is missing the dedicated IP rate limiter: %s", trimmed)
		}
		if strings.Contains(line, "authFlowRateLimit") {
			t.Fatalf("route POST /credential-challenges must not share the outbound auth bucket: %s", trimmed)
		}
	}
	if !matched {
		t.Fatal("route POST /credential-challenges not found in router.go")
	}
}

// 安全审查 SEC-B7 路由表守卫：登录的账号维度限流必须在凭据解密**之后**执行。凭据加密
// 模式下 account 位于 credential_envelope 密文内，挂在最外层的 gin 中间件只能读到密文，
// 账号桶会静默退化为只计 IP 桶（账号维度限流失效）。本守卫锁定：限流用
// LoginRateLimitHTTP 包住明文 handler，且该 handler 再交给 WrapJSON 解密。
func TestLoginRouteRateLimitsAfterCredentialDecryption(t *testing.T) {
	source, err := os.ReadFile("router.go")
	if err != nil {
		t.Fatalf("read router.go: %v", err)
	}
	content := string(source)
	limitWrapping := `middleware.LoginRateLimitHTTP("account", 30, 120, time.Minute)(authHandler.Login)`
	limitIndex := strings.Index(content, limitWrapping)
	if limitIndex < 0 {
		t.Fatalf("login rate limit must wrap the plaintext handler: %s", limitWrapping)
	}
	decryptWrapping := `loginHandler = operational.CredentialEncryption.WrapJSON(credentialcrypto.PurposeLogin, "auth.login", loginHandler)`
	decryptIndex := strings.Index(content, decryptWrapping)
	if decryptIndex < 0 {
		t.Fatalf("encrypted login must wrap loginHandler with WrapJSON: %s", decryptWrapping)
	}
	if limitIndex > decryptIndex {
		t.Fatal("the rate-limited handler must be built before WrapJSON wraps it, otherwise decryption happens after rate limiting")
	}
	// 旧实现（限流挂在解密外层的 gin 中间件）不得回归：那种写法在加密模式下账号桶失效。
	loginRouteFound := false
	for _, line := range strings.Split(content, "\n") {
		trimmed := strings.TrimSpace(line)
		if !strings.HasPrefix(trimmed, `authRouter.POST("/login",`) {
			continue
		}
		loginRouteFound = true
		if !strings.Contains(line, "adaptHandler(loginHandler)") {
			t.Fatalf("route POST /login must register the decrypted, rate-limited handler: %s", trimmed)
		}
		if strings.Contains(line, "middleware.LoginRateLimit(") {
			t.Fatalf("route POST /login must not rate limit before decryption: %s", trimmed)
		}
	}
	if !loginRouteFound {
		t.Fatal("route POST /login not found in router.go")
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
