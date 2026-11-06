// Package keycloakctx 在 HTTP 认证边界与 broker 取证处理器之间传递已验证、
// 范围收窄的 Keycloak broker 身份。
package keycloakctx

import "context"

type brokerClaimsKey struct{}

// TokenUseAccessToken 是 Keycloak 为平台 mapper 下发的 access token 用途标记（M-2）。
// 它属于 Keycloak 外部令牌域，刻意不复用平台自签 OIDC 令牌的用途常量：两者由不同
// 签发方解释，混用会让平台自签域的口径变化直接影响 broker 边界。
const TokenUseAccessToken = "access_token"

// BrokerClaims 只包含证实一次 broker 登录所需的最小声明集，
// 且只能在 Keycloak JWT 验签通过之后填充。TokenUse 是令牌自述用途（keycloak 的
// token_use 声明）；验签层只透传，是否允许某用途由各认证边界自行强制。
type BrokerClaims struct {
	Issuer          string
	Subject         string
	SessionID       string
	TenantID        string
	IdentityID      string
	AuthorizedParty string
	Audience        []string
	TokenUse        string
}

// WithBrokerClaims 把验签后的 broker 声明注入上下文。该值只能由认证边界注入；
// 下游不得把普通请求参数升级为 BrokerClaims。
func WithBrokerClaims(ctx context.Context, claims BrokerClaims) context.Context {
	// 该值只能由认证边界注入；下游不得把普通请求参数升级为 BrokerClaims。
	return context.WithValue(ctx, brokerClaimsKey{}, claims)
}

// BrokerClaimsFromContext 从上下文读取已验签的 broker 声明。
// 读取不到时必须按未认证处理，不能回退到客户端提供的身份字段。
func BrokerClaimsFromContext(ctx context.Context) (BrokerClaims, bool) {
	// 读取不到已验签上下文时必须按未认证处理，不能回退到客户端提供的身份字段。
	claims, ok := ctx.Value(brokerClaimsKey{}).(BrokerClaims)
	return claims, ok
}
