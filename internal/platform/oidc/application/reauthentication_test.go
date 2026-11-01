package application

import (
	"context"
	"errors"
	"github.com/J-S-Te/Basic-Platform/internal/platform/oidc/domain"
	"testing"
	"time"
)

// TestFreshAuthenticationPolicy 验证prompt和max_age使用原认证时间而非本次授权时间。
// 参数：t为固定时钟和明确预期的策略测试上下文。
// 返回值：无；旧登录通过新鲜认证要求或已完成重登仍循环要求认证时失败。
func TestFreshAuthenticationPolicy(t *testing.T) {
	now := time.Date(2026, 11, 1, 1, 0, 0, 0, time.UTC)
	zero, age := uint64(0), uint64(60)
	cases := []struct {
		name  string
		input AuthorizationInput
		auth  time.Time
		want  bool
	}{
		{"normal", AuthorizationInput{}, now.Add(-time.Hour), false},
		{"prompt", AuthorizationInput{Prompt: "login"}, now.Add(-time.Hour), true},
		{"prompt proof", AuthorizationInput{Prompt: "login", ReauthenticatedAfter: now.Add(-time.Second)}, now, false},
		{"old despite proof", AuthorizationInput{Prompt: "login", ReauthenticatedAfter: now}, now.Add(-time.Hour), true},
		{"max age old", AuthorizationInput{MaxAgeSeconds: &age}, now.Add(-time.Minute - time.Second), true},
		{"max age fresh", AuthorizationInput{MaxAgeSeconds: &age}, now.Add(-time.Second), false},
		{"zero requires proof", AuthorizationInput{MaxAgeSeconds: &zero}, now, true},
		{"zero proof", AuthorizationInput{MaxAgeSeconds: &zero, ReauthenticatedAfter: now}, now, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := freshAuthenticationRequired(tc.input, tc.auth, now); got != tc.want {
				t.Fatalf("fresh authentication required=%v, want %v", got, tc.want)
			}
		})
	}
}

// TestAuthorizeEnforcesFreshAuthentication 验证实际授权服务在签发代码之前执行新鲜认证门禁。
// 参数：t为固定时钟和可信客户端夹具的服务层测试上下文。
// 返回值：无；旧会话获得授权码或被误报成其他错误时失败。
func TestAuthorizeEnforcesFreshAuthentication(t *testing.T) {
	now := time.Date(2026, 11, 1, 1, 0, 0, 0, time.UTC)
	repo := &refreshSessionRepositoryStub{client: domain.Client{ID: "client", TenantID: "tenant", ClientID: "web", GrantTypes: map[string]struct{}{"authorization_code": {}}, Scopes: map[string]struct{}{"openid": {}}, RedirectURIs: map[string]struct{}{"https://app.test/callback": {}}}, subject: domain.SessionSubject{TenantID: "tenant", SessionID: "session", AccountID: "account", UserID: "user", AuthenticatedAt: now.Add(-time.Hour), ExpiresAt: now.Add(time.Hour)}}
	service := &Service{repository: repo, clock: refreshSessionClock{now: now}}
	_, err := service.Authorize(context.Background(), AuthorizationInput{ClientID: "web", RedirectURI: "https://app.test/callback", SessionID: "session", Scopes: []string{"openid"}, Prompt: "login"})
	if !errors.Is(err, ErrFreshAuthenticationRequired) {
		t.Fatalf("freshness gate was not applied before code issuance: %v", err)
	}
}
