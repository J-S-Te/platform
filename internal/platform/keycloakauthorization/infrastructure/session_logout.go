package infrastructure

import (
	"context"
	"errors"
	stdhttp "net/http"
	"net/url"
	"strings"
)

// LogoutSession 删除已验签sid对应的单个Realm会话，不扩大为整个账号注销。
// 参数：ctx为请求上下文，sid为本Realm已验证令牌中的原始会话标识。
// 返回值：已删除或不存在时nil；鉴权、网络及其他HTTP失败返回错误。
func (admin *KeycloakAdmin) LogoutSession(ctx context.Context, sid string) error {
	if sid == "" || len(sid) > 256 || sid != strings.TrimSpace(sid) {
		return errors.New("invalid Keycloak session identifier")
	}
	token, err := admin.token(ctx)
	if err != nil {
		return err
	}
	response, err := admin.request(ctx, token, stdhttp.MethodDelete, "/sessions/"+url.PathEscape(sid), nil)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode == stdhttp.StatusNoContent || response.StatusCode == stdhttp.StatusNotFound {
		return nil
	}
	return admin.statusError("logout exact Keycloak session", response.StatusCode)
}
