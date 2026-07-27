package http

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	stdhttp "net/http"
	"strings"

	"github.com/J-S-Te/Basic-Platform/backend/internal/platform/applicationregistry/application"
	"github.com/J-S-Te/Basic-Platform/backend/internal/shared/authctx"
	"github.com/J-S-Te/Basic-Platform/backend/internal/shared/httperror"
	"github.com/J-S-Te/Basic-Platform/backend/internal/shared/httpresponse"
)

type subsystemOnboardingService interface {
	OnboardSubsystem(context.Context, application.SubsystemOnboardingInput) (application.SubsystemOnboardingResult, error)
	ListPortalApplications(context.Context, string, string) ([]application.PortalApplication, error)
}

// SubsystemOnboardingHandler exposes the simplified onboarding workflow and authenticated portal
// catalog. The configured OIDC issuer is returned to administrators as integration metadata.
type SubsystemOnboardingHandler struct {
	service    subsystemOnboardingService
	oidcIssuer string
	logger     *slog.Logger
}

// NewSubsystemOnboardingHandler constructs the subsystem onboarding HTTP adapter.
func NewSubsystemOnboardingHandler(service subsystemOnboardingService, oidcIssuer string, logger *slog.Logger) (*SubsystemOnboardingHandler, error) {
	if service == nil || strings.TrimSpace(oidcIssuer) == "" {
		return nil, errors.New("subsystem onboarding service and OIDC issuer are required")
	}
	if logger == nil {
		logger = slog.Default()
	}
	return &SubsystemOnboardingHandler{service: service, oidcIssuer: strings.TrimRight(strings.TrimSpace(oidcIssuer), "/"), logger: logger}, nil
}

type subsystemOnboardingRequest struct {
	ApplicationCode string  `json:"application_code"`
	ApplicationName string  `json:"application_name"`
	Description     *string `json:"description"`
	Environment     string  `json:"environment"`
	PublicBaseURL   string  `json:"public_base_url"`
	UpstreamURL     string  `json:"upstream_url"`
	PathPrefix      string  `json:"path_prefix"`
	ClientType      string  `json:"client_type"`
}

type subsystemIntegrationResponse struct {
	Issuer          string `json:"issuer"`
	ClientID        string `json:"client_id"`
	ClientSecret    string `json:"client_secret,omitempty"`
	ClientType      string `json:"client_type"`
	TokenAuthMethod string `json:"token_auth_method"`
	RedirectURI     string `json:"redirect_uri"`
	Scopes          string `json:"scopes"`
	PublicURL       string `json:"public_url"`
	EnvironmentFile string `json:"environment_file"`
	GatewayCommand  string `json:"gateway_command"`
}

type subsystemOnboardingResponse struct {
	Application applicationResponse          `json:"application"`
	Environment environmentResponse          `json:"environment"`
	LoginTarget loginTargetResponse          `json:"login_target"`
	OAuthClient oauthClientResponse          `json:"oauth_client"`
	Integration subsystemIntegrationResponse `json:"integration"`
}

type portalApplicationResponse struct {
	ApplicationID string  `json:"application_id"`
	Code          string  `json:"code"`
	Name          string  `json:"name"`
	Description   *string `json:"description"`
	EnvironmentID string  `json:"environment_id"`
	Environment   string  `json:"environment"`
	PathPrefix    *string `json:"path_prefix"`
	TargetCode    string  `json:"target_code"`
	TargetURI     string  `json:"target_uri"`
	PublicURL     string  `json:"public_url"`
}

// OnboardSubsystem handles POST /api/v1/subsystem-onboarding.
func (handler *SubsystemOnboardingHandler) OnboardSubsystem(writer stdhttp.ResponseWriter, request *stdhttp.Request) {
	principal, ok := subsystemPrincipal(writer, request)
	if !ok {
		return
	}
	var payload subsystemOnboardingRequest
	if !decodeApplicationManagementJSON(writer, request, &payload) {
		return
	}
	result, err := handler.service.OnboardSubsystem(request.Context(), application.SubsystemOnboardingInput{
		TenantID: principal.Tenant.ID, OperatorID: principal.User.ID,
		ApplicationCode: payload.ApplicationCode, ApplicationName: payload.ApplicationName,
		Description: payload.Description, Environment: payload.Environment,
		PublicBaseURL: payload.PublicBaseURL, UpstreamURL: payload.UpstreamURL,
		PathPrefix: payload.PathPrefix, ClientType: payload.ClientType,
	})
	if err != nil {
		handler.writeError(writer, request, err)
		return
	}

	integration := handler.integrationResponse(result)
	writer.Header().Set("Cache-Control", "no-store, private")
	writer.Header().Set("Pragma", "no-cache")
	httpresponse.WriteSuccess(writer, request, stdhttp.StatusCreated, "子系统接入配置已创建", subsystemOnboardingResponse{
		Application: applicationToResponse(result.Application),
		Environment: environmentToResponse(result.Environment),
		LoginTarget: loginTargetToResponse(result.LoginTarget),
		OAuthClient: toOAuthClientResponse(result.OAuthClient),
		Integration: integration,
	})
}

// ListPortalApplications handles GET /api/v1/portal/applications. All authenticated users may read
// the active tenant catalog; management permissions are not required for this read-only endpoint.
func (handler *SubsystemOnboardingHandler) ListPortalApplications(writer stdhttp.ResponseWriter, request *stdhttp.Request) {
	principal, ok := subsystemPrincipal(writer, request)
	if !ok {
		return
	}
	items, err := handler.service.ListPortalApplications(request.Context(), principal.Tenant.ID, request.URL.Query().Get("environment"))
	if err != nil {
		handler.writeError(writer, request, err)
		return
	}
	responses := make([]portalApplicationResponse, 0, len(items))
	for _, item := range items {
		responses = append(responses, portalApplicationResponse{
			ApplicationID: item.ApplicationID, Code: item.Code, Name: item.Name, Description: item.Description,
			EnvironmentID: item.EnvironmentID, Environment: item.Environment, PathPrefix: item.PathPrefix,
			TargetCode: item.TargetCode, TargetURI: item.TargetURI, PublicURL: item.PublicURL,
		})
	}
	httpresponse.WriteSuccess(writer, request, stdhttp.StatusOK, "门户应用目录查询成功", responses)
}

func (handler *SubsystemOnboardingHandler) integrationResponse(result application.SubsystemOnboardingResult) subsystemIntegrationResponse {
	lines := []string{
		"OIDC_ISSUER=" + handler.oidcIssuer,
		"OIDC_CLIENT_ID=" + result.OAuthClient.ClientID,
	}
	if result.PlaintextSecret != "" {
		lines = append(lines, "OIDC_CLIENT_SECRET="+result.PlaintextSecret)
	}
	lines = append(lines,
		"OIDC_REDIRECT_URI="+result.RedirectURI,
		"OIDC_SCOPES=openid profile",
	)
	pathPrefix, upstreamURL := "", ""
	if result.Environment.PathPrefix != nil {
		pathPrefix = *result.Environment.PathPrefix
	}
	if result.Environment.UpstreamURL != nil {
		upstreamURL = *result.Environment.UpstreamURL
	}
	gatewayCommand := fmt.Sprintf("bash scripts/portal-gateway.sh add '%s' '%s' '%s' && bash scripts/portal-gateway.sh reload", result.Application.Code, pathPrefix, upstreamURL)
	return subsystemIntegrationResponse{
		Issuer: handler.oidcIssuer, ClientID: result.OAuthClient.ClientID,
		ClientSecret: result.PlaintextSecret, ClientType: result.OAuthClient.ClientType,
		TokenAuthMethod: result.OAuthClient.TokenAuthMethod, RedirectURI: result.RedirectURI,
		Scopes: "openid profile", PublicURL: result.PublicURL,
		EnvironmentFile: strings.Join(lines, "\n"), GatewayCommand: gatewayCommand,
	}
}

func subsystemPrincipal(writer stdhttp.ResponseWriter, request *stdhttp.Request) (authctx.Principal, bool) {
	principal, ok := authctx.PrincipalFromContext(request.Context())
	if !ok || strings.TrimSpace(principal.Tenant.ID) == "" || strings.TrimSpace(principal.User.ID) == "" {
		httpresponse.WriteError(writer, request, stdhttp.StatusUnauthorized, httperror.Unauthenticated)
		return authctx.Principal{}, false
	}
	return principal, true
}

func (handler *SubsystemOnboardingHandler) writeError(writer stdhttp.ResponseWriter, request *stdhttp.Request, err error) {
	switch {
	case errors.Is(err, application.ErrValidation), errors.Is(err, application.ErrManagementValidation):
		httpresponse.WriteError(writer, request, stdhttp.StatusUnprocessableEntity, httperror.Validation)
	case errors.Is(err, application.ErrNotFound), errors.Is(err, application.ErrManagementNotFound):
		httpresponse.WriteError(writer, request, stdhttp.StatusNotFound, httperror.NotFound)
	case errors.Is(err, application.ErrConflict), errors.Is(err, application.ErrVersionConflict), errors.Is(err, application.ErrManagementConflict):
		httpresponse.WriteError(writer, request, stdhttp.StatusConflict, httperror.Conflict)
	default:
		handler.logger.Error("subsystem onboarding request failed", "path", request.URL.Path, "error", err)
		httpresponse.WriteError(writer, request, stdhttp.StatusInternalServerError, httperror.Internal)
	}
}
