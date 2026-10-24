// Package accesssubject 把紧凑 OIDC JWT 声明绑定回活动的租户内存储记录。
package accesssubject

import (
	"context"
	"errors"
	"net/netip"
	"strings"
	"time"

	"github.com/J-S-Te/Basic-Platform/internal/platform/oidc/domain"
	oidchttp "github.com/J-S-Te/Basic-Platform/internal/platform/oidc/interfaces/http"
)

// repository 是 UserInfo 所需的最小查询集合。两个查询各自独立要求活动的
// client/session 状态；其租户与主体标识再在此处交叉核对。
type repository interface {
	FindClient(context.Context, string, time.Time) (domain.Client, error)
	ResolveSessionSubject(context.Context, string, time.Time) (domain.SessionSubject, error)
}

// Clock 为活动会话查询提供权威的校验时间。
type Clock interface {
	Now() time.Time
}

// Resolver 是由 OIDC 仓储支撑的 AccessTokenSubjectResolver 实现。
type Resolver struct {
	repository repository
	clock      Clock
}

// New 构造解析器；repository 可以是生产 GORM OIDC 仓储。
func New(repository repository, clock Clock) (*Resolver, error) {
	if repository == nil || clock == nil {
		return nil, errors.New("OIDC access-token subject resolver dependencies must not be nil")
	}
	return &Resolver{repository: repository, clock: clock}, nil
}

// ResolveAccessTokenSubject 校验签名 access token 中的 client_id、会话 ID 与用户主体
// 仍指向同一组合的活动租户内 client/session 记录。
func (resolver *Resolver) ResolveAccessTokenSubject(ctx context.Context, clientID, sessionID, userID string) (oidchttp.AccessTokenSubject, error) {
	if resolver == nil || resolver.repository == nil || resolver.clock == nil {
		return oidchttp.AccessTokenSubject{}, errors.New("OIDC access-token subject resolver is not initialized")
	}
	clientID, sessionID, userID = strings.TrimSpace(clientID), strings.TrimSpace(sessionID), strings.TrimSpace(userID)
	if clientID == "" || sessionID == "" || userID == "" {
		return oidchttp.AccessTokenSubject{}, errors.New("OIDC access-token subject is incomplete")
	}
	now := resolver.clock.Now().UTC()
	client, err := resolver.repository.FindClient(ctx, clientID, now)
	if err != nil {
		return oidchttp.AccessTokenSubject{}, err
	}
	subject, err := resolver.repository.ResolveSessionSubject(ctx, sessionID, now)
	if err != nil {
		return oidchttp.AccessTokenSubject{}, err
	}
	if subject.TenantID != client.TenantID || subject.UserID != userID {
		return oidchttp.AccessTokenSubject{}, errors.New("OIDC access-token subject does not match active storage")
	}
	return oidchttp.AccessTokenSubject{TenantID: subject.TenantID, OAuthClientID: client.ID, LoginIP: canonicalLoginIP(subject.LoginIP.String())}, nil
}

func canonicalLoginIP(value string) string {
	ip, err := netip.ParseAddr(strings.TrimSpace(value))
	if err != nil || !ip.IsValid() || !ip.IsGlobalUnicast() || ip.IsPrivate() {
		return ""
	}
	return ip.String()
}

var _ oidchttp.AccessTokenSubjectResolver = (*Resolver)(nil)
