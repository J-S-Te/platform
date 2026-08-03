package http

import (
	"context"
	"errors"
	"log/slog"
	stdhttp "net/http"
	"os"
	"strings"
	"time"

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
	service         subsystemOnboardingService
	provisioner     application.SubsystemProvisioner
	access          subsystemInitialAccessManager
	deploymentState application.SubsystemDeploymentStateStore
	oidcIssuer      string
	logger          *slog.Logger
}

// NewSubsystemOnboardingHandler constructs the subsystem onboarding HTTP adapter. The optional
// state store keeps compatibility with lightweight callers/tests while production wiring passes
// the durable control-plane repository.
func NewSubsystemOnboardingHandler(service subsystemOnboardingService, provisioner application.SubsystemProvisioner, access subsystemInitialAccessManager, oidcIssuer string, logger *slog.Logger, stateStores ...application.SubsystemDeploymentStateStore) (*SubsystemOnboardingHandler, error) {
	if service == nil || provisioner == nil || access == nil || strings.TrimSpace(oidcIssuer) == "" {
		return nil, errors.New("subsystem onboarding service, provisioner, access manager and OIDC issuer are required")
	}
	if logger == nil {
		logger = slog.Default()
	}
	var deploymentState application.SubsystemDeploymentStateStore
	if len(stateStores) > 0 {
		deploymentState = stateStores[0]
	}
	return &SubsystemOnboardingHandler{
		service: service, provisioner: provisioner, access: access, deploymentState: deploymentState,
		oidcIssuer: strings.TrimRight(strings.TrimSpace(oidcIssuer), "/"), logger: logger,
	}, nil
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

type subsystemDeploymentStateResponse struct {
	ApplicationCode string     `json:"application_code"`
	Environment     string     `json:"environment"`
	Status          string     `json:"status"`
	Operation       string     `json:"operation"`
	Generation      uint64     `json:"generation"`
	AttemptCount    uint       `json:"attempt_count"`
	LastErrorCode   string     `json:"last_error_code,omitempty"`
	LastError       string     `json:"last_error,omitempty"`
	StartedAt       *time.Time `json:"started_at,omitempty"`
	CompletedAt     *time.Time `json:"completed_at,omitempty"`
	UpdatedAt       time.Time  `json:"updated_at"`
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
	if err := handler.provisioner.Preflight(request.Context(), application.SubsystemPreflightInput{
		TenantID: principal.Tenant.ID, ApplicationCode: onboardingInput.ApplicationCode,
		Environment: onboardingInput.Environment, Issuer: handler.oidcIssuer,
		PublicBaseURL: onboardingInput.PublicBaseURL, UpstreamURL: onboardingInput.UpstreamURL,
		PathPrefix: onboardingInput.PathPrefix, ClientType: onboardingInput.ClientType,
	}); err != nil {
		handler.writeError(writer, request, err)
		return
	}
	result, err := handler.service.OnboardSubsystem(request.Context(), onboardingInput)
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
		ServiceCredentials:           result.ServiceCredentials,
		RedirectURI:                  result.RedirectURI, PublicURL: result.PublicURL,
		PathPrefix: pathPrefix, UpstreamURL: upstreamURL,
	}); err != nil {
		handler.markDeploymentFailed(request.Context(), principal.Tenant.ID, result.Application.Code, result.Environment.Environment, "ONBOARD", "DEPLOYMENT_AGENT_FAILED", "部署 Agent 执行失败")
		handler.writeError(writer, request, err)
		return
	}
	// The application-owned role catalog is published by the deployment Agent. Assigning the
	// conventional admin role before Provision meant every new subsystem silently skipped its
	// initial administrator because the role did not exist yet.
	initialAdminUserID := strings.TrimSpace(payload.InitialAdminID)
	if initialAdminUserID == "" {
		initialAdminUserID = principal.User.ID
	}
	roleCode, err := handler.access.AssignInitialAdministrator(
		request.Context(), principal.Tenant.ID, result.Application.Code, initialAdminUserID, principal.User.ID,
	)
	if err != nil {
		handler.markDeploymentFailed(request.Context(), principal.Tenant.ID, result.Application.Code, result.Environment.Environment, "ONBOARD", "INITIAL_ACCESS_ASSIGNMENT_FAILED", "初始管理员授权失败")
		handler.writeError(writer, request, err)
		return
	}
	if err := handler.transitionDeployment(request.Context(), principal.Tenant.ID, result.Application.Code, result.Environment.Environment, application.SubsystemDeploymentStatusReady, "ONBOARD", "", ""); err != nil {
		handler.logger.Error("subsystem deployment completed but state update failed", "application_code", result.Application.Code, "environment", result.Environment.Environment, "error", err)
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
	// This response is a user-specific authorization projection. Without explicit cache controls a
	// browser or reverse proxy can serve the previous bp_session user's module list after account
	// switching, even though authentication itself already resolves the new Cookie correctly.
	writer.Header().Set("Cache-Control", "no-store, private")
	writer.Header().Set("Pragma", "no-cache")
	writer.Header().Add("Vary", "Cookie")
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
	// Resolve the identifiers before marking the deployment non-ready. The portal intentionally
	// hides UPDATING/FAILED environments, so resolving after the transition would make a normal
	// retry unable to find the application context needed by catalog synchronization.
	if applicationID, _, ok := handler.resolveApplicationContext(writer, request, applicationCode, environment); ok {
		updateInput.ApplicationID = applicationID
	}
	if publisherID, publisherSecret, ok := readCatalogPublisherCredentials(); ok {
		updateInput.CatalogPublisherClientID = publisherID
		updateInput.CatalogPublisherClientSecret = publisherSecret
	}
	operation := "UPDATE"
	if strings.HasSuffix(strings.TrimRight(request.URL.Path, "/"), "/subsystem-retry") {
		operation = "RETRY"
	}
	if err := handler.transitionDeployment(request.Context(), principal.Tenant.ID, applicationCode, environment, application.SubsystemDeploymentStatusUpdating, operation, "", ""); err != nil {
		handler.writeError(writer, request, err)
		return
	}
	if err := handler.provisioner.Update(request.Context(), updateInput); err != nil {
		handler.markDeploymentFailed(request.Context(), principal.Tenant.ID, applicationCode, environment, operation, "DEPLOYMENT_AGENT_FAILED", "部署 Agent 执行失败")
		handler.writeError(writer, request, err)
		return
	}
	if err := handler.transitionDeployment(request.Context(), principal.Tenant.ID, applicationCode, environment, application.SubsystemDeploymentStatusReady, operation, "", ""); err != nil {
		handler.logger.Error("subsystem update completed but state update failed", "application_code", applicationCode, "environment", environment, "error", err)
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
	applicationCode := strings.TrimSpace(payload.ApplicationCode)
	environment := strings.ToLower(strings.TrimSpace(payload.Environment))
	if err := handler.transitionDeployment(request.Context(), principal.Tenant.ID, applicationCode, environment, application.SubsystemDeploymentStatusDraining, "TEARDOWN", "", ""); err != nil {
		handler.writeError(writer, request, err)
		return
	}
	if err := handler.provisioner.Teardown(request.Context(), applicationCode, environment); err != nil {
		handler.markDeploymentFailed(request.Context(), principal.Tenant.ID, applicationCode, environment, "TEARDOWN", "DEPLOYMENT_AGENT_FAILED", "部署 Agent 执行失败")
		handler.writeError(writer, request, err)
		return
	}
	if err := handler.transitionDeployment(request.Context(), principal.Tenant.ID, applicationCode, environment, application.SubsystemDeploymentStatusOffboarded, "TEARDOWN", "", ""); err != nil {
		handler.logger.Error("subsystem teardown completed but state update failed", "application_code", applicationCode, "environment", environment, "error", err)
		handler.writeError(writer, request, err)
		return
	}
	writer.Header().Set("Cache-Control", "no-store, private")
	writer.Header().Set("Pragma", "no-cache")
	httpresponse.WriteSuccess(writer, request, stdhttp.StatusOK, "子系统已拆解", map[string]string{
		"status":           "torn_down",
		"application_code": applicationCode,
		"environment":      environment,
	})
	handler.logger.Warn("subsystem torn down", "path", request.URL.Path,
		"application_code", applicationCode,
		"environment", environment,
		"actor_user_id", principal.User.ID, "actor_tenant_id", principal.Tenant.ID,
	)
}

// GetSubsystemStatus handles GET /api/v1/subsystem-status. It exposes durable lifecycle
// metadata only; deployment commands, filesystem paths and credentials never cross this API.
func (handler *SubsystemOnboardingHandler) GetSubsystemStatus(writer stdhttp.ResponseWriter, request *stdhttp.Request) {
	principal, ok := subsystemPrincipal(writer, request)
	if !ok {
		return
	}
	if handler.deploymentState == nil {
		handler.writeError(writer, request, application.ErrSubsystemProvisioningUnavailable)
		return
	}
	applicationCode := strings.TrimSpace(request.URL.Query().Get("application_code"))
	environment := strings.ToLower(strings.TrimSpace(request.URL.Query().Get("environment")))
	if applicationCode == "" || environment == "" {
		handler.writeError(writer, request, application.ErrValidation)
		return
	}
	state, err := handler.deploymentState.GetSubsystemDeploymentState(request.Context(), principal.Tenant.ID, applicationCode, environment)
	if err != nil {
		handler.writeError(writer, request, err)
		return
	}
	writer.Header().Set("Cache-Control", "no-store, private")
	writer.Header().Set("Pragma", "no-cache")
	httpresponse.WriteSuccess(writer, request, stdhttp.StatusOK, "子系统部署状态查询成功", subsystemDeploymentStateResponse{
		ApplicationCode: state.ApplicationCode,
		Environment:     state.Environment,
		Status:          state.Status,
		Operation:       state.Operation,
		Generation:      state.Generation,
		AttemptCount:    state.AttemptCount,
		LastErrorCode:   state.LastErrorCode,
		LastError:       state.LastError,
		StartedAt:       state.StartedAt,
		CompletedAt:     state.CompletedAt,
		UpdatedAt:       state.UpdatedAt,
	})
}

func (handler *SubsystemOnboardingHandler) transitionDeployment(ctx context.Context, tenantID, applicationCode, environment, status, operation, errorCode, errorMessage string) error {
	if handler.deploymentState == nil {
		return nil
	}
	return handler.deploymentState.TransitionSubsystemDeployment(ctx, tenantID, applicationCode, environment, status, operation, errorCode, errorMessage, time.Now().UTC())
}

func (handler *SubsystemOnboardingHandler) markDeploymentFailed(ctx context.Context, tenantID, applicationCode, environment, operation, errorCode, errorMessage string) {
	if err := handler.transitionDeployment(ctx, tenantID, applicationCode, environment, application.SubsystemDeploymentStatusFailed, operation, errorCode, errorMessage); err != nil {
		handler.logger.Error("failed to persist subsystem deployment failure", "application_code", applicationCode, "environment", environment, "error", err)
	}
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
				"next_action":      "该环境的控制面记录已存在。子系统日常代码、镜像、功能模块和业务迁移发布无需执行接入或撤销脚本；仅基础设施入口参数变更时使用环境、登录目标或 OAuth 客户端的受控更新接口。若此前部署 Agent 失败，请先查询 subsystem-status，再使用 subsystem-retry，不要重复 onboard",
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
		message := httperror.DependencyUnavailable.Message
		if strings.Contains(strings.ToLower(err.Error()), "disabled") {
			message = "当前部署未启用受控部署 Agent，无法在平台内完成一键接入"
		}
		httpresponse.WriteError(writer, request, stdhttp.StatusServiceUnavailable, httperror.New(
			httperror.DependencyUnavailable.Code,
			message,
			map[string]string{"next_action": subsystemProvisioningNextAction(err)},
		))
	default:
		handler.logger.Error("subsystem onboarding request failed", "path", request.URL.Path, "error", err)
		httpresponse.WriteError(writer, request, stdhttp.StatusInternalServerError, httperror.Internal)
	}
}

func subsystemProvisioningNextAction(err error) string {
	message := strings.ToLower(err.Error())
	switch {
	case strings.Contains(message, "disabled"):
		return "请升级并重新发布当前环境的生产部署资产，确认 platform-api 与 subsystem-provisioner 均健康后在本页面重试；不要手工复制 OAuth Secret 或重复创建环境"
	case strings.Contains(message, "tenant is not allowed"):
		return "当前租户不是该服务器合同实例绑定的生产租户；请核对服务器部署配置中的允许租户 ID，禁止用其他租户覆盖现有合同实例"
	case strings.Contains(message, "preflight values are inconsistent"):
		return "生产一键接入目前只支持合同管理系统 prod、confidential 客户端和页面自动填充的固定地址；请恢复预设后重试"
	case strings.Contains(message, "infrastructure secrets are incomplete"):
		return "服务器基础设施密钥仍为空或占位值；请先由部署管理员完成生产平台初始化，接入页面不会自动改动数据库和 IAM 密钥"
	case strings.Contains(message, "immutable digest"):
		return "服务器尚未发布合同管理的不可变镜像 digest；请先完成合同镜像发布，再回到本页面接入"
	case strings.Contains(message, "compose file"):
		return "部署 Agent 未找到子系统 Compose；内置客户与商机系统请更新平台代码并重启 api 与 subsystem-provisioner，独立子系统请在同名项目目录提供 compose.yaml"
	case strings.Contains(message, "environment template"):
		return "部署 Agent 未找到运行配置模板；请提供 .env.example，内置客户与商机系统请检查 platform/docker/.env.customer.local"
	case strings.Contains(message, "docker service"):
		return "Docker 服务不可用；请启动 Docker 后重试"
	case strings.Contains(message, "start subsystem containers"), strings.Contains(message, "rebuild subsystem containers"):
		return "子系统构建或启动失败；请查看 subsystem-provisioner 与目标 API 容器日志，修复后使用“重试部署”"
	default:
		return "请执行 docker-local.sh ps，并查看 api、subsystem-provisioner 与目标 API 容器日志；修复后使用“重试部署”，不要重复创建应用环境"
	}
}
