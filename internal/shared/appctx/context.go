// Package appctx 在请求上下文中保存已认证的应用主体。
package appctx

import (
	"context"
	"strings"
)

type contextKey struct{}

// Principal 标识被允许调用平台集成 API 的 OAuth client。
// 有意不包含 client secret、bearer token 或任何其他凭据材料。
type Principal struct {
	OAuthClientID   string
	ClientID        string
	TenantID        string
	ApplicationID   string
	ApplicationCode string
	EnvironmentID   string
	EnvironmentCode string
	Scopes          map[string]struct{}
}

// Valid 报告主体是否具备集成请求所需的完整非空应用绑定。该检查有意放在传输边界：
// 有缺陷的认证器不能把不完整令牌转换成可信主体。
func (p Principal) Valid() bool {
	for _, value := range []string{
		p.OAuthClientID,
		p.ClientID,
		p.TenantID,
		p.ApplicationID,
		p.ApplicationCode,
		p.EnvironmentID,
		p.EnvironmentCode,
	} {
		if strings.TrimSpace(value) == "" {
			return false
		}
	}
	if len(p.Scopes) == 0 {
		return false
	}
	for scope := range p.Scopes {
		if scope == "" || strings.TrimSpace(scope) != scope {
			return false
		}
	}
	return true
}

// HasScope 报告客户端令牌是否包含请求的 scope。
func (p Principal) HasScope(scope string) bool {
	if scope == "" || strings.TrimSpace(scope) != scope {
		return false
	}
	_, ok := p.Scopes[scope]
	return ok
}

// WithPrincipal 将应用主体的不可变快照附加到上下文。
func WithPrincipal(ctx context.Context, principal Principal) context.Context {
	return context.WithValue(ctx, contextKey{}, clonePrincipal(principal))
}

// PrincipalFromContext 取出已认证的应用主体。返回的 scope map 是副本，
// 下游处理器无法篡改请求上下文保留的原值。
func PrincipalFromContext(ctx context.Context) (Principal, bool) {
	principal, ok := ctx.Value(contextKey{}).(Principal)
	if !ok || !principal.Valid() {
		return Principal{}, false
	}
	return clonePrincipal(principal), true
}

func clonePrincipal(principal Principal) Principal {
	if principal.Scopes == nil {
		return principal
	}
	scopes := make(map[string]struct{}, len(principal.Scopes))
	for scope := range principal.Scopes {
		scopes[scope] = struct{}{}
	}
	principal.Scopes = scopes
	return principal
}
