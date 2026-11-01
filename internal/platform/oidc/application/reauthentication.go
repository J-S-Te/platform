package application

import (
	"errors"
	"strings"
	"time"
)

var ErrFreshAuthenticationRequired = errors.New("OIDC fresh authentication is required")

// freshAuthenticationRequired 以原认证时间判断直接授权请求的新鲜认证要求。
// 参数：input为校验后的请求和可信重新认证凭据，authenticated为原登录时间，now为服务端时钟。
// 返回值：需要重新认证时为true；授权或刷新时间不能替代原认证时间。
func freshAuthenticationRequired(input AuthorizationInput, authenticated, now time.Time) bool {
	needsPrompt := false
	for _, prompt := range strings.Fields(input.Prompt) {
		if prompt == "login" {
			needsPrompt = true
		}
	}
	if needsPrompt && input.ReauthenticatedAfter.IsZero() {
		return true
	}
	if !input.ReauthenticatedAfter.IsZero() && authenticated.Before(input.ReauthenticatedAfter) {
		return true
	}
	if input.MaxAgeSeconds == nil {
		return false
	}
	if *input.MaxAgeSeconds == 0 {
		return input.ReauthenticatedAfter.IsZero()
	}
	return now.Sub(authenticated) > time.Duration(*input.MaxAgeSeconds)*time.Second
}
