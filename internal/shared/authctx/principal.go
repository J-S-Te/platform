// Package authctx 在请求上下文中保存已认证的主体信息。
package authctx

import (
	"context"
	"net"
	"time"
)

type principalKey struct{}

// ReferenceName 是认证 API 返回的紧凑身份表示。
type ReferenceName struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	Code string `json:"code,omitempty"`
}

// Principal 是受保护请求处理器可以使用的服务端校验身份。
// 它只能由认证中间件写入，调用方不得根据请求头自行构造。
type Principal struct {
	SessionID string `json:"-"`
	// SessionIdleDeadline 来自持久化会话与当前策略，仅用于服务端响应剩余窗口。
	SessionIdleDeadline time.Time `json:"-"`
	// LoginIP 是创建该浏览器会话时捕获的已验证客户端 IP。仅限内部请求上下文使用，
	// 身份类 API 不得把它返回给前端。
	LoginIP         net.IP          `json:"-"`
	Tenant          ReferenceName   `json:"tenant"`
	User            ReferenceName   `json:"user"`
	Account         ReferenceName   `json:"account"`
	Roles           []ReferenceName `json:"roles"`
	PermissionCodes []string        `json:"permission_codes"`
}

// WithPrincipal 将已认证主体附加到请求上下文。
func WithPrincipal(ctx context.Context, principal Principal) context.Context {
	return context.WithValue(ctx, principalKey{}, principal)
}

// PrincipalFromContext 返回已认证主体，以及中间件是否写入过主体的标记。
func PrincipalFromContext(ctx context.Context) (Principal, bool) {
	principal, ok := ctx.Value(principalKey{}).(Principal)
	return principal, ok
}
