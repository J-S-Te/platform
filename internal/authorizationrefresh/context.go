package authorizationrefresh

import (
	"context"
	"net/http"
	"strings"
)

const Header = "X-Authorization-Refresh"

type forceKey struct{}

// WithForce 标记服务端本次必须读取最新授权，不改变认证身份。
// 参数：ctx 为原请求上下文，保留其取消、期限与其他信息。
// 返回值：携带新鲜授权标记的派生上下文。
func WithForce(ctx context.Context) context.Context {
	return context.WithValue(ctx, forceKey{}, true)
}

// Forced 判断当前请求是否明确要求重新计算有效授权。
// 参数：ctx 为请求上下文。
// 返回值：仅内部新鲜读取标记为真时返回真。
func Forced(ctx context.Context) bool {
	if ctx == nil {
		return false
	}
	forced, _ := ctx.Value(forceKey{}).(bool)
	return forced
}

// FromRequest 在受控的只读会话入口接受新鲜授权标记。
// 参数：ctx 为原上下文，request 为当前真实路由请求；不读取角色等输入。
// 返回值：符合明确协议时派生新鲜读取上下文，否则原样返回。
func FromRequest(ctx context.Context, request *http.Request) context.Context {
	if request == nil || request.URL == nil || request.Method != http.MethodGet || request.Header.Get(Header) != "1" {
		return ctx
	}
	path := request.URL.Path
	if !strings.HasSuffix(path, "/auth/me") && !strings.HasSuffix(path, "/auth/session") &&
		!strings.HasSuffix(path, "/oauth2/authorization-context") {
		return ctx
	}
	return WithForce(ctx)
}
