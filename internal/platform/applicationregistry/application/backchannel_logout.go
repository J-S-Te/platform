package application

import (
	"context"
	"errors"
	"net/url"
	"strings"
	"time"
)

var ErrInvalidBackchannelLogoutURI = errors.New("invalid back-channel logout URI")

// BackchannelLogoutURIUpdate 是客户端登出回调地址的租户范围更新请求。
type BackchannelLogoutURIUpdate struct{ TenantID, OAuthClientID, OperatorID, URI string }

// BackchannelLogoutURIRepository 持久化已登记的客户端回调地址。
type BackchannelLogoutURIRepository interface {
	Get(context.Context, string, string) (string, error)
	Set(context.Context, BackchannelLogoutURIUpdate, time.Time) error
	Delete(context.Context, string, string) error
}

// ValidateBackchannelLogoutURI 只允许无 query/fragment/userinfo 的 HTTPS 地址；本地测试可显式允许回环 HTTP。
func ValidateBackchannelLogoutURI(raw string, allowLocalHTTP bool) error {
	value := strings.TrimSpace(raw)
	parsed, err := url.Parse(value)
	if err != nil || value == "" || parsed.Host == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return ErrInvalidBackchannelLogoutURI
	}
	if parsed.Scheme == "https" {
		return nil
	}
	if allowLocalHTTP && parsed.Scheme == "http" && (parsed.Hostname() == "127.0.0.1" || parsed.Hostname() == "localhost" || parsed.Hostname() == "::1") {
		return nil
	}
	return ErrInvalidBackchannelLogoutURI
}
