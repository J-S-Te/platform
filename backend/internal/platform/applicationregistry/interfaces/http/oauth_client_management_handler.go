// Package http contains net/http adapters that are mounted through the platform Gin router.
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

	application "github.com/J-S-Te/Basic-Platform/backend/internal/platform/applicationregistry/application"
	"github.com/J-S-Te/Basic-Platform/backend/internal/shared/authctx"
	"github.com/J-S-Te/Basic-Platform/backend/internal/shared/httperror"
	"github.com/J-S-Te/Basic-Platform/backend/internal/shared/httpresponse"
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

// OAuthClientManagementHandler exposes OAuth client-management use cases through HTTP.
// Router and authorization policy registration are intentionally owned by the composition layer.
type OAuthClientManagementHandler struct {
	service oauthClientManagementService
	logger  *slog.Logger
}

// NewOAuthClientManagementHandler constructs an OAuth client-management HTTP adapter.
func NewOAuthClientManagementHandler(service oauthClientManagementService, logger *slog.Logger) (*OAuthClientManagementHandler, error) {
	if service == nil {
		return nil, errors.New("oauth client management service is required")
	}
	if logger == nil {
		logger = slog.Default()
	}
	return &OAuthClientManagementHandler{service: service, logger: logger}, nil
}

// ListOAuthClients handles GET /api/v1/oauth-clients.
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

// CreateOAuthClient handles POST /api/v1/oauth-clients.
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

	response := oauthClientCreateResponse{OAuthClient: toOAuthClientResponse(result.Client)}
	if result.PlaintextSecret != "" {
		setOAuthClientSecretNoStoreHeaders(writer)
		response.ClientSecret = result.PlaintextSecret
	}
	httpresponse.WriteSuccess(writer, request, stdhttp.StatusCreated, "OAuth 客户端已创建", response)
}

// GetOAuthClient handles GET /api/v1/oauth-clients/:oauth_client_id.
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

// UpdateOAuthClientScopes handles PUT /api/v1/oauth-clients/:oauth_client_id/scopes.
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

// UpdateOAuthClientRedirectURIs handles PUT /api/v1/oauth-clients/:oauth_client_id/redirect-uris.
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

// DisableOAuthClient handles POST /api/v1/oauth-clients/:oauth_client_id/disable.
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

// CreateCredential handles POST /api/v1/oauth-clients/:oauth_client_id/credentials.
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

// DisableCredential handles POST /api/v1/oauth-clients/:oauth_client_id/credentials/:credential_id/disable.
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

// RotateCredential handles POST /api/v1/oauth-clients/:oauth_client_id/credentials/rotate.
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
		// Do not record the error value: service errors can originate from secret operations.
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

// oauthClientResponse defines the external API projection explicitly. Application-layer views
// intentionally have no JSON tags because they are shared by non-HTTP adapters.
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

// oauthClientCredentialResponse contains credential metadata only. Neither the secret plaintext
// nor its bcrypt hash may be returned by read or mutation endpoints.
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
