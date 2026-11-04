package identityhttp

import (
	"context"
	"errors"
	"net/http"

	"github.com/J-S-Te/Basic-Platform/internal/platform/identity/application"
	sessiondomain "github.com/J-S-Te/Basic-Platform/internal/platform/sessiongovernance/domain"
	"github.com/J-S-Te/Basic-Platform/internal/shared/authctx"
	"github.com/J-S-Te/Basic-Platform/internal/shared/httperror"
	"github.com/J-S-Te/Basic-Platform/internal/shared/httpresponse"
)

type browserAuthenticator interface {
	Authenticate(context.Context, string) (authctx.Principal, error)
}

// trustedBrowserPrincipal 只将确定缺失或过期视为无父锚点，保留依赖失败。
// 参数：r为当前请求，cookieName为平台Cookie名，auth为平台认证服务。
// 返回值：可信主体、确定缺失时nil，或必须中止回调的依赖错误。
func trustedBrowserPrincipal(r *http.Request, cookieName string, auth browserAuthenticator) (*authctx.Principal, error) {
	cookie, err := r.Cookie(cookieName)
	if err != nil || cookie.Value == "" {
		return nil, nil
	}
	principal, err := auth.Authenticate(r.Context(), cookie.Value)
	if errors.Is(err, application.ErrUnauthenticated) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &principal, nil
}

// writeCentralSessionFailure 输出确定失效、锚点冲突或暂时依赖故障。
// 参数：w/r为HTTP边界，err为中央协议或存储错误。
// 返回值：无；输出不包含底层错误及认证材料。
func writeCentralSessionFailure(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, sessiondomain.ErrBindingConflict):
		httpresponse.WriteError(w, r, http.StatusConflict, httperror.New("SESSION_BINDING_CONFLICT", "登录会话已变化，请重新登录", nil))
	case errors.Is(err, sessiondomain.ErrInactive):
		httpresponse.WriteError(w, r, http.StatusUnauthorized, httperror.Unauthenticated)
	default:
		httpresponse.WriteError(w, r, http.StatusServiceUnavailable, httperror.New("SESSION_DEPENDENCY_UNAVAILABLE", "会话服务暂不可用，请稍后重试", nil))
	}
}
