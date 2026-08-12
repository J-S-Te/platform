// Package keycloakctx carries the verified, narrowly-scoped Keycloak broker
// identity between the HTTP authentication boundary and the broker evidence handler.
package keycloakctx

import "context"

type brokerClaimsKey struct{}

// BrokerClaims contains only claims needed to attest a brokered login. It is
// populated exclusively after a Keycloak JWT has been verified.
type BrokerClaims struct {
	Issuer          string
	Subject         string
	SessionID       string
	TenantID        string
	IdentityID      string
	AuthorizedParty string
	Audience        []string
}

func WithBrokerClaims(ctx context.Context, claims BrokerClaims) context.Context {
	// 该值只能由认证边界注入；下游不得把普通请求参数升级为 BrokerClaims。
	return context.WithValue(ctx, brokerClaimsKey{}, claims)
}

func BrokerClaimsFromContext(ctx context.Context) (BrokerClaims, bool) {
	// 读取不到已验签上下文时必须按未认证处理，不能回退到客户端提供的身份字段。
	claims, ok := ctx.Value(brokerClaimsKey{}).(BrokerClaims)
	return claims, ok
}
