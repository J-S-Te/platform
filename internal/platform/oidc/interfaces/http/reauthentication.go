package oidchttp

import (
	"crypto/sha256"
	"encoding/base64"
	"net/http"
	"strings"
	"time"

	"github.com/J-S-Te/Basic-Platform/internal/platform/oidc/application"
)

type reauthenticationProofManager interface {
	IssueReauthenticationProof(string, string, time.Time) (string, error)
	VerifyReauthenticationProof(string, string, time.Time) (string, time.Time, error)
}

// authenticationRequestHash 将凭据限制到原始授权请求，不能用于另一客户端、nonce或回调。
// 参数：r为本域原生authorize请求。
// 返回值：原请求路径及完整查询参数的不可逆摘要。
func authenticationRequestHash(r *http.Request) string {
	hash := sha256.Sum256([]byte(r.URL.RequestURI()))
	return base64.RawURLEncoding.EncodeToString(hash[:])
}

// reauthenticationAfter 只在完成新的服务器会话后接受原请求的短效签名凭据。
// 参数：r为当前请求，sessionID为数据库核验后的当前平台会话。
// 返回值：原要求时间；无凭据、错请求、原会话或过期时返回零值。
func (h *Handler) reauthenticationAfter(r *http.Request, sessionID string) time.Time {
	manager, ok := h.jwtManager.(reauthenticationProofManager)
	if !ok {
		return time.Time{}
	}
	cookie, err := r.Cookie(h.cookie.name + "_oidc_reauth")
	if err != nil {
		return time.Time{}
	}
	previous, after, err := manager.VerifyReauthenticationProof(cookie.Value, authenticationRequestHash(r), h.clock.Now().UTC())
	if err != nil || previous == sessionID {
		return time.Time{}
	}
	return after
}

// redirectForFreshAuthentication 保留原请求与nonce，通过独立签名Cookie要求正常重新登录。
// 参数：w为HTTP响应，r为原请求，sessionID为原平台会话，input为已解析的认证要求。
// 返回值：无；静默认证返回login_required，签名依赖不可用返回503而不降级。
func (h *Handler) redirectForFreshAuthentication(w http.ResponseWriter, r *http.Request, sessionID string, input application.AuthorizationInput) {
	for _, prompt := range strings.Fields(input.Prompt) {
		if prompt == "none" {
			writeAuthorizationError(w, http.StatusBadRequest, "login_required")
			return
		}
	}
	manager, ok := h.jwtManager.(reauthenticationProofManager)
	if !ok {
		writeAuthorizationError(w, http.StatusServiceUnavailable, "temporarily_unavailable")
		return
	}
	if sessionID == "" {
		sessionID = "no-authenticated-session"
	}
	hash, now := authenticationRequestHash(r), h.clock.Now().UTC()
	proof := ""
	if cookie, err := r.Cookie(h.cookie.name + "_oidc_reauth"); err == nil {
		if _, _, err := manager.VerifyReauthenticationProof(cookie.Value, hash, now); err == nil {
			proof = cookie.Value
		}
	}
	if proof == "" {
		var err error
		proof, err = manager.IssueReauthenticationProof(hash, sessionID, now)
		if err != nil {
			writeAuthorizationError(w, http.StatusServiceUnavailable, "temporarily_unavailable")
			return
		}
	}
	h.setReauthenticationCookie(w, proof, 300)
	redirectToPlatformLogin(w, r)
}

// setReauthenticationCookie 写入或清除用途隔离的重新认证凭据，不写浏览器存储。
// 参数：w为HTTP响应，proof为签名材料，maxAge为短效秒数或删除标记。
// 返回值：无。
func (h *Handler) setReauthenticationCookie(w http.ResponseWriter, proof string, maxAge int) {
	http.SetCookie(w, &http.Cookie{Name: h.cookie.name + "_oidc_reauth", Value: proof, Path: "/", HttpOnly: true, Secure: h.cookie.secure, SameSite: h.cookie.sameSite, MaxAge: maxAge})
}
