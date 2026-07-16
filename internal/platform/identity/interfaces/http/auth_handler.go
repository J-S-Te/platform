// Package identityhttp adapts identity authentication use cases to the public HTTP API.
package identityhttp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/J-S-Te/Basic-Platform/internal/platform/identity/application"
	"github.com/J-S-Te/Basic-Platform/internal/shared/authctx"
	"github.com/J-S-Te/Basic-Platform/internal/shared/config"
	"github.com/J-S-Te/Basic-Platform/internal/shared/httperror"
	"github.com/J-S-Te/Basic-Platform/internal/shared/httpresponse"
)

const maxLoginRequestBytes = 8 * 1024

// Handler exposes the OpenAPI /auth endpoints.
type Handler struct {
	service applicationService
	logger  *slog.Logger
	cookie  cookieConfig
}

type applicationService interface {
	Login(ctx context.Context, input application.LoginInput) (application.SessionResult, error)
	Authenticate(ctx context.Context, token string) (authctx.Principal, error)
	Refresh(ctx context.Context, principal authctx.Principal) (application.SessionResult, error)
	Logout(ctx context.Context, principal authctx.Principal) error
}

// cookieConfig keeps only cookie properties that must remain consistent when setting or clearing.
type cookieConfig struct {
	name     string
	secure   bool
	sameSite http.SameSite
}

// NewHandler creates the identity HTTP adapter. The caller validates the service during bootstrap.
func NewHandler(service applicationService, logger *slog.Logger, authConfig config.AuthConfig) (*Handler, error) {
	if service == nil || logger == nil {
		return nil, errors.New("identity HTTP handler dependencies must not be nil")
	}
	sameSite, err := parseSameSite(authConfig.SessionCookieSameSite)
	if err != nil {
		return nil, err
	}
	return &Handler{
		service: service,
		logger:  logger,
		cookie:  cookieConfig{name: authConfig.SessionCookieName, secure: authConfig.SessionCookieSecure, sameSite: sameSite},
	}, nil
}

type loginRequest struct {
	Account   string `json:"account"`
	Password  string `json:"password"`
	LoginType string `json:"login_type"`
}

type sessionResponse struct {
	ExpiresAt   time.Time `json:"expires_at"`
	RedirectURL string    `json:"redirect_url"`
}

// Login handles the sole P0 login type: a local account with an Argon2id password credential.
func (handler *Handler) Login(writer http.ResponseWriter, request *http.Request) {
	payload, err := decodeLoginRequest(writer, request)
	if err != nil {
		httpresponse.WriteError(writer, request, http.StatusUnprocessableEntity, httperror.Validation)
		return
	}

	result, err := handler.service.Login(request.Context(), application.LoginInput{
		Account: payload.Account, Password: payload.Password, IPAddress: remoteIP(request), UserAgent: request.UserAgent(),
	})
	if err != nil {
		if errors.Is(err, application.ErrUnauthenticated) {
			httpresponse.WriteError(writer, request, http.StatusUnauthorized, httperror.New("AUTH_UNAUTHENTICATED", "账号或密码错误", nil))
			return
		}
		handler.writeApplicationError(writer, request, err)
		return
	}
	handler.setSessionCookie(writer, result.Token, result.ExpiresAt)
	httpresponse.WriteSuccess(writer, request, http.StatusOK, "登录成功", toSessionResponse(result))
}

// Refresh renews the already authenticated current browser session and replaces its cookie.
func (handler *Handler) Refresh(writer http.ResponseWriter, request *http.Request) {
	principal, ok := authctx.PrincipalFromContext(request.Context())
	if !ok {
		httpresponse.WriteError(writer, request, http.StatusUnauthorized, httperror.Unauthenticated)
		return
	}
	result, err := handler.service.Refresh(request.Context(), principal)
	if err != nil {
		handler.writeApplicationError(writer, request, err)
		return
	}
	handler.setSessionCookie(writer, result.Token, result.ExpiresAt)
	httpresponse.WriteSuccess(writer, request, http.StatusOK, "会话已刷新", toSessionResponse(result))
}

// Logout revokes the already authenticated session and always writes a matching expired cookie on
// a successful server-side revocation.
func (handler *Handler) Logout(writer http.ResponseWriter, request *http.Request) {
	principal, ok := authctx.PrincipalFromContext(request.Context())
	if !ok {
		httpresponse.WriteError(writer, request, http.StatusUnauthorized, httperror.Unauthenticated)
		return
	}
	if err := handler.service.Logout(request.Context(), principal); err != nil {
		handler.writeApplicationError(writer, request, err)
		return
	}
	handler.clearSessionCookie(writer)
	httpresponse.WriteSuccess(writer, request, http.StatusOK, "已退出登录", map[string]any{})
}

// Me returns only the principal that middleware obtained by verifying the JWT and persisted state.
func (handler *Handler) Me(writer http.ResponseWriter, request *http.Request) {
	principal, ok := authctx.PrincipalFromContext(request.Context())
	if !ok {
		httpresponse.WriteError(writer, request, http.StatusUnauthorized, httperror.Unauthenticated)
		return
	}
	httpresponse.WriteSuccess(writer, request, http.StatusOK, "操作成功", principal)
}

// Authenticate delegates token verification to the application service for HTTP middleware.
func (handler *Handler) Authenticate(ctx context.Context, token string) (authctx.Principal, error) {
	return handler.service.Authenticate(ctx, token)
}

// CookieName exposes the configured cookie name to the HTTP authentication middleware.
func (handler *Handler) CookieName() string {
	return handler.cookie.name
}

func (handler *Handler) setSessionCookie(writer http.ResponseWriter, token string, expiresAt time.Time) {
	http.SetCookie(writer, &http.Cookie{
		Name: handler.cookie.name, Value: token, Path: "/", HttpOnly: true,
		Secure: handler.cookie.secure, SameSite: handler.cookie.sameSite, Expires: expiresAt.UTC(),
	})
}

func (handler *Handler) clearSessionCookie(writer http.ResponseWriter) {
	http.SetCookie(writer, &http.Cookie{
		Name: handler.cookie.name, Value: "", Path: "/", HttpOnly: true,
		Secure: handler.cookie.secure, SameSite: handler.cookie.sameSite,
		Expires: time.Unix(1, 0).UTC(), MaxAge: -1,
	})
}

func (handler *Handler) writeApplicationError(writer http.ResponseWriter, request *http.Request, err error) {
	var locked application.AccountLockedError
	switch {
	case errors.As(err, &locked):
		apiError := httperror.AccountLocked
		apiError.Details = map[string]time.Time{"locked_until": locked.LockedUntil.UTC()}
		httpresponse.WriteError(writer, request, http.StatusLocked, apiError)
	case errors.Is(err, application.ErrUnauthenticated):
		httpresponse.WriteError(writer, request, http.StatusUnauthorized, httperror.Unauthenticated)
	default:
		handler.logger.Error("identity authentication request failed", "error", err)
		httpresponse.WriteError(writer, request, http.StatusInternalServerError, httperror.Internal)
	}
}

func decodeLoginRequest(writer http.ResponseWriter, request *http.Request) (loginRequest, error) {
	defer request.Body.Close()
	decoder := json.NewDecoder(http.MaxBytesReader(writer, request.Body, maxLoginRequestBytes))
	decoder.DisallowUnknownFields()

	var payload loginRequest
	if err := decoder.Decode(&payload); err != nil {
		return loginRequest{}, err
	}
	if err := ensureSingleJSONValue(decoder); err != nil {
		return loginRequest{}, err
	}
	payload.Account = strings.TrimSpace(payload.Account)
	if payload.LoginType != "password" || payload.Account == "" || len(payload.Account) > 128 ||
		payload.Password == "" || len(payload.Password) > 256 {
		return loginRequest{}, errors.New("login request does not meet contract")
	}
	return payload, nil
}

func ensureSingleJSONValue(decoder *json.Decoder) error {
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			return errors.New("request has multiple JSON values")
		}
		return err
	}
	return nil
}

func parseSameSite(value string) (http.SameSite, error) {
	switch strings.ToLower(value) {
	case "lax":
		return http.SameSiteLaxMode, nil
	case "strict":
		return http.SameSiteStrictMode, nil
	case "none":
		return http.SameSiteNoneMode, nil
	default:
		return http.SameSiteDefaultMode, fmt.Errorf("unsupported session cookie SameSite value %q", value)
	}
}

func toSessionResponse(result application.SessionResult) sessionResponse {
	return sessionResponse{ExpiresAt: result.ExpiresAt.UTC(), RedirectURL: result.RedirectURL}
}

func remoteIP(request *http.Request) net.IP {
	host, _, err := net.SplitHostPort(request.RemoteAddr)
	if err == nil {
		return net.ParseIP(host)
	}
	return net.ParseIP(request.RemoteAddr)
}
