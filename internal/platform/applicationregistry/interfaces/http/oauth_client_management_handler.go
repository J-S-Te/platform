// Package http 包含经平台 Gin 路由挂载的 net/http 适配层。
package http

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	stdhttp "net/http"
	"strings"
	"time"

	application "github.com/J-S-Te/Basic-Platform/internal/platform/applicationregistry/application"
	"github.com/J-S-Te/Basic-Platform/internal/shared/authctx"
	"github.com/J-S-Te/Basic-Platform/internal/shared/httperror"
	"github.com/J-S-Te/Basic-Platform/internal/shared/httpresponse"
	"github.com/J-S-Te/Basic-Platform/internal/transport/http/middleware"
)

const oauthClientManagementRequestBodyLimit = 1 << 20

type oauthClientManagementService interface {
	CreateOAuthClient(context.Context, application.OAuthClientCreateInput) (application.OAuthClientCreateResult, error)
	ListOAuthClients(context.Context, string) ([]application.OAuthClientView, error)
	GetOAuthClient(context.Context, string, string) (application.OAuthClientView, error)
	ReplaceOAuthClientScopes(context.Context, application.OAuthClientScopesUpdateInput) (application.OAuthClientView, error)
	ReplaceOAuthClientRedirectURIs(context.Context, application.OAuthClientRedirectURIsUpdateInput) (application.OAuthClientView, error)
	GetOAuthClientPostLogoutRedirectURIs(context.Context, string, string) (application.OAuthClientPostLogoutRedirectURIsView, error)
	ReplaceOAuthClientPostLogoutRedirectURIs(context.Context, application.OAuthClientPostLogoutRedirectURIsUpdateInput) (application.OAuthClientPostLogoutRedirectURIsView, error)
	GetOAuthClientJWKs(context.Context, string, string) (application.OAuthClientJWKsView, error)
	ReplaceOAuthClientJWKs(context.Context, application.OAuthClientJWKsUpdateInput) (application.OAuthClientJWKsView, error)
	DisableOAuthClient(context.Context, application.OAuthClientDisableInput) (application.OAuthClientView, error)
	CreateOAuthClientSecret(context.Context, application.OAuthClientSecretCreateInput) (application.OAuthClientSecretResult, error)
	RotateOAuthClientSecret(context.Context, application.OAuthClientSecretRotateInput) (application.OAuthClientSecretResult, error)
	DisableOAuthClientSecret(context.Context, application.OAuthClientSecretDisableInput) error
}

// OAuthClientManagementHandler 经 HTTP 暴露 OAuth 客户端管理用例。
// 路由与授权策略登记刻意归组合层持有，本适配层不自行挂载。
type OAuthClientManagementHandler struct {
	service oauthClientManagementService
	logger  *slog.Logger
}

// NewOAuthClientManagementHandler 构造 OAuth 客户端管理 HTTP 适配器；
// service 缺失即拒绝构造，logger 缺失时回退到默认日志器。
func NewOAuthClientManagementHandler(service oauthClientManagementService, logger *slog.Logger) (*OAuthClientManagementHandler, error) {
	if service == nil {
		return nil, errors.New("oauth client management service is required")
	}
	if logger == nil {
		logger = slog.Default()
	}
	return &OAuthClientManagementHandler{service: service, logger: logger}, nil
}

// ListOAuthClients 处理 GET /api/v1/oauth-clients，租户取自认证主体。
func (handler *OAuthClientManagementHandler) ListOAuthClients(writer stdhttp.ResponseWriter, request *stdhttp.Request) {
	principal, ok := authctx.PrincipalFromContext(request.Context())
	if !ok || strings.TrimSpace(principal.Tenant.ID) == "" || strings.TrimSpace(principal.User.ID) == "" {
		httpresponse.WriteError(writer, request, stdhttp.StatusUnauthorized, httperror.Unauthenticated)
		return
	}

	clients, err := handler.service.ListOAuthClients(request.Context(), principal.Tenant.ID)
	if err != nil {
		handler.writeOAuthClientManagementError(writer, request, err)
		return
	}
	httpresponse.WriteSuccess(writer, request, stdhttp.StatusOK, "OAuth 客户端查询成功", toOAuthClientResponses(clients))
}

// CreateOAuthClient 处理 POST /api/v1/oauth-clients；仅在响应体一次性返回初始 Secret。
func (handler *OAuthClientManagementHandler) CreateOAuthClient(writer stdhttp.ResponseWriter, request *stdhttp.Request) {
	principal, ok := authctx.PrincipalFromContext(request.Context())
	if !ok || strings.TrimSpace(principal.Tenant.ID) == "" || strings.TrimSpace(principal.User.ID) == "" {
		httpresponse.WriteError(writer, request, stdhttp.StatusUnauthorized, httperror.Unauthenticated)
		return
	}

	var payload oauthClientCreateRequest
	if err := decodeOAuthClientManagementJSON(writer, request, &payload); err != nil {
		handler.writeOAuthClientManagementError(writer, request, err)
		return
	}

	result, err := handler.service.CreateOAuthClient(request.Context(), application.OAuthClientCreateInput{
		TenantID:               principal.Tenant.ID,
		ApplicationID:          payload.ApplicationID,
		EnvironmentID:          payload.EnvironmentID,
		OperatorID:             principal.User.ID,
		ClientID:               payload.ClientID,
		ClientName:             payload.ClientName,
		ClientType:             payload.ClientType,
		TokenAuthMethod:        payload.TokenAuthMethod,
		AccessTokenTTLSeconds:  payload.AccessTokenTTLSeconds,
		RefreshTokenTTLSeconds: payload.RefreshTokenTTLSeconds,
		RequirePKCE:            payload.RequirePKCE,
		GrantTypes:             payload.GrantTypes,
		Scopes:                 payload.Scopes,
		RedirectURIs:           payload.RedirectURIs,
		SecretValidUntil:       payload.SecretValidUntil,
	})
	if err != nil {
		handler.writeOAuthClientManagementError(writer, request, err)
		return
	}

	// 安全审查 SEC-D5：体寻址的创建操作没有路径 *_id，登记被创建对象 id 供审计中间件
	// 写入事件 ResourceID/metadata.resource_id；失败路径不登记，避免空键或错配。
	middleware.MarkCreatedResource(request, result.Client.ID)

	response := oauthClientCreateResponse{OAuthClient: toOAuthClientResponse(result.Client)}
	if result.PlaintextSecret != "" {
		setOAuthClientSecretNoStoreHeaders(writer)
		response.ClientSecret = result.PlaintextSecret
	}
	httpresponse.WriteSuccess(writer, request, stdhttp.StatusCreated, "OAuth 客户端已创建", response)
}

// GetOAuthClient 处理 GET /api/v1/oauth-clients/:oauth_client_id。
func (handler *OAuthClientManagementHandler) GetOAuthClient(writer stdhttp.ResponseWriter, request *stdhttp.Request) {
	principal, ok := authctx.PrincipalFromContext(request.Context())
	if !ok || strings.TrimSpace(principal.Tenant.ID) == "" || strings.TrimSpace(principal.User.ID) == "" {
		httpresponse.WriteError(writer, request, stdhttp.StatusUnauthorized, httperror.Unauthenticated)
		return
	}

	clientID := strings.TrimSpace(request.PathValue("oauth_client_id"))
	if clientID == "" {
		handler.writeOAuthClientManagementError(writer, request, application.ErrManagementValidation)
		return
	}
	client, err := handler.service.GetOAuthClient(request.Context(), principal.Tenant.ID, clientID)
	if err != nil {
		handler.writeOAuthClientManagementError(writer, request, err)
		return
	}
	httpresponse.WriteSuccess(writer, request, stdhttp.StatusOK, "OAuth 客户端查询成功", toOAuthClientResponse(client))
}

// UpdateOAuthClientScopes 处理 PUT /api/v1/oauth-clients/:oauth_client_id/scopes，
// 请求体需携带乐观锁版本，冲突返回 409。
func (handler *OAuthClientManagementHandler) UpdateOAuthClientScopes(writer stdhttp.ResponseWriter, request *stdhttp.Request) {
	principal, clientID, ok := handler.oauthClientPrincipalAndID(writer, request)
	if !ok {
		return
	}
	var payload oauthClientScopesUpdateRequest
	if err := decodeOAuthClientManagementJSON(writer, request, &payload); err != nil {
		handler.writeOAuthClientManagementError(writer, request, err)
		return
	}
	if payload.Scopes == nil || payload.Version == 0 {
		handler.writeOAuthClientManagementError(writer, request, application.ErrManagementValidation)
		return
	}
	client, err := handler.service.ReplaceOAuthClientScopes(request.Context(), application.OAuthClientScopesUpdateInput{
		TenantID: principal.Tenant.ID, OAuthClientID: clientID, OperatorID: principal.User.ID,
		Scopes: *payload.Scopes, Version: payload.Version,
	})
	if err != nil {
		handler.writeOAuthClientManagementError(writer, request, err)
		return
	}
	httpresponse.WriteSuccess(writer, request, stdhttp.StatusOK, "OAuth 客户端 Scope 已更新", toOAuthClientResponse(client))
}

// UpdateOAuthClientRedirectURIs 处理 PUT /api/v1/oauth-clients/:oauth_client_id/redirect-uris，
// 整体替换回调地址集合。
func (handler *OAuthClientManagementHandler) UpdateOAuthClientRedirectURIs(writer stdhttp.ResponseWriter, request *stdhttp.Request) {
	principal, clientID, ok := handler.oauthClientPrincipalAndID(writer, request)
	if !ok {
		return
	}
	var payload oauthClientRedirectURIsUpdateRequest
	if err := decodeOAuthClientManagementJSON(writer, request, &payload); err != nil {
		handler.writeOAuthClientManagementError(writer, request, err)
		return
	}
	if payload.RedirectURIs == nil || payload.Version == 0 {
		handler.writeOAuthClientManagementError(writer, request, application.ErrManagementValidation)
		return
	}
	client, err := handler.service.ReplaceOAuthClientRedirectURIs(request.Context(), application.OAuthClientRedirectURIsUpdateInput{
		TenantID: principal.Tenant.ID, OAuthClientID: clientID, OperatorID: principal.User.ID,
		RedirectURIs: *payload.RedirectURIs, Version: payload.Version,
	})
	if err != nil {
		handler.writeOAuthClientManagementError(writer, request, err)
		return
	}
	httpresponse.WriteSuccess(writer, request, stdhttp.StatusOK, "OAuth 客户端回调地址已更新", toOAuthClientResponse(client))
}

// GetOAuthClientPostLogoutRedirectURIs 处理独立登出后回调地址集合查询。
func (handler *OAuthClientManagementHandler) GetOAuthClientPostLogoutRedirectURIs(writer stdhttp.ResponseWriter, request *stdhttp.Request) {
	principal, clientID, ok := handler.oauthClientPrincipalAndID(writer, request)
	if !ok {
		return
	}
	view, err := handler.service.GetOAuthClientPostLogoutRedirectURIs(request.Context(), principal.Tenant.ID, clientID)
	if err != nil {
		handler.writeOAuthClientManagementError(writer, request, err)
		return
	}
	httpresponse.WriteSuccess(writer, request, stdhttp.StatusOK, "OAuth 客户端登出后回调地址查询成功", toOAuthClientPostLogoutRedirectURIsResponse(view))
}

// UpdateOAuthClientPostLogoutRedirectURIs 使用乐观锁整体替换独立登出后回调地址集合。
func (handler *OAuthClientManagementHandler) UpdateOAuthClientPostLogoutRedirectURIs(writer stdhttp.ResponseWriter, request *stdhttp.Request) {
	principal, clientID, ok := handler.oauthClientPrincipalAndID(writer, request)
	if !ok {
		return
	}
	var payload oauthClientPostLogoutRedirectURIsUpdateRequest
	if err := decodeOAuthClientManagementJSON(writer, request, &payload); err != nil {
		handler.writeOAuthClientManagementError(writer, request, err)
		return
	}
	if payload.PostLogoutRedirectURIs == nil || payload.Version == 0 {
		handler.writeOAuthClientManagementError(writer, request, application.ErrManagementValidation)
		return
	}
	view, err := handler.service.ReplaceOAuthClientPostLogoutRedirectURIs(request.Context(), application.OAuthClientPostLogoutRedirectURIsUpdateInput{
		TenantID: principal.Tenant.ID, OAuthClientID: clientID, OperatorID: principal.User.ID,
		PostLogoutRedirectURIs: *payload.PostLogoutRedirectURIs, Version: payload.Version,
	})
	if err != nil {
		handler.writeOAuthClientManagementError(writer, request, err)
		return
	}
	httpresponse.WriteSuccess(writer, request, stdhttp.StatusOK, "OAuth 客户端登出后回调地址已更新", toOAuthClientPostLogoutRedirectURIsResponse(view))
}

// GetOAuthClientJWKs 处理客户端公钥 JWK 集合查询。
func (handler *OAuthClientManagementHandler) GetOAuthClientJWKs(writer stdhttp.ResponseWriter, request *stdhttp.Request) {
	principal, clientID, ok := handler.oauthClientPrincipalAndID(writer, request)
	if !ok {
		return
	}
	view, err := handler.service.GetOAuthClientJWKs(request.Context(), principal.Tenant.ID, clientID)
	if err != nil {
		handler.writeOAuthClientManagementError(writer, request, err)
		return
	}
	httpresponse.WriteSuccess(writer, request, stdhttp.StatusOK, "OAuth 客户端公钥查询成功", toOAuthClientJWKsResponse(view))
}

// UpdateOAuthClientJWKs 使用乐观锁整体替换客户端公钥 JWK 集合。
func (handler *OAuthClientManagementHandler) UpdateOAuthClientJWKs(writer stdhttp.ResponseWriter, request *stdhttp.Request) {
	principal, clientID, ok := handler.oauthClientPrincipalAndID(writer, request)
	if !ok {
		return
	}
	var payload oauthClientJWKsUpdateRequest
	if err := decodeOAuthClientManagementJSON(writer, request, &payload); err != nil {
		handler.writeOAuthClientManagementError(writer, request, err)
		return
	}
	if payload.PublicJWKs == nil || payload.Version == 0 {
		handler.writeOAuthClientManagementError(writer, request, application.ErrManagementValidation)
		return
	}
	jwks := make([]application.OAuthClientJWK, 0, len(*payload.PublicJWKs))
	for _, jwk := range *payload.PublicJWKs {
		var validFrom time.Time
		if jwk.ValidFrom != nil {
			validFrom = *jwk.ValidFrom
		}
		jwks = append(jwks, application.OAuthClientJWK{
			KeyID: jwk.KeyID, PublicJWK: jwk.PublicJWK, Algorithm: jwk.Algorithm,
			ValidFrom: validFrom, ValidUntil: jwk.ValidUntil,
		})
	}
	view, err := handler.service.ReplaceOAuthClientJWKs(request.Context(), application.OAuthClientJWKsUpdateInput{
		TenantID: principal.Tenant.ID, OAuthClientID: clientID, OperatorID: principal.User.ID,
		JWKs: jwks, Version: payload.Version,
	})
	if err != nil {
		handler.writeOAuthClientManagementError(writer, request, err)
		return
	}
	httpresponse.WriteSuccess(writer, request, stdhttp.StatusOK, "OAuth 客户端公钥已更新", toOAuthClientJWKsResponse(view))
}

// DisableOAuthClient 处理 POST /api/v1/oauth-clients/:oauth_client_id/disable，
// 同时吊销该客户端全部 ACTIVE Secret。
func (handler *OAuthClientManagementHandler) DisableOAuthClient(writer stdhttp.ResponseWriter, request *stdhttp.Request) {
	principal, clientID, ok := handler.oauthClientPrincipalAndID(writer, request)
	if !ok {
		return
	}
	var payload oauthClientDisableRequest
	if err := decodeOAuthClientManagementJSON(writer, request, &payload); err != nil {
		handler.writeOAuthClientManagementError(writer, request, err)
		return
	}
	if payload.Version == 0 {
		handler.writeOAuthClientManagementError(writer, request, application.ErrManagementValidation)
		return
	}
	client, err := handler.service.DisableOAuthClient(request.Context(), application.OAuthClientDisableInput{
		TenantID: principal.Tenant.ID, OAuthClientID: clientID, OperatorID: principal.User.ID, Version: payload.Version,
	})
	if err != nil {
		handler.writeOAuthClientManagementError(writer, request, err)
		return
	}
	httpresponse.WriteSuccess(writer, request, stdhttp.StatusOK, "OAuth 客户端已禁用", toOAuthClientResponse(client))
}

// CreateCredential 处理 POST /api/v1/oauth-clients/:oauth_client_id/credentials，
// 新 Secret 仅随本响应一次性下发。
func (handler *OAuthClientManagementHandler) CreateCredential(writer stdhttp.ResponseWriter, request *stdhttp.Request) {
	principal, clientID, ok := handler.oauthClientPrincipalAndID(writer, request)
	if !ok {
		return
	}
	var payload oauthClientCredentialCreateRequest
	if err := decodeOAuthClientManagementJSON(writer, request, &payload); err != nil {
		handler.writeOAuthClientManagementError(writer, request, err)
		return
	}

	result, err := handler.service.CreateOAuthClientSecret(request.Context(), application.OAuthClientSecretCreateInput{
		TenantID: principal.Tenant.ID, OAuthClientID: clientID, OperatorID: principal.User.ID, ValidUntil: payload.ValidUntil,
	})
	if err != nil {
		handler.writeOAuthClientManagementError(writer, request, err)
		return
	}
	setOAuthClientSecretNoStoreHeaders(writer)
	httpresponse.WriteSuccess(writer, request, stdhttp.StatusCreated, "OAuth 客户端密钥已创建", oauthClientCredentialSecretResponse{
		Credential: toOAuthClientCredentialResponse(result.Credential), ClientSecret: result.PlaintextSecret,
	})
}

// DisableCredential 处理 POST /api/v1/oauth-clients/:oauth_client_id/credentials/:credential_id/disable，
// 立即吊销单个 Secret 版本。
func (handler *OAuthClientManagementHandler) DisableCredential(writer stdhttp.ResponseWriter, request *stdhttp.Request) {
	principal, clientID, ok := handler.oauthClientPrincipalAndID(writer, request)
	if !ok {
		return
	}
	credentialID := strings.TrimSpace(request.PathValue("credential_id"))
	if credentialID == "" {
		handler.writeOAuthClientManagementError(writer, request, application.ErrManagementValidation)
		return
	}
	if err := handler.service.DisableOAuthClientSecret(request.Context(), application.OAuthClientSecretDisableInput{
		TenantID: principal.Tenant.ID, OAuthClientID: clientID, CredentialID: credentialID, OperatorID: principal.User.ID,
	}); err != nil {
		handler.writeOAuthClientManagementError(writer, request, err)
		return
	}
	writer.WriteHeader(stdhttp.StatusNoContent)
}

// RotateCredential 处理 POST /api/v1/oauth-clients/:oauth_client_id/credentials/rotate，
// 旧密钥仅在限定的重叠窗口内保持有效。
func (handler *OAuthClientManagementHandler) RotateCredential(writer stdhttp.ResponseWriter, request *stdhttp.Request) {
	principal, clientID, ok := handler.oauthClientPrincipalAndID(writer, request)
	if !ok {
		return
	}
	var payload oauthClientCredentialRotateRequest
	if err := decodeOAuthClientManagementJSON(writer, request, &payload); err != nil {
		handler.writeOAuthClientManagementError(writer, request, err)
		return
	}
	result, err := handler.service.RotateOAuthClientSecret(request.Context(), application.OAuthClientSecretRotateInput{
		TenantID: principal.Tenant.ID, OAuthClientID: clientID, OperatorID: principal.User.ID,
		OverlapSeconds: payload.OverlapSeconds, ValidUntil: payload.ValidUntil,
	})
	if err != nil {
		handler.writeOAuthClientManagementError(writer, request, err)
		return
	}
	setOAuthClientSecretNoStoreHeaders(writer)
	httpresponse.WriteSuccess(writer, request, stdhttp.StatusCreated, "OAuth 客户端密钥已轮换", oauthClientCredentialSecretResponse{
		Credential: toOAuthClientCredentialResponse(result.Credential), ClientSecret: result.PlaintextSecret,
	})
}

func (handler *OAuthClientManagementHandler) oauthClientPrincipalAndID(writer stdhttp.ResponseWriter, request *stdhttp.Request) (authctx.Principal, string, bool) {
	principal, ok := authctx.PrincipalFromContext(request.Context())
	if !ok || strings.TrimSpace(principal.Tenant.ID) == "" || strings.TrimSpace(principal.User.ID) == "" {
		httpresponse.WriteError(writer, request, stdhttp.StatusUnauthorized, httperror.Unauthenticated)
		return authctx.Principal{}, "", false
	}
	clientID := strings.TrimSpace(request.PathValue("oauth_client_id"))
	if clientID == "" {
		handler.writeOAuthClientManagementError(writer, request, application.ErrManagementValidation)
		return authctx.Principal{}, "", false
	}
	return principal, clientID, true
}

func (handler *OAuthClientManagementHandler) writeOAuthClientManagementError(writer stdhttp.ResponseWriter, request *stdhttp.Request, err error) {
	switch {
	case errors.Is(err, application.ErrManagementValidation):
		httpresponse.WriteError(writer, request, stdhttp.StatusUnprocessableEntity, httperror.Validation)
	case errors.Is(err, application.ErrManagementNotFound):
		httpresponse.WriteError(writer, request, stdhttp.StatusNotFound, httperror.NotFound)
	case errors.Is(err, application.ErrManagementConflict):
		httpresponse.WriteError(writer, request, stdhttp.StatusConflict, httperror.Conflict)
	default:
		// 不记录错误值：服务层错误可能源自 Secret 操作，其文本不得进入日志。
		handler.logger.Error("oauth client management request failed", "path", request.URL.Path)
		httpresponse.WriteError(writer, request, stdhttp.StatusInternalServerError, httperror.Internal)
	}
}

func decodeOAuthClientManagementJSON(writer stdhttp.ResponseWriter, request *stdhttp.Request, target any) error {
	request.Body = stdhttp.MaxBytesReader(writer, request.Body, oauthClientManagementRequestBodyLimit)
	decoder := json.NewDecoder(request.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return application.ErrManagementValidation
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return application.ErrManagementValidation
	}
	return nil
}

func setOAuthClientSecretNoStoreHeaders(writer stdhttp.ResponseWriter) {
	writer.Header().Set("Cache-Control", "no-store")
	writer.Header().Set("Pragma", "no-cache")
}

type oauthClientCreateRequest struct {
	ApplicationID          string     `json:"application_id"`
	EnvironmentID          string     `json:"environment_id"`
	ClientID               string     `json:"client_id"`
	ClientName             string     `json:"client_name"`
	ClientType             string     `json:"client_type"`
	TokenAuthMethod        string     `json:"token_auth_method"`
	AccessTokenTTLSeconds  uint       `json:"access_token_ttl_seconds"`
	RefreshTokenTTLSeconds uint       `json:"refresh_token_ttl_seconds"`
	RequirePKCE            bool       `json:"require_pkce"`
	GrantTypes             []string   `json:"grant_types"`
	Scopes                 []string   `json:"scopes"`
	RedirectURIs           []string   `json:"redirect_uris"`
	SecretValidUntil       *time.Time `json:"secret_valid_until"`
}

type oauthClientScopesUpdateRequest struct {
	Scopes  *[]string `json:"scopes"`
	Version uint64    `json:"version"`
}

type oauthClientRedirectURIsUpdateRequest struct {
	RedirectURIs *[]string `json:"redirect_uris"`
	Version      uint64    `json:"version"`
}

type oauthClientPostLogoutRedirectURIsUpdateRequest struct {
	PostLogoutRedirectURIs *[]string `json:"post_logout_redirect_uris"`
	Version                uint64    `json:"version"`
}

type oauthClientJWKsUpdateRequest struct {
	PublicJWKs *[]oauthClientJWKRequest `json:"public_jwks"`
	Version    uint64                   `json:"version"`
}

type oauthClientJWKRequest struct {
	KeyID      string          `json:"key_id"`
	PublicJWK  json.RawMessage `json:"public_jwk"`
	Algorithm  string          `json:"algorithm"`
	ValidFrom  *time.Time      `json:"valid_from"`
	ValidUntil *time.Time      `json:"valid_until"`
}

type oauthClientDisableRequest struct {
	Version uint64 `json:"version"`
}

type oauthClientCredentialCreateRequest struct {
	ValidUntil *time.Time `json:"valid_until"`
}

type oauthClientCredentialRotateRequest struct {
	OverlapSeconds uint       `json:"overlap_seconds"`
	ValidUntil     *time.Time `json:"valid_until"`
}

type oauthClientCreateResponse struct {
	OAuthClient  oauthClientResponse `json:"oauth_client"`
	ClientSecret string              `json:"client_secret,omitempty"`
}

type oauthClientCredentialSecretResponse struct {
	Credential   oauthClientCredentialResponse `json:"credential"`
	ClientSecret string                        `json:"client_secret"`
}

type oauthClientPostLogoutRedirectURIsResponse struct {
	OAuthClientID          string   `json:"oauth_client_id"`
	Version                uint64   `json:"version"`
	PostLogoutRedirectURIs []string `json:"post_logout_redirect_uris"`
}

type oauthClientJWKsResponse struct {
	OAuthClientID string                   `json:"oauth_client_id"`
	Version       uint64                   `json:"version"`
	PublicJWKs    []oauthClientJWKResponse `json:"public_jwks"`
}

// oauthClientJWKResponse 仅返回公钥 JWK 和可安全公开的轮换元数据。
type oauthClientJWKResponse struct {
	KeyID      string          `json:"key_id"`
	PublicJWK  json.RawMessage `json:"public_jwk"`
	Algorithm  string          `json:"algorithm,omitempty"`
	ValidFrom  time.Time       `json:"valid_from"`
	ValidUntil *time.Time      `json:"valid_until,omitempty"`
	Status     string          `json:"status"`
}

// oauthClientResponse 显式定义对外 API 投影。应用层视图刻意不带 JSON 标签，
// 因为它们还要被非 HTTP 适配层复用。
type oauthClientResponse struct {
	OAuthClientID          string                          `json:"oauth_client_id"`
	ApplicationID          string                          `json:"application_id"`
	EnvironmentID          string                          `json:"environment_id"`
	ClientID               string                          `json:"client_id"`
	ClientName             string                          `json:"client_name"`
	ClientType             string                          `json:"client_type"`
	TokenAuthMethod        string                          `json:"token_auth_method"`
	AccessTokenTTLSeconds  uint                            `json:"access_token_ttl_seconds"`
	RefreshTokenTTLSeconds uint                            `json:"refresh_token_ttl_seconds"`
	RequirePKCE            bool                            `json:"require_pkce"`
	Status                 string                          `json:"status"`
	Version                uint64                          `json:"version"`
	GrantTypes             []string                        `json:"grant_types"`
	Scopes                 []string                        `json:"scopes"`
	RedirectURIs           []string                        `json:"redirect_uris"`
	Credentials            []oauthClientCredentialResponse `json:"credentials"`
	CreatedAt              time.Time                       `json:"created_at"`
	UpdatedAt              time.Time                       `json:"updated_at"`
}

// oauthClientCredentialResponse 仅含凭据元数据；Secret 明文与 bcrypt 哈希
// 均不得经读取或变更端点返回。
type oauthClientCredentialResponse struct {
	CredentialID string     `json:"credential_id"`
	Fingerprint  string     `json:"fingerprint"`
	ValidFrom    time.Time  `json:"valid_from"`
	ValidUntil   *time.Time `json:"valid_until,omitempty"`
	RevokedAt    *time.Time `json:"revoked_at,omitempty"`
	Status       string     `json:"status"`
}

func toOAuthClientResponses(views []application.OAuthClientView) []oauthClientResponse {
	responses := make([]oauthClientResponse, 0, len(views))
	for _, view := range views {
		responses = append(responses, toOAuthClientResponse(view))
	}
	return responses
}

func toOAuthClientPostLogoutRedirectURIsResponse(view application.OAuthClientPostLogoutRedirectURIsView) oauthClientPostLogoutRedirectURIsResponse {
	return oauthClientPostLogoutRedirectURIsResponse{
		OAuthClientID: view.OAuthClientID, Version: view.Version, PostLogoutRedirectURIs: view.PostLogoutRedirectURIs,
	}
}

func toOAuthClientJWKsResponse(view application.OAuthClientJWKsView) oauthClientJWKsResponse {
	publicJWKs := make([]oauthClientJWKResponse, 0, len(view.JWKs))
	for _, jwk := range view.JWKs {
		publicJWKs = append(publicJWKs, oauthClientJWKResponse{
			KeyID: jwk.KeyID, PublicJWK: jwk.PublicJWK, Algorithm: jwk.Algorithm,
			ValidFrom: jwk.ValidFrom, ValidUntil: jwk.ValidUntil, Status: jwk.Status,
		})
	}
	return oauthClientJWKsResponse{OAuthClientID: view.OAuthClientID, Version: view.Version, PublicJWKs: publicJWKs}
}

func toOAuthClientResponse(view application.OAuthClientView) oauthClientResponse {
	credentials := make([]oauthClientCredentialResponse, 0, len(view.Credentials))
	for _, credential := range view.Credentials {
		credentials = append(credentials, toOAuthClientCredentialResponse(credential))
	}
	return oauthClientResponse{
		OAuthClientID: view.ID, ApplicationID: view.ApplicationID, EnvironmentID: view.EnvironmentID,
		ClientID: view.ClientID, ClientName: view.ClientName, ClientType: view.ClientType,
		TokenAuthMethod: view.TokenAuthMethod, AccessTokenTTLSeconds: view.AccessTokenTTLSeconds,
		RefreshTokenTTLSeconds: view.RefreshTokenTTLSeconds, RequirePKCE: view.RequirePKCE,
		Status: view.Status, Version: view.Version, GrantTypes: view.GrantTypes, Scopes: view.Scopes,
		RedirectURIs: view.RedirectURIs, Credentials: credentials, CreatedAt: view.CreatedAt, UpdatedAt: view.UpdatedAt,
	}
}

func toOAuthClientCredentialResponse(view application.OAuthClientCredentialView) oauthClientCredentialResponse {
	return oauthClientCredentialResponse{
		CredentialID: view.ID, Fingerprint: view.Fingerprint, ValidFrom: view.ValidFrom,
		ValidUntil: view.ValidUntil, RevokedAt: view.RevokedAt, Status: view.Status,
	}
}
