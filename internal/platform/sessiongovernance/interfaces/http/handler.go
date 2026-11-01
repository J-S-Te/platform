// Package sessionhttp 暴露中央会话协议，不返回令牌秘密给浏览器。
package sessionhttp

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	identityapplication "github.com/J-S-Te/Basic-Platform/internal/platform/identity/application"
	"github.com/J-S-Te/Basic-Platform/internal/platform/sessiongovernance/domain"
	store "github.com/J-S-Te/Basic-Platform/internal/platform/sessiongovernance/infrastructure"
	"github.com/J-S-Te/Basic-Platform/internal/shared/authctx"
)

// Repository 是有事务保证的中央存储边界。
type Repository interface {
	RegisteredClient(context.Context, string, string) (store.Client, error)
	Begin(context.Context, store.Client, string, string, time.Time) (string, error)
	TicketContext(context.Context, string, time.Time) (string, string, error)
	Complete(context.Context, string, string, string, store.VerifiedIdentity, time.Time) (store.Binding, error)
	State(context.Context, string, time.Time, bool) (store.Context, error)
	PlatformState(context.Context, string, time.Time, bool) (store.Context, error)
}

// Verifier 独立验签访问和身份令牌，并返回同一次原始认证身份。
type Verifier interface {
	VerifySessionTokens(context.Context, string, string, string, string) (store.VerifiedIdentity, error)
}

// Authenticator 只用于读取当前浏览器平台Cookie，不能从查询参数选择身份。
type Authenticator interface {
	Authenticate(context.Context, string) (authctx.Principal, error)
}

// Handler 持有固定协议依赖；无可用签名验证器时不会降级接受声明。
type Handler struct {
	repository    Repository
	verifier      Verifier
	authenticator Authenticator
	cookieName    string
	now           func() time.Time
}

// NewHandler 装配平台原始JSON会话协议。
// 参数：repository为中央存储，verifier为签名校验，authenticator为平台认证，cookieName为Cookie名。
// 返回值：处理器；任何必需依赖缺失时返回配置错误。
func NewHandler(repository Repository, verifier Verifier, authenticator Authenticator, cookieName string) (*Handler, error) {
	if repository == nil || verifier == nil || authenticator == nil || cookieName == "" {
		return nil, errors.New("central session HTTP dependencies are required")
	}
	return &Handler{repository: repository, verifier: verifier, authenticator: authenticator, cookieName: cookieName, now: time.Now}, nil
}

// Begin 核对精确登记回调并签发一次性浏览器桥接票据。
// 参数：w为响应，r携带client_id、redirect_uri、原state与nonce摘要。
// 返回值：无；成功仅回跳已登记URI，失败不反射目标地址。
func (h *Handler) Begin(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	id, redirect, state, nonce := q.Get("client_id"), q.Get("redirect_uri"), q.Get("state"), q.Get("nonce_challenge")
	raw, err := base64.RawURLEncoding.DecodeString(nonce)
	if len(id) == 0 || len(id) > 128 || len(redirect) > 2048 || len(state) < 16 || len(state) > 512 || err != nil || len(raw) != 32 || base64.RawURLEncoding.EncodeToString(raw) != nonce {
		failure(w, domain.ErrValidation)
		return
	}
	client, err := h.repository.RegisteredClient(r.Context(), id, redirect)
	if err != nil {
		failure(w, err)
		return
	}
	parent := ""
	if cookie, cookieErr := r.Cookie(h.cookieName); cookieErr == nil && cookie.Value != "" {
		principal, authErr := h.authenticator.Authenticate(r.Context(), cookie.Value)
		if authErr != nil && !errors.Is(authErr, identityapplication.ErrUnauthenticated) {
			failure(w, authErr)
			return
		}
		if authErr == nil {
			state, stateErr := h.repository.PlatformState(r.Context(), principal.SessionID, h.now().UTC(), false)
			if stateErr != nil {
				failure(w, stateErr)
				return
			}
			if state.TenantID != client.TenantID {
				failure(w, domain.ErrBindingConflict)
				return
			}
			parent = state.SessionID
		}
	}
	ticket, err := h.repository.Begin(r.Context(), client, nonce, parent, h.now().UTC())
	if err != nil {
		failure(w, err)
		return
	}
	target, err := url.Parse(redirect)
	if err != nil {
		failure(w, domain.ErrValidation)
		return
	}
	values := target.Query()
	values.Set("session_link_ticket", ticket)
	values.Set("state", state)
	target.RawQuery = values.Encode()
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Referrer-Policy", "no-referrer")
	http.Redirect(w, r, target.String(), http.StatusFound)
}

// Complete 独立验签并原子消费票据，绑定原文只返回服务器调用方。
// 参数：w为响应，r携带Bearer访问令牌及ticket/id_token JSON。
// 返回值：无；失败按参数、失效、冲突和依赖异常分类，绝不泄漏底层材料。
func (h *Handler) Complete(w http.ResponseWriter, r *http.Request) {
	access, ok := authorization(r, "Bearer")
	if !ok {
		failure(w, domain.ErrInactive)
		return
	}
	var body struct {
		Ticket  string `json:"ticket"`
		IDToken string `json:"id_token"`
	}
	r.Body = http.MaxBytesReader(w, r.Body, 32<<10)
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&body); err != nil || body.Ticket == "" || len(body.Ticket) > 128 || len(body.IDToken) > 16<<10 {
		failure(w, domain.ErrValidation)
		return
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		failure(w, domain.ErrValidation)
		return
	}
	client, nonce, err := h.repository.TicketContext(r.Context(), body.Ticket, h.now().UTC())
	if err != nil {
		failure(w, err)
		return
	}
	identity, err := h.verifier.VerifySessionTokens(r.Context(), access, body.IDToken, client, nonce)
	if err != nil {
		failure(w, err)
		return
	}
	result, err := h.repository.Complete(r.Context(), body.Ticket, nonce, client, identity, h.now().UTC())
	if err != nil {
		if errors.Is(err, domain.ErrUpstreamInactive) {
			terminator, ok := h.verifier.(interface {
				TerminateSession(context.Context, store.VerifiedIdentity) error
			})
			if !ok {
				failure(w, errors.New("session termination unavailable"))
				return
			}
			if terminateErr := terminator.TerminateSession(r.Context(), identity); terminateErr != nil {
				failure(w, terminateErr)
				return
			}
		}
		failure(w, err)
		return
	}
	respond(w, http.StatusOK, result)
}

// State 只读返回中央窗口，后台状态查询不计真实活动。
// 参数：w为响应，r携带RP服务器的SessionBinding秘密。
// 返回值：无；失败由标准协议错误表示。
func (h *Handler) State(w http.ResponseWriter, r *http.Request) { h.state(w, r, false) }

// Activity 接收RP已做同源及Cookie校验的交互代理。
// 参数：w为响应，r携带RP服务器绑定秘密；浏览器不能直接得到该秘密。
// 返回值：无；已失效周期不能被此接口复活。
func (h *Handler) Activity(w http.ResponseWriter, r *http.Request) { h.state(w, r, true) }

// state 复用生命周期读写入口，不承担业务权限加载。
// 参数：w/r为HTTP请求响应，interact区分只读与真实活动。
// 返回值：无；成功返回不含秘密的中央上下文。
func (h *Handler) state(w http.ResponseWriter, r *http.Request, interact bool) {
	token, ok := authorization(r, "SessionBinding")
	if !ok {
		failure(w, domain.ErrInactive)
		return
	}
	result, err := h.repository.State(r.Context(), token, h.now().UTC(), interact)
	if err != nil {
		failure(w, err)
		return
	}
	respond(w, http.StatusOK, result)
}

// authorization 读取固定方案且有界的服务器认证材料。
// 参数：r为请求，scheme为期望认证方案。
// 返回值：无前后空格的秘密；重复头、空值、异常分隔或超长值返回false。
func authorization(r *http.Request, scheme string) (string, bool) {
	if len(r.Header.Values("Authorization")) != 1 {
		return "", false
	}
	parts := strings.Split(r.Header.Get("Authorization"), " ")
	if len(parts) != 2 || !strings.EqualFold(parts[0], scheme) || len(parts[1]) == 0 || len(parts[1]) > 16<<10 {
		return "", false
	}
	return parts[1], true
}

// failure 将错误收敛为不含内部细节和令牌的OAuth协议响应。
// 参数：w为响应，err为已分类领域或依赖错误。
// 返回值：无；未知错误保持503且不伪装成登出。
func failure(w http.ResponseWriter, err error) {
	status, code := http.StatusServiceUnavailable, "temporarily_unavailable"
	switch {
	case errors.Is(err, domain.ErrValidation):
		status, code = http.StatusBadRequest, "invalid_request"
	case errors.Is(err, domain.ErrInactive):
		status, code = http.StatusUnauthorized, "invalid_session"
	case errors.Is(err, domain.ErrBindingConflict):
		status, code = http.StatusConflict, "SESSION_BINDING_CONFLICT"
	}
	respond(w, status, map[string]string{"error": code})
}

// respond 为会话协议写入不可缓存的JSON。
// 参数：w为响应，status为HTTP状态，value为非敏感投影或服务器专用绑定。
// 返回值：无。
func respond(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Pragma", "no-cache")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}
