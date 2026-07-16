// Package httperror defines stable API errors independent of the HTTP transport implementation.
package httperror

// Error is a client-safe API error. Cause values belong in structured logs and must not be
// serialized directly because they can contain infrastructure or sensitive details.
type Error struct {
	Code    string
	Message string
	Details any
}

// New creates a stable API error value.
func New(code, message string, details any) Error {
	return Error{Code: code, Message: message, Details: details}
}

var (
	// NotFound is returned when no route or resource can be resolved for the caller.
	NotFound = New("PLATFORM_NOT_FOUND", "请求的资源不存在", nil)
	// MethodNotAllowed is returned when a known route does not support the HTTP method.
	MethodNotAllowed = New("PLATFORM_METHOD_NOT_ALLOWED", "请求方法不被支持", nil)
	// Validation is returned when an API request does not satisfy its OpenAPI input contract.
	Validation = New("PLATFORM_VALIDATION_ERROR", "请求参数不合法", nil)
	// Unauthenticated is returned for missing, invalid, expired or revoked authentication state.
	Unauthenticated = New("AUTH_UNAUTHENTICATED", "登录状态无效或已过期", nil)
	// AccountLocked is returned when a local account has an active lock window.
	AccountLocked = New("AUTH_ACCOUNT_LOCKED", "账号已锁定，请稍后再试", nil)
	// Conflict is returned when a create or lifecycle transition violates an IAM invariant.
	Conflict = New("IAM_CONFLICT", "资源状态冲突", nil)
	// VersionConflict is returned when an optimistic-lock version is stale.
	VersionConflict = New("IAM_VERSION_CONFLICT", "数据已被更新，请刷新后重试", nil)
	// Internal is returned after the server has logged the underlying failure.
	Internal = New("PLATFORM_INTERNAL_ERROR", "服务暂时不可用，请稍后重试", nil)
	// DependencyUnavailable is returned by readiness checks when a required dependency is down.
	DependencyUnavailable = New("PLATFORM_DEPENDENCY_UNAVAILABLE", "依赖服务暂时不可用", nil)
)
