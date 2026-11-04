package application

import (
	"github.com/J-S-Te/Basic-Platform/internal/platform/oidc/domain"
	"testing"
	"time"
)

// TestAuthorizationGrantKeepsOriginalAuthenticationTime 防止授权码签发时间冒充登录起点。
// 参数：t为测试上下文。
// 返回值：无；原始时间被授权或刷新时间替换时失败。
func TestAuthorizationGrantKeepsOriginalAuthenticationTime(t *testing.T) {
	auth := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	code := domain.AuthorizationCode{CreatedAt: auth.Add(7 * time.Hour), AuthenticatedAt: auth}
	grant := grantFromAuthorizationCode(code, domain.Client{})
	if !grant.AuthorizedAt.Equal(auth) {
		t.Fatalf("auth_time moved from original login: %v", grant.AuthorizedAt)
	}
}
