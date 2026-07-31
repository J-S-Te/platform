package http

import (
	"context"
	"errors"
	"log/slog"
	stdhttp "net/http"
	"os"
	"strings"

	"github.com/J-S-Te/Basic-Platform/backend/internal/platform/applicationregistry/application"
	"github.com/J-S-Te/Basic-Platform/backend/internal/shared/authctx"
	"github.com/J-S-Te/Basic-Platform/backend/internal/shared/httperror"
	"github.com/J-S-Te/Basic-Platform/backend/internal/shared/httpresponse"
)

// readCatalogPublisherCredentials returns the long-lived catalog-publisher OAuth client
// credentials that the platform operator has provisioned for the contract_management
// subsystem. The credentials live in the platform's own environment so that the API container
// (which intentionally has no access to the subsystem's .env.local on disk) can hand them
// to the deployment helper for the post-rebuild catalog sync.
func readCatalogPublisherCredentials() (string, string, bool) {
	id := strings.TrimSpace(os.Getenv("CONTRACT_CATALOG_PUBLISHER_CLIENT_ID"))
	secret := strings.TrimSpace(os.Getenv("CONTRACT_CATALOG_PUBLISHER_CLIENT_SECRET"))
	if id == "" || secret == "" {
		return "", "", false
	}
	return id, secret, true
}

// resolveApplicationContext looks up the (application, environment) identifiers for a
// (code, environment-name) pair. The post-rebuild catalog sync needs the platform-side primary
// keys, not the human-readable codes, to address the right application. The lookup piggybacks
// on the existing portal listing path so no new repository method is needed.
func (handler *SubsystemOnboardingHandler) resolveApplicationContext(writer stdhttp.ResponseWriter, request *stdhttp.Request, applicationCode, environment string) (string, string, bool) {
	principal, ok := authctx.PrincipalFromContext(request.Context())
	if !ok {
		return "", "", false
	}
	items, err := handler.service.ListPortalApplications(request.Context(), principal.Tenant.ID, principal.User.ID, environment)
	if err != nil {
		return "", "", false
	}
	for _, item := range items {
		if strings.EqualFold(strings.TrimSpace(item.Code), applicationCode) &&
			strings.EqualFold(strings.TrimSpace(item.Environment), environment) {
			return strings.TrimSpace(item.ApplicationID), strings.TrimSpace(item.EnvironmentID), true
		}
	}
	return "", "", false
}

type subsystemOnboardingService interface {
	OnboardSubsystem(context.Context, application.SubsystemOnboardingInput) (application.SubsystemOnboardingResult, error)
	ListPortalApplications(context.Context, string, string, string) ([]application.PortalApplication, error)
}

// subsystemInitialAccessManager keeps subsystem-specific authorization outside the application
// registry. An empty role code means the registered subsystem has no managed role catalog yet.
type subsystemInitialAccessManager interface {
	AssignInitialAdministrator(context.Context, string, string, string, string) (string, error)
}

// SubsystemOnboardingHandler exposes the simplified onboarding workflow and authenticated portal
// catalog. The configured OIDC issuer is used only by the isolated deployment workflow and is not
// returned to the browser together with generated credentials or infrastructure commands.
type SubsystemOnboardingHandler struct {
	service     subsystemOnboardingService
	provisioner application.SubsystemProvisioner
	access      subsystemInitialAccessManager
	oidcIssuer  string
	logger      *slog.Logger
}

// NewSubsystemOnboardingHandler constructs the subsystem onboarding HTTP adapter.
func NewSubsystemOnboardingHandler(service subsystemOnboardingService, provisioner application.SubsystemProvisioner, access subsystemInitialAccessManager, oidcIssuer string, logger *slog.Logger) (*SubsystemOnboardingHandler, error) {
	if service == nil || provisioner == nil || access == nil || strings.TrimSpace(oidcIssuer) == "" {
		return nil, errors.New("subsystem onboarding service, provisioner, access manager and OIDC issuer are required")
	}
	if logger == nil {
		logger = slog.Default()
	}
	return &SubsystemOnboardingHandler{service: service, provisioner: provisioner, access: access, oidcIssuer: strings.TrimRight(strings.TrimSpace(oidcIssuer), "/"), logger: logger}, nil
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
	InitialAdminID  string  `json:"initial_admin_user_id"`
}

// subsystemLifecycleRequest is the shared payload for Update and Teardown. Both endpoints
// only need the application code + environment to find the existing DB row; any value
// changes (BaseURL, UpstreamURL, PathPrefix, OAuth client) must be PATCHed via the regular
// management endpoints first, then `update` re-provisions the running subsystem.
type subsystemLifecycleRequest struct {
	ApplicationCode string `json:"application_code"`
	Environment     string `json:"environment"`
}

type subsystemAutomationResponse struct {
	Status    string `json:"status"`
	PublicURL string `json:"public_url"`
}

type subsystemOnboardingResponse struct {
	Application   applicationResponse             `json:"application"`
	Environment   environmentResponse             `json:"environment"`
	LoginTarget   loginTargetResponse             `json:"login_target"`
	OAuthClient   oauthClientResponse             `json:"oauth_client"`
	Automation    subsystemAutomationResponse     `json:"automation"`
	Authorization *subsystemAuthorizationResponse `json:"authorization,omitempty"`
}

type subsystemAuthorizationResponse struct {
	InitialAdminUserID string `json:"initial_admin_user_id"`
	RoleCode           string `json:"role_code"`
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
	onboardingInput := application.SubsystemOnboardingInput{
		TenantID: principal.Tenant.ID, OperatorID: principal.User.ID,
		ApplicationCode: payload.ApplicationCode, ApplicationName: payload.ApplicationName,
		Description: payload.Description, Environment: payload.Environment,
		PublicBaseURL: payload.PublicBaseURL, UpstreamURL: payload.UpstreamURL,
		PathPrefix: payload.PathPrefix, ClientType: payload.ClientType,
	}
	if err := application.ValidateSubsystemOnboardingInput(onboardingInput); err != nil {
		handler.writeError(writer, request, err)
		return
	}
	if err := handler.provisioner.Preflight(request.Context(), onboardingInput.ApplicationCode); err != nil {
		handler.writeError(writer, request, err)
		return
	}
	result, err := handler.service.OnboardSubsystem(request.Context(), onboardingInput)
	if err != nil {
		handler.writeError(writer, request, err)
		return
	}
	initialAdminUserID := strings.TrimSpace(payload.InitialAdminID)
	if initialAdminUserID == "" {
		initialAdminUserID = principal.User.ID
	}
	roleCode, err := handler.access.AssignInitialAdministrator(
		request.Context(), principal.Tenant.ID, result.Application.Code, initialAdminUserID, principal.User.ID,
	)
	if err != nil {
		handler.writeError(writer, request, err)
		return
	}
	pathPrefix, upstreamURL := "", ""
	if result.Environment.PathPrefix != nil {
		pathPrefix = *result.Environment.PathPrefix
	}
	if result.Environment.UpstreamURL != nil {
		upstreamURL = *result.Environment.UpstreamURL
	}
	if err := handler.provisioner.Provision(request.Context(), application.SubsystemProvisioningInput{
		TenantID: principal.Tenant.ID, ApplicationID: result.Application.ID, ApplicationCode: result.Application.Code,
		Environment: result.Environment.Environment, Issuer: handler.oidcIssuer,
		ClientID: result.OAuthClient.ClientID, ClientSecret: result.PlaintextSecret,
		CatalogPublisherClientID:     result.CatalogPublisherOAuthClient.ClientID,
		CatalogPublisherClientSecret: result.CatalogPublisherPlaintextSecret,
		RedirectURI:                  result.RedirectURI, PublicURL: result.PublicURL,
		PathPrefix: pathPrefix, UpstreamURL: upstreamURL,
	}); err != nil {
		handler.writeError(writer, request, err)
		return
	}
	writer.Header().Set("Cache-Control", "no-store, private")
	writer.Header().Set("Pragma", "no-cache")
	response := subsystemOnboardingResponse{
		Application: applicationToResponse(result.Application),
		Environment: environmentToResponse(result.Environment),
		LoginTarget: loginTargetToResponse(result.LoginTarget),
		OAuthClient: toOAuthClientResponse(result.OAuthClient),
		Automation:  subsystemAutomationResponse{Status: "completed", PublicURL: result.PublicURL},
	}
	if roleCode != "" {
		response.Authorization = &subsystemAuthorizationResponse{InitialAdminUserID: initialAdminUserID, RoleCode: roleCode}
	}
	httpresponse.WriteSuccess(writer, request, stdhttp.StatusCreated, "子系统已完成自动接入和部署", response)
}

// ListPortalApplications handles GET /api/v1/portal/applications. All authenticated users may read
// the active tenant catalog; management permissions are not required for this read-only endpoint.
func (handler *SubsystemOnboardingHandler) ListPortalApplications(writer stdhttp.ResponseWriter, request *stdhttp.Request) {
	principal, ok := subsystemPrincipal(writer, request)
	if !ok {
		return
	}
	items, err := handler.service.ListPortalApplications(request.Context(), principal.Tenant.ID, principal.User.ID, request.URL.Query().Get("environment"))
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

// UpdateSubsystem handles POST /api/v1/subsystem-update. The handler assumes the caller has
// already updated any DB aggregate (Environment base URL, OAuth redirect URI) via the regular
// management PATCH endpoints. This endpoint only re-runs the provisioner so the running subsystem
// picks up the new .env.local values and the portal gateway is reloaded.
func (handler *SubsystemOnboardingHandler) UpdateSubsystem(writer stdhttp.ResponseWriter, request *stdhttp.Request) {
	principal, ok := subsystemPrincipal(writer, request)
	if !ok {
		return
	}
	var payload subsystemLifecycleRequest
	if !decodeApplicationManagementJSON(writer, request, &payload) {
		return
	}
	if err := validateLifecycleRequest(payload); err != nil {
		handler.writeError(writer, request, err)
		return
	}
	applicationCode := strings.TrimSpace(payload.ApplicationCode)
	environment := strings.ToLower(strings.TrimSpace(payload.Environment))
	// Update only re-runs the rebuild path: it never re-issues the OAuth client secret, so we
	// deliberately send a minimal SubsystemProvisioningInput with just the identifiers the
	// provisioner needs to locate the project directory and compose stack.
	//
	// The catalog-publisher client is not re-issued either. The post-rebuild catalog sync (if
	// enabled) needs the long-lived publisher credentials to authenticate against the platform.
	// Those credentials are read from the platform operator's environment, not from the
	// subsystem's .env.local, so both the API process and the deployment helper can pick them
	// up without exposing the host filesystem into the API container.
	updateInput := application.SubsystemProvisioningInput{
		ApplicationCode: applicationCode,
		Environment:     environment,
		// Issuer is required by the post-rebuild catalog sync (it issues the PUT against
		// the platform's /authorization-catalog endpoint). Use the platform-configured OIDC
		// issuer as a stable source of truth instead of having the client send it in.
		Issuer: handler.oidcIssuer,
	}
	if applicationID, _, ok := handler.resolveApplicationContext(writer, request, applicationCode, environment); ok {
		updateInput.ApplicationID = applicationID
	}
	if publisherID, publisherSecret, ok := readCatalogPublisherCredentials(); ok {
		updateInput.CatalogPublisherClientID = publisherID
		updateInput.CatalogPublisherClientSecret = publisherSecret
	}
	if err := handler.provisioner.Update(request.Context(), updateInput); err != nil {
		handler.writeError(writer, request, err)
		return
	}
	writer.Header().Set("Cache-Control", "no-store, private")
	writer.Header().Set("Pragma", "no-cache")
	httpresponse.WriteSuccess(writer, request, stdhttp.StatusOK, "子系统已重新部署", subsystemAutomationResponse{
		Status:    "reapplied",
		PublicURL: "",
	})
	handler.logger.Info("subsystem re-provisioned", "path", request.URL.Path,
		"application_code", applicationCode, "environment", environment,
		"actor_user_id", principal.User.ID, "actor_tenant_id", principal.Tenant.ID,
	)
}

// TeardownSubsystem handles POST /api/v1/subsystem-teardown. Stops containers, removes
// .env.local, drops the portal gateway include, and reloads nginx. The HTTP layer does not
// delete the corresponding DB rows here: the script follows up with DELETE /environments and
// (optionally) DELETE /applications so the audit trail preserves each cleanup step.
func (handler *SubsystemOnboardingHandler) TeardownSubsystem(writer stdhttp.ResponseWriter, request *stdhttp.Request) {
	principal, ok := subsystemPrincipal(writer, request)
	if !ok {
		return
	}
	var payload subsystemLifecycleRequest
	if !decodeApplicationManagementJSON(writer, request, &payload) {
		return
	}
	if err := validateLifecycleRequest(payload); err != nil {
		handler.writeError(writer, request, err)
		return
	}
	if err := handler.provisioner.Teardown(request.Context(), strings.TrimSpace(payload.ApplicationCode), strings.TrimSpace(payload.Environment)); err != nil {
		handler.writeError(writer, request, err)
		return
	}
	writer.Header().Set("Cache-Control", "no-store, private")
	writer.Header().Set("Pragma", "no-cache")
	httpresponse.WriteSuccess(writer, request, stdhttp.StatusOK, "子系统已拆解", map[string]string{
		"status":          "torn_down",
		"application_code": strings.TrimSpace(payload.ApplicationCode),
		"environment":     strings.TrimSpace(payload.Environment),
	})
	handler.logger.Warn("subsystem torn down", "path", request.URL.Path,
		"application_code", strings.TrimSpace(payload.ApplicationCode),
		"environment", strings.TrimSpace(payload.Environment),
		"actor_user_id", principal.User.ID, "actor_tenant_id", principal.Tenant.ID,
	)
}

func validateLifecycleRequest(payload subsystemLifecycleRequest) error {
	if strings.TrimSpace(payload.ApplicationCode) == "" {
		return application.ErrValidation
	}
	if strings.TrimSpace(payload.Environment) == "" {
		return application.ErrValidation
	}
	return nil
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
	var onboardingConflict *application.SubsystemOnboardingConflict
	switch {
	case errors.As(err, &onboardingConflict):
		handler.logger.Warn("subsystem onboarding skipped because environment already exists",
			"path", request.URL.Path,
			"application_code", onboardingConflict.ApplicationCode,
			"environment", onboardingConflict.Environment,
			"environment_status", onboardingConflict.Status,
		)
		httpresponse.WriteError(writer, request, stdhttp.StatusConflict, httperror.New(
			"IAM_SUBSYSTEM_ALREADY_ONBOARDED",
			"该应用环境已存在；接入脚本不会覆盖已有登录目标或 OAuth 客户端",
			map[string]string{
				"application_code": onboardingConflict.ApplicationCode,
				"environment":      onboardingConflict.Environment,
				"status":           onboardingConflict.Status,
				"next_action":      "该环境已完成接入。子系统日常代码、镜像、功能模块和业务迁移发布无需执行接入或撤销脚本；仅基础设施入口参数变更时使用环境、登录目标或 OAuth 客户端的受控更新接口",
			},
		))
	case errors.Is(err, application.ErrValidation), errors.Is(err, application.ErrManagementValidation):
		httpresponse.WriteError(writer, request, stdhttp.StatusUnprocessableEntity, httperror.Validation)
	case errors.Is(err, application.ErrNotFound), errors.Is(err, application.ErrManagementNotFound):
		httpresponse.WriteError(writer, request, stdhttp.StatusNotFound, httperror.NotFound)
	case errors.Is(err, application.ErrConflict), errors.Is(err, application.ErrVersionConflict), errors.Is(err, application.ErrManagementConflict):
		httpresponse.WriteError(writer, request, stdhttp.StatusConflict, httperror.Conflict)
	case errors.Is(err, application.ErrSubsystemProvisioningUnavailable):
		handler.logger.Error("subsystem automatic provisioning failed", "path", request.URL.Path, "error", err)
		httpresponse.WriteError(writer, request, stdhttp.StatusServiceUnavailable, httperror.DependencyUnavailable)
	default:
		handler.logger.Error("subsystem onboarding request failed", "path", request.URL.Path, "error", err)
		httpresponse.WriteError(writer, request, stdhttp.StatusInternalServerError, httperror.Internal)
	}
}
