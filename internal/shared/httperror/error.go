// Package httperror 定义独立于 HTTP 传输实现的稳定 API 错误。
package httperror

// Error 是可安全下发给客户端的 API 错误。Cause 细节只能进结构化日志，绝不能
// 直接序列化返回——其中可能包含基础设施或敏感信息。
type Error struct {
	Code    string
	Message string
	Details any
}

// New 创建一个稳定的 API 错误值。
func New(code, message string, details any) Error {
	return Error{Code: code, Message: message, Details: details}
}

var (
	// NotFound 表示调用方可解析范围内不存在该路由或资源，而非全局不存在。
	NotFound = New("PLATFORM_NOT_FOUND", "请求的资源不存在", nil)
	// MethodNotAllowed 表示路由存在但不支持该 HTTP 方法。
	MethodNotAllowed = New("PLATFORM_METHOD_NOT_ALLOWED", "请求方法不被支持", nil)
	// Validation 表示请求未满足 OpenAPI 输入契约。
	Validation = New("PLATFORM_VALIDATION_ERROR", "请求参数不合法", nil)
	// Unauthenticated 覆盖缺失、无效、过期或已撤销等全部认证失败情形，
	// 统一返回以避免向调用方泄露状态差异。
	Unauthenticated = New("AUTH_UNAUTHENTICATED", "登录状态无效或已过期", nil)
	// Forbidden 表示主体已认证但不持有必需权限。
	Forbidden = New("AUTH_FORBIDDEN", "没有执行此操作的权限", nil)
	// 锁定窗口内的登录统一返回 Unauthenticated（防枚举）；锁定状态经安全模块的
	// 登录策略与解锁接口承载，不单设独立错误码。
	// ConcurrentSession 表示同一账号已存在有效的终端会话。
	ConcurrentSession = New("AUTH_CONCURRENT_SESSION", "该账号已有有效会话；如原页面已关闭，可选择退出原会话并重新登录", nil)
	// Conflict 表示创建或生命周期流转违反 IAM 不变量。
	Conflict = New("IAM_CONFLICT", "资源状态冲突", nil)
	// VersionConflict 表示乐观锁版本已过期。
	VersionConflict = New("IAM_VERSION_CONFLICT", "数据已被更新，请刷新后重试", nil)
	// BootstrapAdminProtected 表示一次管理写操作试图停用、过期、转移或撤销
	// 部署初始化的超级管理员。
	BootstrapAdminProtected = New("IAM_BOOTSTRAP_ADMIN_PROTECTED", "部署初始超级管理员为系统保留账号，不允许执行此操作。", nil)
	// BootstrapAdminConfiguration 表示初始化记录不一致；运维必须修复记录本身，
	// 而不是另行指定其他管理员。
	BootstrapAdminConfiguration = New("IAM_BOOTSTRAP_ADMIN_CONFIGURATION_INVALID", "部署初始超级管理员初始化记录异常，请联系运维处理", nil)
	// Internal 表示服务端内部错误；返回前底层失败必须已记录日志。
	Internal = New("PLATFORM_INTERNAL_ERROR", "服务暂时不可用，请稍后重试", nil)
	// DependencyUnavailable 由就绪探针在必需依赖不可用时返回。
	DependencyUnavailable = New("PLATFORM_DEPENDENCY_UNAVAILABLE", "依赖服务暂时不可用", nil)
)
