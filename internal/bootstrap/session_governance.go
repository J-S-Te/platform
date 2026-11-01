package bootstrap

import (
	"context"
	"errors"

	sessiondomain "github.com/J-S-Te/Basic-Platform/internal/platform/sessiongovernance/domain"
	sessionstore "github.com/J-S-Te/Basic-Platform/internal/platform/sessiongovernance/infrastructure"
	"github.com/J-S-Te/Basic-Platform/internal/transport/http/middleware"
)

// centralSessionVerifier 仅接受配置中的Keycloak签名材料，保持与业务授权验证相同信任边界。
type centralSessionVerifier struct {
	verifier   *middleware.KeycloakBrokerJWTVerifier
	terminator interface {
		LogoutSession(context.Context, string) error
	}
}

// TerminateSession 终止已经验签且被中央判定失效的单个上游sid。
// 参数：ctx为请求，identity为本配置验证器返回的可信声明。
// 返回值：精确删除结果；未装配管理适配器时失败关闭，不做账号级回退。
func (v centralSessionVerifier) TerminateSession(ctx context.Context, identity sessionstore.VerifiedIdentity) error {
	if v.terminator == nil {
		return errors.New("exact session termination is unavailable")
	}
	return v.terminator.LogoutSession(ctx, identity.SID)
}

// VerifySessionTokens 将验签结果收窄为中央绑定需要的原始身份。
// 参数：ctx为请求，access/id为原始令牌，client为票据客户端，nonce为票据摘要。
// 返回值：可信认证声明；明确校验失败为未认证，签名依赖不可用保留503语义。
func (v centralSessionVerifier) VerifySessionTokens(ctx context.Context, access, id, client, nonce string) (sessionstore.VerifiedIdentity, error) {
	claims, err := v.verifier.VerifySessionTokens(ctx, access, id, client, nonce)
	if errors.Is(err, middleware.ErrKeycloakUnavailable) {
		return sessionstore.VerifiedIdentity{}, err
	}
	if err != nil {
		return sessionstore.VerifiedIdentity{}, sessiondomain.ErrInactive
	}
	return sessionstore.VerifiedIdentity{Issuer: claims.Issuer, SID: claims.SessionID, TenantID: claims.TenantID,
		IdentityID: claims.IdentityID, Subject: claims.Subject, AuthenticatedAt: claims.AuthenticatedAt}, nil
}
