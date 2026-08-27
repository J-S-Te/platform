package http

import (
	"context"
	"errors"
	"log/slog"
	stdhttp "net/http"
	"os"
	"strings"
	"time"

	"github.com/J-S-Te/Basic-Platform/internal/platform/applicationregistry/application"
	projectionapplication "github.com/J-S-Te/Basic-Platform/internal/platform/keycloakauthorization/application"
	"github.com/J-S-Te/Basic-Platform/internal/shared/authctx"
	"github.com/J-S-Te/Basic-Platform/internal/shared/httperror"
	"github.com/J-S-Te/Basic-Platform/internal/shared/httpresponse"
	"github.com/J-S-Te/Basic-Platform/internal/shared/keycloakctx"
	"github.com/J-S-Te/Basic-Platform/internal/shared/requestctx"
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

func optionalIssuerAlias(value string) *string {
	value = strings.ToLower(strings.TrimSpace(value))
	if value == "" || value == "platform" || value == "basic_platform" {
		return nil
	}
	return &value
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
	return handler.resolveApplicationContextForIdentity(request, principal.Tenant.ID, principal.User.ID, applicationCode, environment)
}

func (handler *SubsystemOnboardingHandler) resolveApplicationContextForIdentity(request *stdhttp.Request, tenantID, identityID, applicationCode, environment string) (string, string, bool) {
	if applicationID, environmentID, err := handler.service.ResolveApplicationEnvironment(request.Context(), tenantID, applicationCode, environment); err == nil {
		return applicationID, environmentID, true
	}
	items, err := handler.service.ListPortalApplications(request.Context(), tenantID, identityID, environment)
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
	RegisterSubsystemDirectory(context.Context, application.SubsystemDirectoryRegistrationInput) (application.SubsystemDirectoryRegistrationResult, error)
	ListPortalApplications(context.Context, string, string, string) ([]application.PortalApplication, error)
	ResolveApplicationEnvironment(context.Context, string, string, string) (string, string, error)
	PreflightValidate(context.Context, application.SubsystemOnboardingInput) error
}

// subsystemEnvironmentIssuerResolver is an optional capability implemented by
// the durable control-plane repository. It lets UpdateSubsystem tell a real
// issuer cutover apart from a rebuild of an environment already using the
// requested provider, while lightweight test services remain compatible.
type subsystemEnvironmentIssuerResolver interface {
	ResolveEnvironmentIssuerAlias(context.Context, string, string, string) (string, error)
}

type keycloakAuthorizationCatalog interface {
	ListKeycloakRoleCodes(context.Context, string, string) ([]string, error)
}

type keycloakClientMappingStore interface {
	SaveKeycloakClientMapping(context.Context, string, string, string, string, string) error
	BackfillKeycloakAuthorization(context.Context, string, string, string) error
}

// keycloakClientCompatibilityResolver is an optional persistence capability used
// during onboarding retries. It preserves a previously registered browser Client
// ID when the environment label was later normalized (for example, an existing
// contract_management-prod-web used by the dev deployment). The resolver is
// tenant/application/environment scoped and never guesses by replacing prod/dev
// strings.
type keycloakClientCompatibilityResolver interface {
	ResolveEffectiveKeycloakClient(context.Context, string, string, string, string) (KeycloakClientResolution, error)
}

type KeycloakClientResolution struct {
	ClientID            string
	CanonicalClientID   string
	Source              string
	LegacyCompatibility bool
}

// KeycloakClientMapping is the deliberately non-sensitive projection of one
// environment's Realm Client.  It lets the authentication page survive a
// refresh without asking the browser to retain a Client secret or infer state
// from a successful sync response.
type KeycloakClientMapping struct {
	Realm        string     `json:"realm,omitempty"`
	ClientID     string     `json:"client_id,omitempty"`
	Status       string     `json:"status"`
	LastSyncedAt *time.Time `json:"last_synced_at,omitempty"`
	Exists       bool       `json:"exists"`
}

// keycloakClientMappingInspector is intentionally optional so older test and
// deployment adapters retain compatibility.  Absence is reported as an
// unconfigured mapping; it never opens a cutover gate.
type keycloakClientMappingInspector interface {
	GetKeycloakClientMapping(context.Context, string, string, string) (KeycloakClientMapping, error)
}

// KeycloakSwitchGate is a non-sensitive, auditable cutover prerequisite.  A
// false value means either that the prerequisite failed or that this server
// cannot independently verify it; clients must treat both cases as blocked.
type KeycloakSwitchGate struct {
	Key        string `json:"key"`
	Label      string `json:"label"`
	Passed     bool   `json:"passed"`
	Detail     string `json:"detail"`
	NextAction string `json:"next_action,omitempty"`
}

// KeycloakSwitchReadiness is intentionally supplied by a separately owned
// verifier.  The application registry must never infer a brokered login from
// an Admin API write or a browser assertion.
type KeycloakSwitchReadiness struct {
	Gates       []KeycloakSwitchGate `json:"switch_gates"`
	SwitchReady bool                 `json:"switch_ready"`
	NextAction  string               `json:"next_action,omitempty"`
}

type keycloakSwitchReadinessInspector interface {
	InspectKeycloakSwitchReadiness(context.Context, string, string, string) (KeycloakSwitchReadiness, error)
}

// keycloakSwitchReadinessUpdater is deliberately narrower than the inspector:
// only server-side successful synchronization may open its first two gates.
type keycloakSwitchReadinessUpdater interface {
	MarkKeycloakClientAndRoleCatalogSynced(context.Context, string, string, string) error
}

// keycloakBrokerLoginVerificationRecorder persists evidence supplied by the
// authenticated current-session verification endpoint.
type keycloakBrokerLoginVerificationRecorder interface {
	RecordBrokerLoginVerification(context.Context, KeycloakBrokerLoginVerification) error
}

// keycloakProjectionOperations keeps dead-letter management behind the
// Keycloak authorization application service.  It is configured only in the
// composition root and has no access to secrets, Keycloak Admin credentials or
// arbitrary queue mutation methods.
type keycloakProjectionOperations interface {
	ListFailed(context.Context, string, projectionapplication.FailurePageRequest) (projectionapplication.FailurePageResult, error)
	AlertStatus(context.Context, string) (projectionapplication.AlertStatus, error)
	Replay(context.Context, projectionapplication.ReplayInput) (projectionapplication.ReplayResult, error)
}

type KeycloakBrokerLoginVerification struct {
	TenantID, ApplicationID, EnvironmentID string
	IdentityID, Issuer, ClientID           string
	VerifiedByID, SessionID                string
}

type subsystemProvisioningCapabilityProvider interface {
	Capabilities() application.SubsystemProvisioningCapabilities
}

type subsystemCandidateDiscoverer interface {
	DiscoverSubsystemCandidates(context.Context) ([]application.SubsystemDiscoveryCandidate, error)
}

// subsystemInitialAccessManager keeps subsystem-specific authorization outside the application
// registry. An empty role code means the registered subsystem has no managed role catalog yet.
type subsystemInitialAccessManager interface {
	AssignInitialAdministrator(context.Context, string, string, string, string) (string, error)
}

// keycloakBrokerProvisioner creates the one dedicated platform OAuth client
// used by Keycloak as an upstream broker. Its secret never reaches HTTP.
type keycloakBrokerProvisioner interface {
	EnsureKeycloakBroker(context.Context, string) (string, string, error)
}

// subsystemServiceCredentialManager is the narrow control-plane capability used to
// backfill newly introduced machine bindings for an already registered environment.
// Plaintext is produced only for a newly created/rotated secret and is passed directly
// to the isolated deployment Agent; it is never returned to the browser.
type subsystemServiceCredentialManager interface {
	ListOAuthClients(context.Context, string) ([]application.OAuthClientView, error)
	CreateOAuthClient(context.Context, application.OAuthClientCreateInput) (application.OAuthClientCreateResult, error)
	CreateOAuthClientSecret(context.Context, application.OAuthClientSecretCreateInput) (application.OAuthClientSecretResult, error)
}

// subsystemNotificationSink 发送租户内站内通知，用于把子系统接入生命周期结果通知给操作人。
// 该接口由基础平台实现，子系统代码无需改动；nil 时处理器保持轻量测试可用。
type subsystemNotificationSink interface {
	SendSubsystemLifecycle(ctx context.Context, input SubsystemLifecycleNotification) error
}

// SubsystemLifecycleNotification 描述一次子系统接入/重试/更新的结果通知，供装配层适配器使用。
type SubsystemLifecycleNotification struct {
	TenantID        string
	OperatorID      string
	ApplicationName string
	ApplicationCode string
	Environment     string
	Succeeded       bool
	Detail          string
}

// SubsystemOnboardingHandler exposes the simplified onboarding workflow and authenticated portal
// catalog. The configured OIDC issuer is used only by the isolated deployment workflow and is not
// returned to the browser together with generated credentials or infrastructure commands.
type SubsystemOnboardingHandler struct {
	service              subsystemOnboardingService
	provisioner          application.SubsystemProvisioner
	access               subsystemInitialAccessManager
	deploymentState      application.SubsystemDeploymentStateStore
	notifications        subsystemNotificationSink
	oidcIssuer           string
	keycloakIssuer       string
	keycloakRealm        string
	keycloakEnabled      bool
	keycloakRequireHTTPS bool
	defaultIssuerAlias   string
	keycloakControl      *keycloakControlPlane
	keycloakBroker       keycloakBrokerProvisioner
	keycloakCatalog      keycloakAuthorizationCatalog
	keycloakMappings     keycloakClientMappingStore
	keycloakReadiness    keycloakSwitchReadinessInspector
	keycloakOperations   keycloakProjectionOperations
	keycloakCutover      keycloakCutoverLifecycleStore
	serviceCredentials   subsystemServiceCredentialManager
	logger               *slog.Logger
}

const (
	keycloakObservationWindow = 7 * 24 * time.Hour
	keycloakRollbackWindow    = 7 * 24 * time.Hour
)

// ConfigureKeycloak enables Keycloak as an explicit deployment provider.  The
// credentials remain solely between the API and the deployment Agent; this
// object intentionally carries only public status information.
func (handler *SubsystemOnboardingHandler) ConfigureKeycloak(enabled bool, publicIssuer, realm string) {
	handler.keycloakEnabled = enabled
	handler.keycloakIssuer = strings.TrimRight(strings.TrimSpace(publicIssuer), "/")
	handler.keycloakRealm = strings.TrimSpace(realm)
}

// ConfigureKeycloakTransportPolicy controls whether a future cutover must use
// HTTPS.  False is intentionally compatible with the current HTTP deployment;
// the preflight still derives Redirect URI and Secure Cookie from the public
// origin so the three settings cannot drift.
func (handler *SubsystemOnboardingHandler) ConfigureKeycloakTransportPolicy(requireHTTPS bool) {
	handler.keycloakRequireHTTPS = requireHTTPS
}

// ConfigureDefaultIssuerAlias controls the provider used when the onboarding
// request omits issuer_alias. Explicit user choices always take precedence.
func (handler *SubsystemOnboardingHandler) ConfigureDefaultIssuerAlias(alias string) {
	handler.defaultIssuerAlias = strings.ToLower(strings.TrimSpace(alias))
}

// ConfigureKeycloakControlPlane is called only from the composition root.  The
// browser never receives this object or its Keycloak administrative credentials.
func (handler *SubsystemOnboardingHandler) ConfigureKeycloakControlPlane(control *keycloakControlPlane) {
	handler.keycloakControl = control
}

func (handler *SubsystemOnboardingHandler) ConfigureKeycloakBroker(provisioner keycloakBrokerProvisioner) {
	handler.keycloakBroker = provisioner
}

func (handler *SubsystemOnboardingHandler) ConfigureKeycloakAuthorizationCatalog(catalog keycloakAuthorizationCatalog) {
	handler.keycloakCatalog = catalog
}

func (handler *SubsystemOnboardingHandler) ConfigureKeycloakClientMappingStore(store keycloakClientMappingStore) {
	handler.keycloakMappings = store
}

// ConfigureKeycloakSwitchReadinessInspector attaches an authoritative,
// server-side source of projection and broker-login evidence.  It is optional
// for backward compatibility; without it every Keycloak cutover stays closed.
func (handler *SubsystemOnboardingHandler) ConfigureKeycloakSwitchReadinessInspector(inspector keycloakSwitchReadinessInspector) {
	handler.keycloakReadiness = inspector
}

// ConfigureKeycloakProjectionOperations enables tenant-scoped inspection and
// controlled replay of FAILED projection records. A nil value intentionally
// leaves the operations endpoints unavailable rather than exposing a partial
// recovery path.
func (handler *SubsystemOnboardingHandler) ConfigureKeycloakProjectionOperations(operations keycloakProjectionOperations) {
	handler.keycloakOperations = operations
}

// ConfigureKeycloakCutoverLifecycle attaches the durable, per-environment
// seven-day observation and rollback-window control plane.
func (handler *SubsystemOnboardingHandler) ConfigureKeycloakCutoverLifecycle(store keycloakCutoverLifecycleStore) {
	handler.keycloakCutover = store
}

// ConfigureSubsystemServiceCredentials enables idempotent migration of new
// purpose-bound machine credentials for environments created by an older release.
func (handler *SubsystemOnboardingHandler) ConfigureSubsystemServiceCredentials(manager subsystemServiceCredentialManager) {
	handler.serviceCredentials = manager
}

func unverifiedKeycloakSwitchReadiness() KeycloakSwitchReadiness {
	return KeycloakSwitchReadiness{Gates: []KeycloakSwitchGate{
		{Key: "client_ready", Label: "Client 已就绪", Detail: "尚未针对当前应用环境验证 Realm Client。", NextAction: "先执行“同步 Keycloak”。"},
		{Key: "role_catalog_synced", Label: "角色目录已同步", Detail: "尚未针对当前应用环境验证角色目录。", NextAction: "同步 Keycloak 后检查角色目录。"},
		{Key: "user_projection_completed", Label: "用户投影已完成", Detail: "服务器没有可审计的用户投影完成证据。", NextAction: "完成用户投影，并接入服务器端状态回传。"},
		{Key: "broker_login_verified", Label: "Broker 登录验证已通过", Detail: "服务器没有可审计的 Broker 登录验证证据。", NextAction: "使用目标应用完成一次 Broker 登录验证，并接入服务器端状态回传。"},
	}, NextAction: "四项门禁均需由服务器验证后才能切换 Issuer。"}
}

func keycloakSyncReadiness() KeycloakSwitchReadiness {
	readiness := unverifiedKeycloakSwitchReadiness()
	readiness.Gates[0] = KeycloakSwitchGate{Key: "client_ready", Label: "Client 已就绪", Passed: true, Detail: "本次同步已成功创建或更新 Realm Client。"}
	readiness.Gates[1] = KeycloakSwitchGate{Key: "role_catalog_synced", Label: "角色目录已同步", Passed: true, Detail: "本次同步已成功写入当前应用角色目录。"}
	return readiness
}

func (handler *SubsystemOnboardingHandler) keycloakSwitchReadiness(ctx context.Context, tenantID, applicationCode, environment string) KeycloakSwitchReadiness {
	if handler.keycloakReadiness == nil {
		return unverifiedKeycloakSwitchReadiness()
	}
	readiness, err := handler.keycloakReadiness.InspectKeycloakSwitchReadiness(ctx, tenantID, applicationCode, environment)
	if err != nil || len(readiness.Gates) == 0 {
		return unverifiedKeycloakSwitchReadiness()
	}
	required := map[string]bool{"client_ready": false, "role_catalog_synced": false, "user_projection_completed": false, "broker_login_verified": false}
	for _, gate := range readiness.Gates {
		if _, requiredGate := required[gate.Key]; requiredGate {
			required[gate.Key] = gate.Passed
		}
		if !gate.Passed {
			readiness.SwitchReady = false
		}
	}
	for _, passed := range required {
		if !passed {
			readiness.SwitchReady = false
			return readiness
		}
	}
	readiness.SwitchReady = true
	return readiness
}

func writeKeycloakSwitchBlocked(writer stdhttp.ResponseWriter, request *stdhttp.Request, readiness KeycloakSwitchReadiness) {
	httpresponse.WriteError(writer, request, stdhttp.StatusConflict, httperror.New(
		"IAM_AUTH_PROVIDER_SWITCH_NOT_READY",
		"认证提供方尚未满足切换门禁，Issuer 未下发到运行时",
		map[string]any{"switch_gates": readiness.Gates, "next_action": readiness.NextAction},
	))
}

func writeKeycloakObservationBlocked(writer stdhttp.ResponseWriter, request *stdhttp.Request, reason string) {
	httpresponse.WriteError(writer, request, stdhttp.StatusConflict, httperror.New(
		"IAM_AUTH_PROVIDER_SWITCH_OBSERVATION_REQUIRED",
		"认证提供方尚未完成七天观察期，Issuer 未下发到运行时",
		map[string]any{"reason": strings.TrimSpace(reason), "observation_window_days": 7, "next_action": "在 Keycloak 认证接入中发起观察；观察期完成后再切换。"},
	))
}

func (handler *SubsystemOnboardingHandler) keycloakCutoverRequired(ctx context.Context, tenantID, applicationCode, environment string) bool {
	// The onboarding service owns the environment aggregate in normal
	// composition.  Keep the state-store fallback for older compositions that
	// expose this optional lookup there instead.
	for _, candidate := range []any{handler.service, handler.deploymentState} {
		resolver, ok := candidate.(subsystemEnvironmentIssuerResolver)
		if !ok {
			continue
		}
		currentAlias, err := resolver.ResolveEnvironmentIssuerAlias(ctx, tenantID, applicationCode, environment)
		if err == nil {
			return !strings.EqualFold(strings.TrimSpace(currentAlias), "keycloak")
		}
	}
	// Unknown state must remain fail-closed. Production wires the durable
	// resolver, while lightweight callers/tests retain the first-switch rule.
	return true
}

func (handler *SubsystemOnboardingHandler) issuerForAlias(alias string) (string, error) {
	alias = strings.ToLower(strings.TrimSpace(alias))
	if alias == "" && handler.defaultIssuerAlias != "" {
		alias = handler.defaultIssuerAlias
	}
	switch alias {
	case "", "platform", "basic_platform":
		return handler.oidcIssuer, nil
	case "keycloak":
		if !handler.keycloakEnabled || handler.keycloakIssuer == "" || handler.keycloakRealm == "" {
			return "", application.ErrValidation
		}
		return handler.keycloakIssuer, nil
	default:
		return "", application.ErrValidation
	}
}

func (handler *SubsystemOnboardingHandler) effectiveIssuerAlias(alias string) string {
	alias = strings.ToLower(strings.TrimSpace(alias))
	if alias == "" {
		alias = handler.defaultIssuerAlias
	}
	return alias
}

// NewSubsystemOnboardingHandler constructs the subsystem onboarding HTTP adapter. The optional
// state store keeps compatibility with lightweight callers/tests while production wiring passes
// the durable control-plane repository.
func NewSubsystemOnboardingHandler(service subsystemOnboardingService, provisioner application.SubsystemProvisioner, access subsystemInitialAccessManager, oidcIssuer string, logger *slog.Logger, stateStores ...application.SubsystemDeploymentStateStore) (*SubsystemOnboardingHandler, error) {
	return NewSubsystemOnboardingHandlerWithNotifications(service, provisioner, access, oidcIssuer, logger, nil, stateStores...)
}

// NewSubsystemOnboardingHandlerWithNotifications 额外注入站内通知发送器（可选）。
func NewSubsystemOnboardingHandlerWithNotifications(service subsystemOnboardingService, provisioner application.SubsystemProvisioner, access subsystemInitialAccessManager, oidcIssuer string, logger *slog.Logger, notifications subsystemNotificationSink, stateStores ...application.SubsystemDeploymentStateStore) (*SubsystemOnboardingHandler, error) {
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
		notifications: notifications,
		oidcIssuer:    strings.TrimRight(strings.TrimSpace(oidcIssuer), "/"), logger: logger,
	}, nil
}

// DiscoverSubsystemCandidates exposes only opt-in Docker label metadata for containers that are
// not yet registered in the caller's tenant.  It never runs a deployment command and therefore
// can be granted with application read permission independently of onboarding permission.
func (handler *SubsystemOnboardingHandler) DiscoverSubsystemCandidates(writer stdhttp.ResponseWriter, request *stdhttp.Request) {
	principal, ok := subsystemPrincipal(writer, request)
	if !ok {
		return
	}
	discoverer, ok := handler.provisioner.(subsystemCandidateDiscoverer)
	if !ok {
		handler.writeError(writer, request, application.ErrSubsystemProvisioningUnavailable)
		return
	}
	candidates, err := discoverer.DiscoverSubsystemCandidates(request.Context())
	if err != nil {
		handler.writeError(writer, request, err)
		return
	}
	registered, err := handler.service.ListPortalApplications(request.Context(), principal.Tenant.ID, principal.User.ID, "")
	if err != nil {
		handler.writeError(writer, request, err)
		return
	}
	existing := make(map[string]struct{}, len(registered))
	for _, item := range registered {
		existing[strings.ToLower(strings.TrimSpace(item.Code))+"/"+strings.ToLower(strings.TrimSpace(item.Environment))] = struct{}{}
	}
	result := make([]application.SubsystemDiscoveryCandidate, 0, len(candidates))
	for _, candidate := range candidates {
		key := strings.ToLower(strings.TrimSpace(candidate.ApplicationCode)) + "/" + strings.ToLower(strings.TrimSpace(candidate.Environment))
		if _, found := existing[key]; !found {
			result = append(result, candidate)
		}
	}
	writer.Header().Set("Cache-Control", "no-store, private")
	httpresponse.WriteSuccess(writer, request, stdhttp.StatusOK, "发现未登记子系统成功", result)
}

// allowedServiceBindingsForTarget 返回服务器清单为该应用/环境声明的服务用途白名单；
// 未声明时返回 nil，应用层回退到平台硬编码默认。
func (handler *SubsystemOnboardingHandler) allowedServiceBindingsForTarget(applicationCode, environment string) []string {
	if provider, ok := handler.provisioner.(subsystemProvisioningCapabilityProvider); ok {
		for _, target := range provider.Capabilities().Targets {
			if target.ApplicationCode == strings.TrimSpace(applicationCode) && target.Environment == strings.TrimSpace(environment) {
				if target.AllowedServiceBindings == nil {
					return nil
				}
				return append([]string(nil), target.AllowedServiceBindings...)
			}
		}
	}
	return nil
}

// notifySubsystemLifecycle 在接入/重试/更新成功或失败时向操作人发送站内通知；通知失败只记日志，
// 不阻断接入结果（接入本身是否成功由控制面状态决定）。
func (handler *SubsystemOnboardingHandler) notifySubsystemLifecycle(ctx context.Context, tenantID, operatorID, applicationName, applicationCode, environment string, succeeded bool, detail string) {
	if handler.notifications == nil || strings.TrimSpace(operatorID) == "" {
		return
	}
	if err := handler.notifications.SendSubsystemLifecycle(ctx, SubsystemLifecycleNotification{
		TenantID: tenantID, OperatorID: operatorID, ApplicationName: applicationName,
		ApplicationCode: applicationCode, Environment: environment, Succeeded: succeeded, Detail: detail,
	}); err != nil {
		handler.logger.Warn("subsystem lifecycle notification failed", "application_code", applicationCode, "environment", environment, "error", err)
	}
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
	IssuerAlias     string  `json:"issuer_alias"`
}

// subsystemDirectoryRegistrationRequest contains only directory and gateway
// metadata.  OAuth client creation is intentionally excluded: for Keycloak it
// happens through /keycloak-integration/sync after this registration succeeds.
type subsystemDirectoryRegistrationRequest struct {
	ApplicationCode string  `json:"application_code"`
	ApplicationName string  `json:"application_name"`
	Description     *string `json:"description"`
	Environment     string  `json:"environment"`
	PublicBaseURL   string  `json:"public_base_url"`
	UpstreamURL     string  `json:"upstream_url"`
	PathPrefix      string  `json:"path_prefix"`
	IssuerAlias     string  `json:"issuer_alias"`
}

// subsystemLifecycleRequest is the shared payload for Update and Teardown. Update may also carry
// the already-persisted public gateway fields so the Agent can reapply non-secret runtime values;
// Teardown ignores them.
type subsystemLifecycleRequest struct {
	ApplicationCode string `json:"application_code"`
	Environment     string `json:"environment"`
	PublicBaseURL   string `json:"public_base_url"`
	UpstreamURL     string `json:"upstream_url"`
	PathPrefix      string `json:"path_prefix"`
	IssuerAlias     string `json:"issuer_alias"`
}

type keycloakClientSyncResponse struct {
	Alias               string               `json:"alias"`
	Realm               string               `json:"realm"`
	ClientID            string               `json:"client_id"`
	CanonicalClientID   string               `json:"canonical_client_id,omitempty"`
	ClientIDSource      string               `json:"client_id_source,omitempty"`
	LegacyCompatibility bool                 `json:"legacy_compatibility,omitempty"`
	ClaimsState         string               `json:"claims_state"`
	SwitchReady         bool                 `json:"switch_ready"`
	SwitchGates         []KeycloakSwitchGate `json:"switch_gates"`
	NextAction          string               `json:"next_action,omitempty"`
}

// keycloakBrokerLoginVerificationRequest is intentionally not a generic
// callback assertion: the current authenticated session must attest to its own
// identity and to the exact application environment and Keycloak Client.
type keycloakBrokerLoginVerificationRequest struct {
	ApplicationCode string `json:"application_code"`
	Environment     string `json:"environment"`
	IdentityID      string `json:"identity_id"`
	Issuer          string `json:"issuer"`
	ClientID        string `json:"client_id"`
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

type subsystemDirectoryRegistrationResponse struct {
	Application applicationResponse `json:"application"`
	Environment environmentResponse `json:"environment"`
	LoginTarget loginTargetResponse `json:"login_target"`
	NextAction  string              `json:"next_action"`
}

type subsystemAuthorizationResponse struct {
	InitialAdminUserID string `json:"initial_admin_user_id"`
	RoleCode           string `json:"role_code"`
}

type portalApplicationResponse struct {
	ApplicationID        string  `json:"application_id"`
	Code                 string  `json:"code"`
	Name                 string  `json:"name"`
	Description          *string `json:"description"`
	EnvironmentID        string  `json:"environment_id"`
	Environment          string  `json:"environment"`
	PathPrefix           *string `json:"path_prefix"`
	TargetCode           string  `json:"target_code"`
	TargetURI            string  `json:"target_uri"`
	PublicURL            string  `json:"public_url"`
	Allowed              bool    `json:"allowed"`
	ProjectionStatus     string  `json:"projection_status"`
	ProjectionReady      bool    `json:"projection_ready"`
	ProjectionNextAction string  `json:"projection_next_action,omitempty"`
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
	NextAction      string     `json:"next_action,omitempty"`
	StartedAt       *time.Time `json:"started_at,omitempty"`
	CompletedAt     *time.Time `json:"completed_at,omitempty"`
	UpdatedAt       time.Time  `json:"updated_at"`
}

type subsystemOnboardingDefaults struct {
	ApplicationCode string `json:"application_code,omitempty"`
	ApplicationName string `json:"application_name,omitempty"`
	Description     string `json:"description,omitempty"`
	Environment     string `json:"environment"`
	PublicBaseURL   string `json:"public_base_url"`
	UpstreamURL     string `json:"upstream_url,omitempty"`
	PathPrefix      string `json:"path_prefix,omitempty"`
	ClientType      string `json:"client_type"`
}

type subsystemProvisioningTargetResponse struct {
	ApplicationCode string `json:"application_code"`
	ApplicationName string `json:"application_name"`
	Description     string `json:"description,omitempty"`
	Environment     string `json:"environment"`
	UpstreamURL     string `json:"upstream_url"`
	PathPrefix      string `json:"path_prefix"`
	ClientType      string `json:"client_type"`
}

type subsystemProvisioningCapabilitiesResponse struct {
	AutomationEnabled         bool                                      `json:"automation_enabled"`
	DeploymentMode            string                                    `json:"deployment_mode"`
	SupportedApplicationCodes []string                                  `json:"supported_application_codes"`
	SupportedEnvironments     []string                                  `json:"supported_environments"`
	Targets                   []subsystemProvisioningTargetResponse     `json:"targets,omitempty"`
	Defaults                  subsystemOnboardingDefaults               `json:"defaults"`
	AuthenticationProviders   []subsystemAuthenticationProviderResponse `json:"authentication_providers"`
}

// subsystemAuthenticationProviderResponse is deliberately non-sensitive.  It
// is the page contract for provider state, switch availability and rollback.
type subsystemAuthenticationProviderResponse struct {
	Alias       string               `json:"alias"`
	Name        string               `json:"name"`
	Issuer      string               `json:"issuer"`
	Realm       string               `json:"realm,omitempty"`
	Status      string               `json:"status"`
	SwitchReady bool                 `json:"switch_ready"`
	SwitchGates []KeycloakSwitchGate `json:"switch_gates,omitempty"`
	NextAction  string               `json:"next_action,omitempty"`
	Detail      string               `json:"detail,omitempty"`
}

// OnboardSubsystem handles POST /api/v1/subsystem-onboarding.
func (handler *SubsystemOnboardingHandler) OnboardSubsystem(writer stdhttp.ResponseWriter, request *stdhttp.Request) {
	extendSubsystemDeploymentWriteDeadline(writer)
	principal, ok := subsystemPrincipal(writer, request)
	if !ok {
		return
	}
	var payload subsystemOnboardingRequest
	if !decodeApplicationManagementJSON(writer, request, &payload) {
		return
	}
	initialAdminUserID := strings.TrimSpace(payload.InitialAdminID)
	if initialAdminUserID == "" {
		initialAdminUserID = principal.User.ID
	}
	effectiveIssuerAlias := handler.effectiveIssuerAlias(payload.IssuerAlias)
	onboardingInput := application.SubsystemOnboardingInput{
		TenantID: principal.Tenant.ID, OperatorID: principal.User.ID, InitialAdminUserID: initialAdminUserID,
		ApplicationCode: payload.ApplicationCode, ApplicationName: payload.ApplicationName,
		Description: payload.Description, Environment: payload.Environment,
		PublicBaseURL: payload.PublicBaseURL, UpstreamURL: payload.UpstreamURL,
		PathPrefix: payload.PathPrefix, ClientType: payload.ClientType,
		IssuerAlias:            optionalIssuerAlias(effectiveIssuerAlias),
		AllowedServiceBindings: handler.allowedServiceBindingsForTarget(payload.ApplicationCode, payload.Environment),
	}
	if err := application.ValidateSubsystemOnboardingInput(onboardingInput); err != nil {
		handler.writeError(writer, request, err)
		return
	}
	if err := handler.service.PreflightValidate(request.Context(), onboardingInput); err != nil {
		handler.writeError(writer, request, err, "preflight")
		return
	}
	issuer, err := handler.issuerForAlias(effectiveIssuerAlias)
	if err != nil {
		handler.writeError(writer, request, err)
		return
	}
	if err := handler.provisioner.Preflight(request.Context(), application.SubsystemPreflightInput{
		TenantID: principal.Tenant.ID, ApplicationCode: onboardingInput.ApplicationCode,
		Environment: onboardingInput.Environment, Issuer: issuer,
		PublicBaseURL: onboardingInput.PublicBaseURL, UpstreamURL: onboardingInput.UpstreamURL,
		PathPrefix: onboardingInput.PathPrefix, ClientType: onboardingInput.ClientType,
	}); err != nil {
		handler.writeError(writer, request, err, "preflight")
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
	provisioningClientID, provisioningClientSecret := result.OAuthClient.ClientID, result.PlaintextSecret
	keycloakClientID := ""
	if strings.EqualFold(effectiveIssuerAlias, "keycloak") {
		if handler.keycloakControl == nil || handler.keycloakBroker == nil {
			handler.writeError(writer, request, application.ErrValidation)
			return
		}
		brokerID, brokerSecret, brokerErr := handler.keycloakBroker.EnsureKeycloakBroker(request.Context(), principal.Tenant.ID)
		if brokerErr != nil || handler.keycloakControl.EnsureBroker(request.Context(), brokerID, brokerSecret) != nil {
			handler.writeError(writer, request, application.ErrSubsystemProvisioningUnavailable)
			return
		}
		client, clientErr := handler.keycloakControl.EnsureClient(request.Context(), result.Application.Code+"-"+result.Environment.Environment+"-web", result.Application.Name+" "+result.Environment.Environment, result.RedirectURI)
		if clientErr != nil {
			handler.writeError(writer, request, application.ErrSubsystemProvisioningUnavailable)
			return
		}
		provisioningClientID, provisioningClientSecret = client.ClientID, client.ClientSecret
		keycloakClientID = client.ClientID
	}
	if err := handler.provisioner.Provision(request.Context(), application.SubsystemProvisioningInput{
		TenantID: principal.Tenant.ID, ApplicationID: result.Application.ID, ApplicationCode: result.Application.Code,
		Environment: result.Environment.Environment, Issuer: issuer,
		ClientID: provisioningClientID, ClientSecret: provisioningClientSecret,
		CatalogPublisherClientID:     result.CatalogPublisherOAuthClient.ClientID,
		CatalogPublisherClientSecret: result.CatalogPublisherPlaintextSecret,
		ServiceCredentials:           result.ServiceCredentials,
		RedirectURI:                  result.RedirectURI, PublicURL: result.PublicURL,
		PathPrefix: pathPrefix, UpstreamURL: upstreamURL,
	}); err != nil {
		handler.markDeploymentFailed(request.Context(), principal.Tenant.ID, result.Application.Code, result.Environment.Environment, "ONBOARD", "DEPLOYMENT_AGENT_FAILED", "部署 Agent 执行失败")
		handler.notifySubsystemLifecycle(request.Context(), principal.Tenant.ID, principal.User.ID, result.Application.Name, result.Application.Code, result.Environment.Environment, false, err.Error())
		handler.writeError(writer, request, err)
		return
	}
	// The application-owned role catalog is published by the deployment Agent. Assigning the
	// conventional admin role before Provision meant every new subsystem silently skipped its
	// initial administrator because the role did not exist yet.
	roleCode, err := handler.access.AssignInitialAdministrator(
		request.Context(), principal.Tenant.ID, result.Application.Code, initialAdminUserID, principal.User.ID,
	)
	if err != nil {
		handler.markDeploymentFailed(request.Context(), principal.Tenant.ID, result.Application.Code, result.Environment.Environment, "ONBOARD", "INITIAL_ACCESS_ASSIGNMENT_FAILED", "初始管理员授权失败")
		handler.notifySubsystemLifecycle(request.Context(), principal.Tenant.ID, principal.User.ID, result.Application.Name, result.Application.Code, result.Environment.Environment, false, err.Error())
		handler.writeError(writer, request, err)
		return
	}
	if err := handler.markInitialAccessAssigned(request.Context(), principal.Tenant.ID, result.Application.Code, result.Environment.Environment); err != nil {
		handler.markDeploymentFailed(request.Context(), principal.Tenant.ID, result.Application.Code, result.Environment.Environment, "ONBOARD", "INITIAL_ACCESS_STATE_FAILED", "初始管理员授权状态保存失败")
		handler.notifySubsystemLifecycle(request.Context(), principal.Tenant.ID, principal.User.ID, result.Application.Name, result.Application.Code, result.Environment.Environment, false, err.Error())
		handler.writeError(writer, request, err)
		return
	}
	// The deployment Agent has now published the application-owned role catalog and the
	// initial administrator binding exists.  A Keycloak-first onboarding must complete the
	// same projection work as an existing environment's “同步 Keycloak” action before it
	// is exposed as READY; otherwise the new Client could issue tokens without its final
	// role/permission claims.
	if keycloakClientID != "" {
		if handler.keycloakCatalog == nil || handler.keycloakMappings == nil {
			handler.markDeploymentFailed(request.Context(), principal.Tenant.ID, result.Application.Code, result.Environment.Environment, "ONBOARD", "KEYCLOAK_MAPPING_UNAVAILABLE", "Keycloak 授权映射组件不可用")
			handler.writeError(writer, request, application.ErrSubsystemProvisioningUnavailable)
			return
		}
		roleCodes, catalogErr := handler.keycloakCatalog.ListKeycloakRoleCodes(request.Context(), principal.Tenant.ID, result.Application.ID)
		if catalogErr != nil || handler.keycloakControl.EnsureClientRoles(request.Context(), keycloakClientID, roleCodes) != nil {
			handler.markDeploymentFailed(request.Context(), principal.Tenant.ID, result.Application.Code, result.Environment.Environment, "ONBOARD", "KEYCLOAK_ROLE_CATALOG_SYNC_FAILED", "Keycloak 角色目录同步失败")
			handler.writeError(writer, request, application.ErrSubsystemProvisioningUnavailable)
			return
		}
		if mappingErr := handler.keycloakMappings.SaveKeycloakClientMapping(request.Context(), principal.Tenant.ID, result.Application.ID, result.Environment.ID, handler.keycloakRealm, keycloakClientID); mappingErr != nil {
			handler.markDeploymentFailed(request.Context(), principal.Tenant.ID, result.Application.Code, result.Environment.Environment, "ONBOARD", "KEYCLOAK_CLIENT_MAPPING_FAILED", "Keycloak Client 映射保存失败")
			handler.writeError(writer, request, application.ErrSubsystemProvisioningUnavailable)
			return
		}
		if backfillErr := handler.keycloakMappings.BackfillKeycloakAuthorization(request.Context(), principal.Tenant.ID, result.Application.ID, result.Environment.ID); backfillErr != nil {
			handler.markDeploymentFailed(request.Context(), principal.Tenant.ID, result.Application.Code, result.Environment.Environment, "ONBOARD", "KEYCLOAK_AUTHORIZATION_BACKFILL_FAILED", "Keycloak 授权投影回填失败")
			handler.writeError(writer, request, application.ErrSubsystemProvisioningUnavailable)
			return
		}
		if updater, ok := handler.keycloakReadiness.(keycloakSwitchReadinessUpdater); ok {
			if readinessErr := updater.MarkKeycloakClientAndRoleCatalogSynced(request.Context(), principal.Tenant.ID, result.Application.ID, result.Environment.ID); readinessErr != nil {
				handler.markDeploymentFailed(request.Context(), principal.Tenant.ID, result.Application.Code, result.Environment.Environment, "ONBOARD", "KEYCLOAK_READINESS_UPDATE_FAILED", "Keycloak 就绪状态写入失败")
				handler.writeError(writer, request, application.ErrSubsystemProvisioningUnavailable)
				return
			}
		}
	}
	if err := handler.transitionDeployment(request.Context(), principal.Tenant.ID, result.Application.Code, result.Environment.Environment, application.SubsystemDeploymentStatusReady, "ONBOARD", "", ""); err != nil {
		handler.logger.Error("subsystem deployment completed but state update failed", "application_code", result.Application.Code, "environment", result.Environment.Environment, "error", err)
		handler.notifySubsystemLifecycle(request.Context(), principal.Tenant.ID, principal.User.ID, result.Application.Name, result.Application.Code, result.Environment.Environment, false, err.Error())
		handler.writeError(writer, request, err)
		return
	}
	handler.notifySubsystemLifecycle(request.Context(), principal.Tenant.ID, principal.User.ID, result.Application.Name, result.Application.Code, result.Environment.Environment, true, "")
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

// RegisterSubsystemDirectory handles POST /api/v1/subsystem-directory.
//
// It is the V2 registration entry point for application directory, environment
// and login-target data.  No platform OAuth Client, Keycloak Client, service
// credential, deployment state or initial role assignment is created here.
// Existing all-in-one /subsystem-onboarding callers stay supported during the
// migration, but new Keycloak integrations must use this endpoint followed by
// the dedicated Keycloak integration API.
func (handler *SubsystemOnboardingHandler) RegisterSubsystemDirectory(writer stdhttp.ResponseWriter, request *stdhttp.Request) {
	principal, ok := subsystemPrincipal(writer, request)
	if !ok {
		return
	}
	var payload subsystemDirectoryRegistrationRequest
	if !decodeApplicationManagementJSON(writer, request, &payload) {
		return
	}
	effectiveIssuerAlias := handler.effectiveIssuerAlias(payload.IssuerAlias)
	if _, err := handler.issuerForAlias(effectiveIssuerAlias); err != nil {
		handler.writeError(writer, request, err)
		return
	}
	result, err := handler.service.RegisterSubsystemDirectory(request.Context(), application.SubsystemDirectoryRegistrationInput{
		TenantID: principal.Tenant.ID, OperatorID: principal.User.ID,
		ApplicationCode: payload.ApplicationCode, ApplicationName: payload.ApplicationName,
		Description: payload.Description, Environment: payload.Environment,
		PublicBaseURL: payload.PublicBaseURL, UpstreamURL: payload.UpstreamURL,
		PathPrefix: payload.PathPrefix, IssuerAlias: optionalIssuerAlias(effectiveIssuerAlias),
	})
	if err != nil {
		handler.writeError(writer, request, err)
		return
	}
	writer.Header().Set("Cache-Control", "no-store, private")
	httpresponse.WriteSuccess(writer, request, stdhttp.StatusCreated, "应用目录与登录目标已登记；请继续在 Keycloak 认证接入中同步 Client 并配置运行时", subsystemDirectoryRegistrationResponse{
		Application: applicationToResponse(result.Application),
		Environment: environmentToResponse(result.Environment),
		LoginTarget: loginTargetToResponse(result.LoginTarget),
		NextAction:  "继续执行 Keycloak Client 同步；完成后再应用子系统运行时配置。",
	})
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
			Allowed: item.Allowed, ProjectionStatus: item.Projection.Status,
			ProjectionReady: item.Projection.Ready, ProjectionNextAction: item.Projection.NextAction,
		})
	}
	httpresponse.WriteSuccess(writer, request, stdhttp.StatusOK, "门户应用目录查询成功", responses)
}

// GetSubsystemCapabilities exposes the non-sensitive, server-configured onboarding policy used
// to render the management form. The isolated Agent still performs authoritative validation.
func (handler *SubsystemOnboardingHandler) GetSubsystemCapabilities(writer stdhttp.ResponseWriter, request *stdhttp.Request) {
	if _, ok := subsystemPrincipal(writer, request); !ok {
		return
	}
	capabilities := application.SubsystemProvisioningCapabilities{
		Enabled: false, Mode: "unknown", SupportedEnvironments: []string{},
	}
	if provider, ok := handler.provisioner.(subsystemProvisioningCapabilityProvider); ok {
		capabilities = provider.Capabilities()
	}
	defaultEnvironment := strings.TrimSpace(capabilities.DefaultEnvironment)
	if defaultEnvironment == "" && len(capabilities.SupportedEnvironments) > 0 {
		defaultEnvironment = capabilities.SupportedEnvironments[0]
	}
	if defaultEnvironment == "" {
		defaultEnvironment = "dev"
	}
	defaultClientType := strings.TrimSpace(capabilities.DefaultClientType)
	if defaultClientType == "" {
		defaultClientType = "confidential"
	}
	targets := make([]subsystemProvisioningTargetResponse, 0, len(capabilities.Targets))
	for _, target := range capabilities.Targets {
		targets = append(targets, subsystemProvisioningTargetResponse{
			ApplicationCode: target.ApplicationCode,
			ApplicationName: target.ApplicationName,
			Description:     target.Description,
			Environment:     target.Environment,
			UpstreamURL:     target.UpstreamURL,
			PathPrefix:      target.PathPrefix,
			ClientType:      target.ClientType,
		})
	}
	response := subsystemProvisioningCapabilitiesResponse{
		AutomationEnabled:         capabilities.Enabled,
		DeploymentMode:            capabilities.Mode,
		SupportedApplicationCodes: append([]string(nil), capabilities.SupportedApplicationCodes...),
		SupportedEnvironments:     append([]string(nil), capabilities.SupportedEnvironments...),
		Targets:                   targets,
		Defaults: subsystemOnboardingDefaults{
			ApplicationCode: capabilities.DefaultApplicationCode,
			ApplicationName: capabilities.DefaultApplicationName,
			Description:     capabilities.DefaultDescription,
			Environment:     defaultEnvironment,
			PublicBaseURL:   handler.oidcIssuer,
			UpstreamURL:     capabilities.DefaultUpstreamURL,
			PathPrefix:      capabilities.DefaultPathPrefix,
			ClientType:      defaultClientType,
		},
		AuthenticationProviders: []subsystemAuthenticationProviderResponse{{
			Alias: "platform", Name: "基础平台 OIDC", Issuer: handler.oidcIssuer,
			Status: "READY", SwitchReady: true,
		}},
	}
	keycloak := subsystemAuthenticationProviderResponse{
		Alias: "keycloak", Name: "Keycloak", Issuer: handler.keycloakIssuer,
		Realm: handler.keycloakRealm, Status: "NOT_CONFIGURED",
		Detail:      "尚未由平台配置 Keycloak 管理连接和 Broker Client。",
		SwitchGates: unverifiedKeycloakSwitchReadiness().Gates,
		NextAction:  "完成 Keycloak 配置后，选择具体应用环境同步并验证四项门禁。",
	}
	if handler.keycloakEnabled && handler.keycloakControl != nil && handler.keycloakBroker != nil && handler.keycloakIssuer != "" && handler.keycloakRealm != "" {
		keycloak.Status = "READY"
		keycloak.Detail = "Keycloak 管理连接已就绪；仍需对具体应用环境完成四项门禁验证。"
		keycloak.NextAction = "先同步 Keycloak，再完成用户投影和 Broker 登录验证。"
	}
	response.AuthenticationProviders = append(response.AuthenticationProviders, keycloak)
	writer.Header().Set("Cache-Control", "no-store, private")
	writer.Header().Set("Pragma", "no-cache")
	httpresponse.WriteSuccess(writer, request, stdhttp.StatusOK, "子系统部署能力查询成功", response)
}

// SubsystemHealthEntry describes the integration health of one subsystem environment.
type SubsystemHealthEntry struct {
	ApplicationCode string `json:"application_code"`
	ApplicationName string `json:"application_name"`
	Environment     string `json:"environment"`
	DirectoryOK     bool   `json:"directory_ok"`
	CredentialsOK   bool   `json:"credentials_ok"`
	RuntimeOK       bool   `json:"runtime_ok"`
	KeycloakOK      bool   `json:"keycloak_ok,omitempty"`
	Status          string `json:"status"`
	NextAction      string `json:"next_action,omitempty"`
}

// GetSubsystemHealthDashboard returns the integration health of all subsystems
// for the current tenant. It is a read-only diagnostic endpoint.
func (handler *SubsystemOnboardingHandler) GetSubsystemHealthDashboard(writer stdhttp.ResponseWriter, request *stdhttp.Request) {
	principal, ok := subsystemPrincipal(writer, request)
	if !ok {
		return
	}
	items, err := handler.service.ListPortalApplications(request.Context(), principal.Tenant.ID, principal.User.ID, "")
	if err != nil {
		handler.writeError(writer, request, err)
		return
	}
	entries := make([]SubsystemHealthEntry, 0, len(items))
	for _, item := range items {
		entry := SubsystemHealthEntry{
			ApplicationCode: item.Code,
			ApplicationName: item.Name,
			Environment:     item.Environment,
			DirectoryOK:     true,
			CredentialsOK:   true,
			RuntimeOK:       item.Projection.Ready,
		}
		if handler.keycloakEnabled {
			entry.KeycloakOK = item.Projection.Ready
		}
		if !item.Projection.Ready {
			entry.Status = "RUNTIME_NOT_READY"
			entry.NextAction = "检查部署状态和 Keycloak 投影"
		} else {
			entry.Status = "HEALTHY"
		}
		entries = append(entries, entry)
	}
	httpresponse.WriteSuccess(writer, request, stdhttp.StatusOK, "子系统健康状态查询成功", entries)
}

// SyncKeycloakClient creates or updates the selected subsystem's Keycloak RP
// and its required token claim mappers.  It is deliberately separate from the
// issuer switch: a successful Admin API write is not proof that brokered login
// has been tested, so the final runtime cutover remains fail-closed.
// VerifyKeycloakBrokerLogin records evidence from the dedicated Keycloak user
// JWT verification route. The route middleware fixes the issuer and verifies
// the signature before this handler binds the token identity and audience to a
// single application environment.
func (handler *SubsystemOnboardingHandler) VerifyKeycloakBrokerLogin(writer stdhttp.ResponseWriter, request *stdhttp.Request) {
	claims, ok := keycloakctx.BrokerClaimsFromContext(request.Context())
	if !ok {
		httpresponse.WriteError(writer, request, stdhttp.StatusUnauthorized, httperror.Unauthenticated)
		return
	}
	if strings.TrimSpace(claims.SessionID) == "" {
		httpresponse.WriteError(writer, request, stdhttp.StatusUnauthorized, httperror.Unauthenticated)
		return
	}
	var payload keycloakBrokerLoginVerificationRequest
	if !decodeApplicationManagementJSON(writer, request, &payload) {
		return
	}
	payload.ApplicationCode = strings.TrimSpace(payload.ApplicationCode)
	payload.Environment = strings.ToLower(strings.TrimSpace(payload.Environment))
	payload.IdentityID = strings.TrimSpace(payload.IdentityID)
	payload.Issuer = strings.TrimRight(strings.TrimSpace(payload.Issuer), "/")
	payload.ClientID = strings.TrimSpace(payload.ClientID)
	if payload.ApplicationCode == "" || payload.Environment == "" || payload.IdentityID == "" || payload.Issuer == "" || payload.ClientID == "" || payload.IdentityID != claims.IdentityID || !containsExactAudience(claims.Audience, payload.ClientID) {
		handler.writeError(writer, request, application.ErrValidation)
		return
	}
	if !handler.keycloakEnabled || handler.keycloakIssuer == "" || payload.Issuer != claims.Issuer || !strings.EqualFold(payload.Issuer, handler.keycloakIssuer) {
		handler.writeError(writer, request, application.ErrValidation)
		return
	}
	applicationID, environmentID, found := handler.resolveApplicationContextForIdentity(request, claims.TenantID, claims.IdentityID, payload.ApplicationCode, payload.Environment)
	if !found {
		// Do not disclose whether a target outside the current session's portal
		// visibility exists.
		handler.writeError(writer, request, application.ErrValidation)
		return
	}
	recorder, ok := handler.keycloakReadiness.(keycloakBrokerLoginVerificationRecorder)
	if !ok {
		// Missing durable recorder is a fail-closed deployment configuration.
		handler.writeError(writer, request, application.ErrSubsystemProvisioningUnavailable)
		return
	}
	if err := recorder.RecordBrokerLoginVerification(request.Context(), KeycloakBrokerLoginVerification{
		TenantID: claims.TenantID, ApplicationID: applicationID, EnvironmentID: environmentID,
		IdentityID: payload.IdentityID, Issuer: payload.Issuer, ClientID: payload.ClientID,
		VerifiedByID: claims.IdentityID, SessionID: claims.SessionID,
	}); err != nil {
		handler.logger.Warn("Keycloak broker login verification rejected", "application_code", payload.ApplicationCode, "environment", payload.Environment, "identity_id", payload.IdentityID, "error", err)
		handler.writeError(writer, request, application.ErrValidation)
		return
	}
	writer.Header().Set("Cache-Control", "no-store, private")
	writer.Header().Set("Pragma", "no-cache")
	httpresponse.WriteSuccess(writer, request, stdhttp.StatusOK, "Keycloak Broker 登录验证已记录；Issuer 仍受其余门禁保护", map[string]string{"status": "verified", "application_code": payload.ApplicationCode, "environment": payload.Environment})
}

func containsExactAudience(audience []string, expected string) bool {
	for _, value := range audience {
		if value == expected {
			return true
		}
	}
	return false
}

func (handler *SubsystemOnboardingHandler) SyncKeycloakClient(writer stdhttp.ResponseWriter, request *stdhttp.Request) {
	if _, ok := subsystemPrincipal(writer, request); !ok {
		return
	}
	if handler.keycloakControl == nil || handler.keycloakBroker == nil || !handler.keycloakEnabled {
		handler.writeError(writer, request, application.ErrValidation)
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
	principal, _ := authctx.PrincipalFromContext(request.Context())
	applicationID, environmentID, found := handler.resolveApplicationContext(writer, request, payload.ApplicationCode, payload.Environment)
	if !found || handler.keycloakCatalog == nil || handler.keycloakMappings == nil {
		handler.writeError(writer, request, application.ErrValidation)
		return
	}
	brokerID, brokerSecret, brokerErr := handler.keycloakBroker.EnsureKeycloakBroker(request.Context(), principal.Tenant.ID)
	if brokerErr != nil {
		handler.writeKeycloakSyncFailure(writer, request, payload, "broker", brokerErr)
		return
	}
	if err := handler.keycloakControl.EnsureBroker(request.Context(), brokerID, brokerSecret); err != nil {
		handler.writeKeycloakSyncFailure(writer, request, payload, "broker", err)
		return
	}
	canonicalClientID := strings.ToLower(strings.TrimSpace(payload.ApplicationCode)) + "-" + strings.ToLower(strings.TrimSpace(payload.Environment)) + "-web"
	resolution := KeycloakClientResolution{ClientID: canonicalClientID, CanonicalClientID: canonicalClientID, Source: "canonical"}
	if resolver, ok := handler.keycloakMappings.(keycloakClientCompatibilityResolver); ok {
		resolved, resolveErr := resolver.ResolveEffectiveKeycloakClient(request.Context(), principal.Tenant.ID, applicationID, environmentID, canonicalClientID)
		if resolveErr != nil {
			handler.writeKeycloakSyncFailure(writer, request, payload, "client", resolveErr)
			return
		}
		if strings.TrimSpace(resolved.ClientID) != "" {
			resolution = resolved
		}
	}
	transport, transportErr := application.ValidateKeycloakCutoverTransport(payload.PublicBaseURL, payload.PathPrefix, handler.keycloakRequireHTTPS)
	if transportErr != nil {
		handler.writeError(writer, request, transportErr)
		return
	}
	redirectURI := transport.RedirectURI
	result, err := handler.keycloakControl.EnsureClient(request.Context(), resolution.ClientID, "Basic Platform "+resolution.ClientID, redirectURI)
	if err != nil {
		handler.writeKeycloakSyncFailure(writer, request, payload, "client", err)
		return
	}
	// 同步 Keycloak Client 不能只更新 Keycloak。授权上下文解析依赖平台自身的
	// platform_oauth_client 记录；旧环境如果只有 Keycloak Client，登录时会在
	// azp 校验阶段得到 403。先确认 Keycloak 写入成功，再以同一租户、应用和
	// 环境补齐平台 Web Client，避免失败时留下孤儿授权记录。
	if _, err := handler.ensureWebOAuthClient(request.Context(), principal.Tenant.ID, applicationID, environmentID, principal.User.ID, result.ClientID, payload.ApplicationCode, redirectURI); err != nil {
		handler.writeKeycloakSyncFailure(writer, request, payload, "oauth_client", err)
		return
	}
	roleCodes, err := handler.keycloakCatalog.ListKeycloakRoleCodes(request.Context(), principal.Tenant.ID, applicationID)
	if err != nil {
		handler.writeKeycloakSyncFailure(writer, request, payload, "roles", err)
		return
	}
	if err := handler.keycloakControl.EnsureClientRoles(request.Context(), result.ClientID, roleCodes); err != nil {
		handler.writeKeycloakSyncFailure(writer, request, payload, "roles", err)
		return
	}
	if err := handler.keycloakMappings.SaveKeycloakClientMapping(request.Context(), principal.Tenant.ID, applicationID, environmentID, handler.keycloakRealm, result.ClientID); err != nil {
		handler.writeKeycloakSyncFailure(writer, request, payload, "mapping", err)
		return
	}
	if err := handler.keycloakMappings.BackfillKeycloakAuthorization(request.Context(), principal.Tenant.ID, applicationID, environmentID); err != nil {
		handler.writeKeycloakSyncFailure(writer, request, payload, "backfill", err)
		return
	}
	if updater, ok := handler.keycloakReadiness.(keycloakSwitchReadinessUpdater); ok {
		if err := updater.MarkKeycloakClientAndRoleCatalogSynced(request.Context(), principal.Tenant.ID, applicationID, environmentID); err != nil {
			handler.writeKeycloakSyncFailure(writer, request, payload, "readiness", err)
			return
		}
	}
	writer.Header().Set("Cache-Control", "no-store, private")
	// The Admin API calls above prove only the first two gates for this
	// request.  If a deployment has an authoritative verifier, ask it for the
	// complete per-environment evidence; otherwise retain the partial result.
	readiness := keycloakSyncReadiness()
	if handler.keycloakReadiness != nil {
		readiness = handler.keycloakSwitchReadiness(request.Context(), principal.Tenant.ID, payload.ApplicationCode, payload.Environment)
	}
	httpresponse.WriteSuccess(writer, request, stdhttp.StatusOK, "Keycloak Realm Client、身份、组织、角色与权限 Claims 映射已同步；Issuer 仍被四项门禁保护", keycloakClientSyncResponse{Alias: "keycloak", Realm: handler.keycloakRealm, ClientID: result.ClientID, CanonicalClientID: resolution.CanonicalClientID, ClientIDSource: resolution.Source, LegacyCompatibility: resolution.LegacyCompatibility, ClaimsState: "MAPPERS_SYNCED", SwitchReady: readiness.SwitchReady, SwitchGates: readiness.Gates, NextAction: readiness.NextAction})
}

// ensureWebOAuthClient 确保平台授权上下文可以解析子系统使用的浏览器 Client。
// 已存在且绑定一致的 ACTIVE 记录直接复用；缺失记录使用授权码 + PKCE 创建。
// 明文密钥不向上层返回，避免同步接口泄露浏览器 Client 的运行时凭据。
func (handler *SubsystemOnboardingHandler) ensureWebOAuthClient(ctx context.Context, tenantID, applicationID, environmentID, operatorID, clientID, applicationCode, redirectURI string) (application.OAuthClientView, error) {
	if handler.serviceCredentials == nil {
		return application.OAuthClientView{}, application.ErrSubsystemProvisioningUnavailable
	}
	clients, err := handler.serviceCredentials.ListOAuthClients(ctx, tenantID)
	if err != nil {
		return application.OAuthClientView{}, err
	}
	for _, client := range clients {
		if client.ClientID != clientID {
			continue
		}
		if !strings.EqualFold(client.Status, "ACTIVE") || client.ApplicationID != applicationID || client.EnvironmentID != environmentID {
			return application.OAuthClientView{}, application.ErrConflict
		}
		return client, nil
	}
	created, err := handler.serviceCredentials.CreateOAuthClient(ctx, application.OAuthClientCreateInput{
		TenantID: tenantID, ApplicationID: applicationID, EnvironmentID: environmentID, OperatorID: operatorID,
		ClientID: clientID, ClientName: applicationCode + " Web", ClientType: "confidential",
		TokenAuthMethod: "client_secret_basic", AccessTokenTTLSeconds: 15 * 60,
		RequirePKCE: true, GrantTypes: []string{"authorization_code"}, Scopes: []string{"openid", "profile"},
		RedirectURIs: []string{redirectURI},
	})
	if err != nil {
		return application.OAuthClientView{}, err
	}
	return created.Client, nil
}

// writeKeycloakSyncFailure keeps the external contract deliberately non-sensitive while
// preserving enough stage information for an operator to select the right remediation.
// The original error is logged only on the server and correlated with the request ID.
func (handler *SubsystemOnboardingHandler) writeKeycloakSyncFailure(writer stdhttp.ResponseWriter, request *stdhttp.Request, payload subsystemLifecycleRequest, stage string, err error) {
	detail, nextAction := keycloakSyncFailureGuidance(stage)
	handler.logger.Warn("Keycloak Client synchronization failed",
		"request_id", requestctx.RequestID(request.Context()),
		"stage", stage,
		"application_code", payload.ApplicationCode,
		"environment", payload.Environment,
		"error", err,
	)
	httpresponse.WriteError(writer, request, stdhttp.StatusServiceUnavailable, httperror.New(
		httperror.DependencyUnavailable.Code,
		httperror.DependencyUnavailable.Message,
		map[string]string{"stage": stage, "detail": detail, "next_action": nextAction},
	))
}

func keycloakSyncFailureGuidance(stage string) (detail, nextAction string) {
	switch stage {
	case "broker":
		return "Keycloak Broker 同步暂时不可用。", "检查 Keycloak 管理连接和平台 Broker 配置后，在当前环境重新同步。"
	case "client":
		return "Keycloak Realm Client 同步暂时不可用。", "检查目标 Realm、Client 配置与回调地址后，在当前环境重新同步。"
	case "oauth_client":
		return "平台 Web OAuth Client 同步暂时不可用。", "检查平台 OAuth Client 接入记录与租户应用环境关联后，在当前环境重新同步。"
	case "roles":
		return "Keycloak Client 角色目录同步暂时不可用。", "检查应用角色目录与 Keycloak Client Role 管理权限后，在当前环境重新同步。"
	case "mapping":
		return "Keycloak Client 映射保存暂时不可用。", "检查基础平台数据库与 Keycloak Client 映射状态后，在当前环境重新同步。"
	case "backfill":
		return "Keycloak 授权投影回填暂时不可用。", "检查基础平台数据库与授权投影队列后，在当前环境重新同步。"
	case "readiness":
		return "Keycloak 切换门禁状态保存暂时不可用。", "检查基础平台数据库与 Keycloak 就绪状态后，在当前环境重新同步。"
	default:
		return "Keycloak 同步暂时不可用。", "检查 Keycloak 管理连接后，在当前环境重新同步。"
	}
}

// UpdateSubsystem handles POST /api/v1/subsystem-update. The handler assumes the caller has
// already updated any DB aggregate (Environment base URL, OAuth redirect URI) via the regular
// management PATCH endpoints. This endpoint only re-runs the provisioner so the running subsystem
// picks up the new .env.local values and the portal gateway is reloaded.
func (handler *SubsystemOnboardingHandler) UpdateSubsystem(writer stdhttp.ResponseWriter, request *stdhttp.Request) {
	handler.updateSubsystem(writer, request, "", "UPDATE")
}

// AdoptSubsystem brings a directory-only environment under the deployment
// state machine. It is intentionally separate from update: callers cannot
// accidentally treat an unmanaged runtime as already deployed.
func (handler *SubsystemOnboardingHandler) AdoptSubsystem(writer stdhttp.ResponseWriter, request *stdhttp.Request) {
	handler.updateSubsystem(writer, request, "", "ADOPT")
}

// SwitchToKeycloak is the explicit authentication-integration cutover action.
// Its target issuer is intentionally fixed by the server so a browser cannot
// turn a Keycloak switch request into an arbitrary provider update.
func (handler *SubsystemOnboardingHandler) SwitchToKeycloak(writer stdhttp.ResponseWriter, request *stdhttp.Request) {
	handler.updateSubsystem(writer, request, "keycloak", "UPDATE")
}

// RollbackToPlatform is the explicit authentication-integration rollback
// action. It reuses the same controlled deployment workflow as legacy
// subsystem updates while pinning the target issuer to Basic Platform.
func (handler *SubsystemOnboardingHandler) RollbackToPlatform(writer stdhttp.ResponseWriter, request *stdhttp.Request) {
	handler.updateSubsystem(writer, request, "platform", "UPDATE")
}

type updateServiceCredentialRequirement struct {
	purpose    string
	suffix     string
	clientName string
	scope      string
	rotate     bool
}

func updateServiceCredentialRequirements(applicationCode string) []updateServiceCredentialRequirement {
	switch applicationCode {
	case "contract_management":
		return []updateServiceCredentialRequirement{{
			purpose: application.ServiceCredentialOwnerDirectoryRead, suffix: "owner-directory",
			clientName: "合同管理系统 Owner Directory Reader", scope: "owner_directory.read",
		}}
	case "customer_and_opportunity":
		// Audit and notification secrets are runtime delivery credentials. Existing
		// installations may predate notification_ingest or may contain a credential
		// delivered for another environment, so every controlled update rotates and
		// redelivers both values atomically with the environment file.
		return []updateServiceCredentialRequirement{
			{purpose: application.ServiceCredentialAuditIngest, suffix: "audit-publisher", clientName: "客户与商机管理系统 Audit Publisher", scope: "audit.ingest", rotate: true},
			{purpose: application.ServiceCredentialNotificationIngest, suffix: "notification-publisher", clientName: "客户与商机管理系统 Notification Publisher", scope: "notification.ingest", rotate: true},
		}
	case "customer_portal":
		// Portal 的服务凭据同时会被写入 portal.env 与 customer.env：门户 API
		// 负责身份映射/邀请校验，CRM 侧的补偿 Worker 负责异步修复跨系统状态。
		// 旧版本部署可能已经创建了这些 OAuth Client，但运行时文件没有收到
		// 明文 Secret；受控更新必须重新签发并原子下发，不能依赖只写密钥回读。
		return []updateServiceCredentialRequirement{
			{purpose: application.ServiceCredentialExternalUserProvision, suffix: "external-user-provision", clientName: "客户自助门户 External User Provisioner", scope: "external_user.provision", rotate: true},
			{purpose: application.ServiceCredentialApplicationRoleAssign, suffix: "role-assign", clientName: "客户自助门户 Application Role Assigner", scope: "application_role.assign", rotate: true},
			{purpose: application.ServiceCredentialApplicationRoleRevoke, suffix: "role-revoke", clientName: "客户自助门户 Application Role Revoker", scope: "application_role.revoke", rotate: true},
			{purpose: application.ServiceCredentialPortalMappingProvision, suffix: "portal-mapping-provision", clientName: "客户自助门户 Portal Identity Mapping Provisioner", scope: "portal.identity_mapping.provision", rotate: true},
			{purpose: application.ServiceCredentialPortalMappingDisable, suffix: "portal-mapping-disable", clientName: "客户自助门户 Portal Identity Mapping Disabler", scope: "portal.identity_mapping.disable", rotate: true},
			{purpose: application.ServiceCredentialPortalInviteVerify, suffix: "portal-invite-verify", clientName: "客户自助门户 Portal Invite Verifier", scope: "portal.invite.verify", rotate: true},
		}
	case "data_analysis":
		return []updateServiceCredentialRequirement{{
			purpose: application.ServiceCredentialAuditIngest, suffix: "audit-publisher",
			clientName: "数据看板与统计分析系统 Audit Publisher", scope: "audit.ingest",
		}}
	default:
		return nil
	}
}

func (handler *SubsystemOnboardingHandler) ensureUpdateServiceCredentials(ctx context.Context, tenantID, applicationID, environmentID, applicationCode, environment, operatorID, operation string) ([]application.SubsystemServiceCredential, error) {
	requirements := updateServiceCredentialRequirements(applicationCode)
	if handler.serviceCredentials == nil || len(requirements) == 0 {
		return nil, nil
	}
	if strings.TrimSpace(applicationID) == "" || strings.TrimSpace(environmentID) == "" {
		return nil, application.ErrNotFound
	}
	clients, err := handler.serviceCredentials.ListOAuthClients(ctx, tenantID)
	if err != nil {
		return nil, err
	}
	byClientID := make(map[string]application.OAuthClientView, len(clients))
	for _, client := range clients {
		byClientID[client.ClientID] = client
	}
	credentials := make([]application.SubsystemServiceCredential, 0, len(requirements))
	for _, requirement := range requirements {
		clientID := applicationCode + "-" + environment + "-" + requirement.suffix
		if client, ok := byClientID[clientID]; ok {
			if !strings.EqualFold(client.Status, "ACTIVE") {
				return nil, application.ErrConflict
			}
			// A retry always creates a recoverable replacement because a prior secret
			// may have been minted immediately before an Agent failure. Credentials
			// marked rotate are also redelivered on normal controlled updates.
			if operation != "RETRY" && !requirement.rotate {
				continue
			}
			secret, secretErr := handler.serviceCredentials.CreateOAuthClientSecret(ctx, application.OAuthClientSecretCreateInput{
				TenantID: tenantID, OAuthClientID: client.ID, OperatorID: operatorID,
			})
			if secretErr != nil {
				return nil, secretErr
			}
			credentials = append(credentials, application.SubsystemServiceCredential{
				Purpose: requirement.purpose, OAuthClient: client, PlaintextSecret: secret.PlaintextSecret,
			})
			continue
		}
		created, createErr := handler.serviceCredentials.CreateOAuthClient(ctx, application.OAuthClientCreateInput{
			TenantID: tenantID, ApplicationID: applicationID, EnvironmentID: environmentID, OperatorID: operatorID,
			ClientID: clientID, ClientName: requirement.clientName, ClientType: "service",
			TokenAuthMethod: "client_secret_basic", AccessTokenTTLSeconds: 15 * 60,
			GrantTypes: []string{"client_credentials"}, Scopes: []string{requirement.scope},
		})
		if createErr != nil {
			return nil, createErr
		}
		credentials = append(credentials, application.SubsystemServiceCredential{
			Purpose: requirement.purpose, OAuthClient: created.Client, PlaintextSecret: created.PlaintextSecret,
		})
	}
	return credentials, nil
}

// ensureCatalogPublisherCredential recovers a publish capability for a controlled
// retry without attempting to read a stored secret. Secrets are write-only, so a
// new version is minted exclusively for the trusted deployment Agent and never
// returned to the browser.
func (handler *SubsystemOnboardingHandler) ensureCatalogPublisherCredential(ctx context.Context, tenantID, applicationID, environmentID, applicationCode, environment, operatorID string) (application.OAuthClientView, string, error) {
	if handler.serviceCredentials == nil {
		return application.OAuthClientView{}, "", application.ErrSubsystemProvisioningUnavailable
	}
	clientID := applicationCode + "-" + environment + "-catalog-publisher"
	clients, err := handler.serviceCredentials.ListOAuthClients(ctx, tenantID)
	if err != nil {
		return application.OAuthClientView{}, "", err
	}
	for _, client := range clients {
		if client.ClientID != clientID {
			continue
		}
		if !strings.EqualFold(client.Status, "ACTIVE") || client.ApplicationID != applicationID || client.EnvironmentID != environmentID {
			return application.OAuthClientView{}, "", application.ErrConflict
		}
		secret, secretErr := handler.serviceCredentials.CreateOAuthClientSecret(ctx, application.OAuthClientSecretCreateInput{TenantID: tenantID, OAuthClientID: client.ID, OperatorID: operatorID})
		if secretErr != nil {
			return application.OAuthClientView{}, "", secretErr
		}
		return client, secret.PlaintextSecret, nil
	}
	created, createErr := handler.serviceCredentials.CreateOAuthClient(ctx, application.OAuthClientCreateInput{
		TenantID: tenantID, ApplicationID: applicationID, EnvironmentID: environmentID, OperatorID: operatorID,
		ClientID: clientID, ClientName: applicationCode + " Authorization Catalog Publisher", ClientType: "service",
		TokenAuthMethod: "client_secret_basic", AccessTokenTTLSeconds: 15 * 60,
		GrantTypes: []string{"client_credentials"}, Scopes: []string{"authorization.catalog.sync"},
	})
	if createErr != nil {
		return application.OAuthClientView{}, "", createErr
	}
	return created.Client, created.PlaintextSecret, nil
}

func (handler *SubsystemOnboardingHandler) updateSubsystem(writer stdhttp.ResponseWriter, request *stdhttp.Request, forcedIssuerAlias, requestedOperation string) {
	extendSubsystemDeploymentWriteDeadline(writer)
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
	if forcedIssuerAlias != "" {
		payload.IssuerAlias = forcedIssuerAlias
	}
	applicationCode := strings.TrimSpace(payload.ApplicationCode)
	environment := strings.ToLower(strings.TrimSpace(payload.Environment))
	isRetry := requestedOperation == "RETRY" || strings.HasSuffix(strings.TrimRight(request.URL.Path, "/"), "/subsystem-retry")
	// 普通更新不会无条件轮换浏览器 OAuth 密钥；只有认证切换或目录恢复时才会把
	// 新密钥交给部署 Agent。授权目录发布凭据则例外：它是应用绑定的机器身份，
	// 每次受控更新都必须重新下发，才能修复旧环境中遗留的占位值或已失效密钥。
	effectiveIssuerAlias := handler.effectiveIssuerAlias(payload.IssuerAlias)
	// An explicit /switch request is always a cutover attempt, even if a legacy
	// browser first updated issuer_alias optimistically.  Otherwise the generic
	// metadata write could accidentally bypass the observation-window gate.
	keycloakCutover := strings.EqualFold(effectiveIssuerAlias, "keycloak") && (strings.EqualFold(forcedIssuerAlias, "keycloak") || handler.keycloakCutoverRequired(request.Context(), principal.Tenant.ID, applicationCode, environment))
	if keycloakCutover {
		readiness := handler.keycloakSwitchReadiness(request.Context(), principal.Tenant.ID, applicationCode, environment)
		if !readiness.SwitchReady {
			writeKeycloakSwitchBlocked(writer, request, readiness)
			return
		}
		if handler.keycloakCutover == nil {
			handler.writeError(writer, request, application.ErrSubsystemProvisioningUnavailable)
			return
		}
		if err := handler.keycloakCutover.CanKeycloakCutover(request.Context(), principal.Tenant.ID, applicationCode, environment); err != nil {
			writeKeycloakObservationBlocked(writer, request, err.Error())
			return
		}
	}
	// As with cutover, explicit rollback must not rely on a browser's previous
	// issuer_alias update. The lifecycle store is the source of truth for the
	// rollback deadline and rejects a platform-only environment safely.
	keycloakRollback := strings.EqualFold(forcedIssuerAlias, "platform")
	if keycloakRollback {
		if handler.keycloakCutover == nil {
			handler.writeError(writer, request, application.ErrSubsystemProvisioningUnavailable)
			return
		}
		if err := handler.keycloakCutover.CanKeycloakRollback(request.Context(), principal.Tenant.ID, applicationCode, environment); err != nil {
			writeKeycloakObservationBlocked(writer, request, err.Error())
			return
		}
	}
	issuer, err := handler.issuerForAlias(effectiveIssuerAlias)
	if err != nil {
		handler.writeError(writer, request, err)
		return
	}
	updateInput := application.SubsystemProvisioningInput{
		TenantID:        principal.Tenant.ID,
		ApplicationCode: applicationCode,
		Environment:     environment,
		// Issuer is required by the post-rebuild catalog sync (it issues the PUT against
		// the platform's /authorization-catalog endpoint). Use the platform-configured OIDC
		// issuer as a stable source of truth instead of having the client send it in.
		Issuer:                      issuer,
		AuthenticationRuntimeUpdate: keycloakCutover || keycloakRollback,
	}
	publicBaseURL := strings.TrimRight(strings.TrimSpace(payload.PublicBaseURL), "/")
	upstreamURL := strings.TrimRight(strings.TrimSpace(payload.UpstreamURL), "/")
	pathPrefix := strings.TrimRight(strings.TrimSpace(payload.PathPrefix), "/")
	// When the management page supplies the current gateway fields, carry the derived
	// browser URLs to the Agent so a changed host/port updates both runtime config and
	// the OIDC callback without re-onboarding the environment. Legacy callers may omit
	// the fields and retain the rebuild-only behavior.
	if publicBaseURL != "" || upstreamURL != "" || pathPrefix != "" {
		if publicBaseURL == "" || upstreamURL == "" || pathPrefix == "" {
			handler.writeError(writer, request, application.ErrValidation)
			return
		}
		updateInput.PublicURL = publicBaseURL + pathPrefix + "/"
		updateInput.RedirectURI = publicBaseURL + pathPrefix + "/auth/callback"
		updateInput.PathPrefix = pathPrefix
		updateInput.UpstreamURL = upstreamURL
	}
	// Directory-only registration deliberately does not create runtime credentials.  A
	// subsequent ADOPT/RETRY must therefore re-resolve the Keycloak web Client and pass its
	// current secret to the Agent; otherwise the Agent starts the container with
	// PENDING_ONBOARDING and the application fails with a catalog-token 401.
	keycloakRuntimeCredentialRefresh := strings.EqualFold(effectiveIssuerAlias, "keycloak") &&
		(requestedOperation == "ADOPT" || isRetry) && !keycloakCutover
	if keycloakRuntimeCredentialRefresh && handler.keycloakControl != nil && strings.TrimSpace(updateInput.RedirectURI) != "" {
		canonicalClientID := strings.ToLower(applicationCode) + "-" + strings.ToLower(environment) + "-web"
		resolution := KeycloakClientResolution{ClientID: canonicalClientID, CanonicalClientID: canonicalClientID, Source: "canonical"}
		client, clientErr := handler.keycloakControl.EnsureClient(request.Context(), resolution.ClientID, "Basic Platform "+resolution.ClientID, updateInput.RedirectURI)
		if clientErr != nil {
			handler.writeError(writer, request, application.ErrSubsystemProvisioningUnavailable)
			return
		}
		updateInput.ClientID, updateInput.ClientSecret = client.ClientID, client.ClientSecret
		updateInput.AuthenticationRuntimeUpdate = true
	}
	if keycloakCutover {
		transport, transportErr := application.ValidateKeycloakCutoverTransport(publicBaseURL, pathPrefix, handler.keycloakRequireHTTPS)
		if transportErr != nil {
			handler.writeError(writer, request, transportErr)
			return
		}
		updateInput.PublicURL = transport.PublicURL
		updateInput.RedirectURI = transport.RedirectURI
		if handler.keycloakControl == nil || handler.keycloakBroker == nil || publicBaseURL == "" || pathPrefix == "" {
			handler.writeError(writer, request, application.ErrValidation)
			return
		}
		brokerID, brokerSecret, brokerErr := handler.keycloakBroker.EnsureKeycloakBroker(request.Context(), principal.Tenant.ID)
		if brokerErr != nil || handler.keycloakControl.EnsureBroker(request.Context(), brokerID, brokerSecret) != nil {
			handler.writeError(writer, request, application.ErrSubsystemProvisioningUnavailable)
			return
		}
		canonicalClientID := strings.ToLower(applicationCode) + "-" + strings.ToLower(environment) + "-web"
		resolution := KeycloakClientResolution{ClientID: canonicalClientID, CanonicalClientID: canonicalClientID, Source: "canonical"}
		if applicationID, environmentID, resolved := handler.resolveApplicationContext(writer, request, applicationCode, environment); resolved {
			if compatibilityResolver, supported := handler.keycloakMappings.(keycloakClientCompatibilityResolver); supported {
				if effective, resolveErr := compatibilityResolver.ResolveEffectiveKeycloakClient(request.Context(), principal.Tenant.ID, applicationID, environmentID, canonicalClientID); resolveErr != nil {
					handler.logger.Warn("Keycloak Client compatibility resolution failed", "application_code", applicationCode, "environment", environment, "error", resolveErr)
				} else if strings.TrimSpace(effective.ClientID) != "" {
					resolution = effective
				}
			}
		}
		client, clientErr := handler.keycloakControl.EnsureClient(request.Context(), resolution.ClientID, "Basic Platform "+resolution.ClientID, updateInput.RedirectURI)
		if clientErr != nil {
			handler.logger.Warn("Keycloak Client synchronization before switch failed", "application_code", applicationCode, "environment", environment, "error", clientErr)
			handler.writeError(writer, request, application.ErrSubsystemProvisioningUnavailable)
			return
		}
		updateInput.ClientID, updateInput.ClientSecret = client.ClientID, client.ClientSecret
	}
	// Resolve identifiers from the deployment control plane, not the portal projection: failed
	// and updating environments are intentionally hidden from the user-facing portal catalog.
	var deploymentContext application.SubsystemDeploymentState
	environmentID := ""
	if handler.deploymentState != nil {
		state, err := handler.deploymentState.GetSubsystemDeploymentContext(request.Context(), principal.Tenant.ID, applicationCode, environment)
		if err != nil {
			// A directory-only registration has application/environment records but no
			// deployment-state row yet.  The UI exposes the recovery action as “retry”
			// after a failed first deployment, so treat RETRY like ADOPT at this
			// boundary.  ResolveApplicationEnvironment still enforces that the target
			// is a known, tenant-scoped application; unknown targets remain 404.
			if requestedOperation != "ADOPT" && !isRetry || !errors.Is(err, application.ErrNotFound) {
				handler.writeError(writer, request, err)
				return
			}
			applicationID, resolvedEnvironmentID, resolveErr := handler.service.ResolveApplicationEnvironment(request.Context(), principal.Tenant.ID, applicationCode, environment)
			if resolveErr != nil {
				handler.writeError(writer, request, resolveErr)
				return
			}
			deploymentContext = application.SubsystemDeploymentState{TenantID: principal.Tenant.ID, ApplicationID: applicationID, EnvironmentID: resolvedEnvironmentID, ApplicationCode: applicationCode, Environment: environment}
			updateInput.ApplicationID = applicationID
			environmentID = resolvedEnvironmentID
		} else {
			deploymentContext = state
			updateInput.ApplicationID = state.ApplicationID
			environmentID = state.EnvironmentID
		}
	} else if applicationID, resolvedEnvironmentID, ok := handler.resolveApplicationContext(writer, request, applicationCode, environment); ok {
		updateInput.ApplicationID = applicationID
		environmentID = resolvedEnvironmentID
	}
	if strings.TrimSpace(updateInput.ApplicationID) == "" {
		handler.writeError(writer, request, application.ErrNotFound)
		return
	}
	if publisherID, publisherSecret, ok := readCatalogPublisherCredentials(); ok {
		updateInput.CatalogPublisherClientID = publisherID
		updateInput.CatalogPublisherClientSecret = publisherSecret
	}
	operation := requestedOperation
	if operation == "" {
		operation = "UPDATE"
	}
	if isRetry {
		operation = "RETRY"
	}
	if requiresCatalogPublisherCredential(applicationCode) && handler.serviceCredentials == nil {
		// 授权目录发布凭据为只写密钥，缺少凭据管理器时绝不能假装更新成功并让
		// Agent 启动一个仍使用占位值的容器。
		handler.writeError(writer, request, application.ErrSubsystemProvisioningUnavailable)
		return
	}
	serviceCredentials, credentialErr := handler.ensureUpdateServiceCredentials(
		request.Context(), principal.Tenant.ID, updateInput.ApplicationID, environmentID,
		applicationCode, environment, principal.User.ID, operation,
	)
	if credentialErr != nil {
		handler.writeError(writer, request, credentialErr)
		return
	}
	updateInput.ServiceCredentials = serviceCredentials
	if handler.serviceCredentials != nil && requiresCatalogPublisherCredential(applicationCode) {
		publisher, publisherSecret, publisherErr := handler.ensureCatalogPublisherCredential(
			request.Context(), principal.Tenant.ID, updateInput.ApplicationID, environmentID, applicationCode, environment, principal.User.ID,
		)
		if publisherErr != nil {
			handler.writeError(writer, request, publisherErr)
			return
		}
		updateInput.CatalogPublisherClientID = publisher.ClientID
		updateInput.CatalogPublisherClientSecret = publisherSecret
	}
	retryAdminUserID := principal.User.ID
	retryNeedsInitialAccess := false
	if operation == "RETRY" && handler.deploymentState != nil {
		if storedAdminUserID := strings.TrimSpace(deploymentContext.InitialAdminUserID); storedAdminUserID != "" {
			retryAdminUserID = storedAdminUserID
		}
		retryNeedsInitialAccess = deploymentContext.InitialAccessAssignedAt == nil
	}
	if err := handler.transitionDeployment(request.Context(), principal.Tenant.ID, applicationCode, environment, application.SubsystemDeploymentStatusUpdating, operation, "", ""); err != nil {
		handler.writeError(writer, request, err)
		return
	}
	if err := handler.provisioner.Update(request.Context(), updateInput); err != nil {
		handler.markDeploymentFailed(request.Context(), principal.Tenant.ID, applicationCode, environment, operation, "DEPLOYMENT_AGENT_FAILED", "部署 Agent 执行失败")
		handler.notifySubsystemLifecycle(request.Context(), principal.Tenant.ID, principal.User.ID, applicationCode, applicationCode, environment, false, err.Error())
		handler.writeError(writer, request, err)
		return
	}
	if operation == "RETRY" && retryNeedsInitialAccess {
		// A first-time deployment can fail after credentials are created but before the role
		// catalog and initial administrator are ready. Retry therefore reapplies the conventional
		// administrator role to the current operator after the Agent succeeds. UpdateAccess is
		// idempotent for an already assigned role and never requires recovering an OAuth secret.
		if _, err := handler.access.AssignInitialAdministrator(
			request.Context(), principal.Tenant.ID, applicationCode, retryAdminUserID, principal.User.ID,
		); err != nil {
			handler.markDeploymentFailed(request.Context(), principal.Tenant.ID, applicationCode, environment, operation, "INITIAL_ACCESS_ASSIGNMENT_FAILED", "初始管理员授权失败")
			handler.writeError(writer, request, err)
			return
		}
		if err := handler.markInitialAccessAssigned(request.Context(), principal.Tenant.ID, applicationCode, environment); err != nil {
			handler.markDeploymentFailed(request.Context(), principal.Tenant.ID, applicationCode, environment, operation, "INITIAL_ACCESS_STATE_FAILED", "初始管理员授权状态保存失败")
			handler.writeError(writer, request, err)
			return
		}
	}
	if err := handler.transitionDeployment(request.Context(), principal.Tenant.ID, applicationCode, environment, application.SubsystemDeploymentStatusReady, operation, "", ""); err != nil {
		handler.logger.Error("subsystem update completed but state update failed", "application_code", applicationCode, "environment", environment, "error", err)
		handler.notifySubsystemLifecycle(request.Context(), principal.Tenant.ID, principal.User.ID, applicationCode, applicationCode, environment, false, err.Error())
		handler.writeError(writer, request, err)
		return
	}
	if keycloakCutover || keycloakRollback {
		applicationID, environmentID, found := handler.resolveApplicationContext(writer, request, applicationCode, environment)
		if !found {
			handler.logger.Error("Keycloak cutover completed but lifecycle scope could not be resolved", "application_code", applicationCode, "environment", environment)
			handler.writeError(writer, request, application.ErrSubsystemProvisioningUnavailable)
			return
		}
		var lifecycleErr error
		if keycloakCutover {
			_, lifecycleErr = handler.keycloakCutover.ConfirmKeycloakCutover(request.Context(), principal.Tenant.ID, applicationID, environmentID, principal.User.ID, keycloakRollbackWindow)
		} else {
			_, lifecycleErr = handler.keycloakCutover.RecordKeycloakRollback(request.Context(), principal.Tenant.ID, applicationID, environmentID, principal.User.ID)
		}
		if lifecycleErr != nil {
			handler.logger.Error("Keycloak runtime change completed but lifecycle evidence could not be recorded", "application_code", applicationCode, "environment", environment, "error", lifecycleErr)
			handler.writeError(writer, request, application.ErrSubsystemProvisioningUnavailable)
			return
		}
	}
	handler.notifySubsystemLifecycle(request.Context(), principal.Tenant.ID, principal.User.ID, applicationCode, applicationCode, environment, true, "")
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

// requiresCatalogPublisherCredential 判断子系统是否会在启动时发布自有授权目录。
// 该机器凭据是只写数据，部署中断后不能从运行时文件反向恢复，因此每次受控生命周期操作
// 都重新下发，避免旧环境继续使用占位值或失效密钥。
func requiresCatalogPublisherCredential(applicationCode string) bool {
	switch applicationCode {
	case "settlement", "data_analysis":
		return true
	default:
		return false
	}
}

// TeardownSubsystem handles POST /api/v1/subsystem-teardown. Stops containers, removes
// .env.local, drops the portal gateway include, and reloads nginx. The HTTP layer does not
// delete the corresponding DB rows here: the script follows up with DELETE /environments and
// (optionally) DELETE /applications so the audit trail preserves each cleanup step.
func (handler *SubsystemOnboardingHandler) TeardownSubsystem(writer stdhttp.ResponseWriter, request *stdhttp.Request) {
	extendSubsystemDeploymentWriteDeadline(writer)
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
	if err := handler.provisioner.Teardown(request.Context(), principal.Tenant.ID, applicationCode, environment); err != nil {
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

const subsystemDeploymentHTTPTimeout = 16 * time.Minute

// A deployment state is written before the long-running Agent call. If the API
// process is terminated during that call, the state must not remain in a
// non-terminal status forever: the management UI only exposes retry for a
// failed attempt. Keep this slightly above the synchronous request timeout so
// a genuinely slow but still live deployment is not interrupted by a status
// poll.
const subsystemDeploymentStaleAfter = 20 * time.Minute

// extendSubsystemDeploymentWriteDeadline keeps the synchronous control-plane request alive for
// the Agent's bounded 15-minute deployment window. ResponseController unwraps Gin's writer to the
// underlying network connection; httptest and unsupported writers safely ignore the capability.
func extendSubsystemDeploymentWriteDeadline(writer stdhttp.ResponseWriter) {
	controller := stdhttp.NewResponseController(writer)
	_ = controller.SetWriteDeadline(time.Now().Add(subsystemDeploymentHTTPTimeout))
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
		// A manually registered application/environment predates, or intentionally
		// bypasses, a managed deployment record. It is still a valid control-plane
		// resource, so the status read must not turn the whole runtime card into a
		// 404. Resolve the registry boundary first; an unknown application remains
		// a real 404, while a known one receives an explicit safe state.
		if !errors.Is(err, application.ErrNotFound) {
			handler.writeError(writer, request, err)
			return
		}
		applicationID, environmentID, resolveErr := handler.service.ResolveApplicationEnvironment(request.Context(), principal.Tenant.ID, applicationCode, environment)
		if resolveErr != nil {
			handler.writeError(writer, request, resolveErr)
			return
		}
		state = application.SubsystemDeploymentState{
			TenantID: principal.Tenant.ID, ApplicationID: applicationID, EnvironmentID: environmentID,
			ApplicationCode: applicationCode, Environment: environment,
			Status: application.SubsystemDeploymentStatusUnmanaged, Operation: "NONE",
		}
	}
	state = handler.recoverStaleSubsystemDeployment(request.Context(), state)
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
		NextAction:      subsystemDeploymentStateNextAction(state),
		StartedAt:       state.StartedAt,
		CompletedAt:     state.CompletedAt,
		UpdatedAt:       state.UpdatedAt,
	})
}

// GetKeycloakIntegrationStatus handles GET /api/v1/keycloak-integration/status.
// It is separate from deployment status: the response is a durable view of
// the selected provider, its non-sensitive Client mapping and server-checked
// cutover gates.  Runtime credentials and arbitrary environment metadata are
// intentionally excluded.
func (handler *SubsystemOnboardingHandler) GetKeycloakIntegrationStatus(writer stdhttp.ResponseWriter, request *stdhttp.Request) {
	principal, ok := subsystemPrincipal(writer, request)
	if !ok {
		return
	}
	applicationCode := strings.TrimSpace(request.URL.Query().Get("application_code"))
	environment := strings.ToLower(strings.TrimSpace(request.URL.Query().Get("environment")))
	if applicationCode == "" || environment == "" {
		handler.writeError(writer, request, application.ErrValidation)
		return
	}
	applicationID, environmentID, err := handler.service.ResolveApplicationEnvironment(request.Context(), principal.Tenant.ID, applicationCode, environment)
	if err != nil {
		handler.writeError(writer, request, err)
		return
	}
	issuerAlias := handler.effectiveIssuerAlias("")
	if resolver, supported := handler.service.(subsystemEnvironmentIssuerResolver); supported {
		if persisted, resolveErr := resolver.ResolveEnvironmentIssuerAlias(request.Context(), principal.Tenant.ID, applicationCode, environment); resolveErr == nil {
			issuerAlias = handler.effectiveIssuerAlias(persisted)
		}
	}
	mapping := KeycloakClientMapping{Status: "NOT_CONFIGURED"}
	if inspector, supported := handler.keycloakMappings.(keycloakClientMappingInspector); supported {
		loaded, loadErr := inspector.GetKeycloakClientMapping(request.Context(), principal.Tenant.ID, applicationID, environmentID)
		if loadErr != nil {
			handler.logger.Warn("load Keycloak Client mapping status failed", "application_code", applicationCode, "environment", environment, "error", loadErr)
			mapping.Status = "UNKNOWN"
		} else {
			mapping = loaded
		}
	}
	if mapping.Realm == "" {
		mapping.Realm = handler.keycloakRealm
	}
	readiness := handler.keycloakSwitchReadiness(request.Context(), principal.Tenant.ID, applicationCode, environment)
	claimsState := "NOT_CONFIGURED"
	if mapping.Exists && strings.EqualFold(mapping.Status, "SYNCED") {
		claimsState = "MAPPERS_SYNCED"
	} else if mapping.Status == "UNKNOWN" {
		claimsState = "UNKNOWN"
	}
	cutover := KeycloakCutoverLifecycle{Status: "NOT_CONFIGURED"}
	var timeline []KeycloakCutoverTimelineEvent
	if handler.keycloakCutover != nil {
		loaded, lifecycleErr := handler.keycloakCutover.GetKeycloakCutoverLifecycle(request.Context(), principal.Tenant.ID, applicationCode, environment)
		if lifecycleErr != nil {
			handler.logger.Warn("load Keycloak cutover lifecycle failed", "application_code", applicationCode, "environment", environment, "error", lifecycleErr)
			cutover.Status = "UNKNOWN"
		} else {
			cutover = loaded
			timeline, lifecycleErr = handler.keycloakCutover.ListKeycloakCutoverTimeline(request.Context(), principal.Tenant.ID, applicationCode, environment, 50)
			if lifecycleErr != nil {
				handler.logger.Warn("load Keycloak cutover timeline failed", "application_code", applicationCode, "environment", environment, "error", lifecycleErr)
			}
		}
	}
	writer.Header().Set("Cache-Control", "no-store, private")
	writer.Header().Set("Pragma", "no-cache")
	httpresponse.WriteSuccess(writer, request, stdhttp.StatusOK, "Keycloak 认证接入状态查询成功", map[string]any{
		"application_code": applicationCode,
		"environment":      environment,
		"provider":         issuerAlias,
		"realm":            mapping.Realm,
		"client_id":        mapping.ClientID,
		"client_state":     mapping.Status,
		"claims_state":     claimsState,
		"last_synced_at":   mapping.LastSyncedAt,
		"switch_ready":     readiness.SwitchReady,
		"switch_gates":     readiness.Gates,
		"next_action":      readiness.NextAction,
		"cutover":          cutover,
		"timeline":         timeline,
	})
}

// KeycloakSyncStatusResponse is the focused synchronization status for one
// subsystem environment. It includes drift detection results for operational
// dashboards.
type KeycloakSyncStatusResponse struct {
	ApplicationCode string `json:"application_code"`
	Environment     string `json:"environment"`
	ClientID        string `json:"client_id,omitempty"`
	ClientState     string `json:"client_state"`
	Realm           string `json:"realm"`
	LastSyncedAt    any    `json:"last_synced_at,omitempty"`
	SwitchReady     bool   `json:"switch_ready"`
	DriftAvailable  bool   `json:"drift_available"`
	DriftError      string `json:"drift_error,omitempty"`
	Drift           *struct {
		HasDrift       bool     `json:"has_drift"`
		MissingRoles   []string `json:"missing_roles,omitempty"`
		StaleRoles     []string `json:"stale_roles,omitempty"`
		MissingMappers []string `json:"missing_mappers,omitempty"`
		DriftedMappers []string `json:"drifted_mappers,omitempty"`
		RedirectURIOK  bool     `json:"redirect_uri_ok"`
		BrokerConfigOK bool     `json:"broker_config_ok"`
	} `json:"drift,omitempty"`
}

func (handler *SubsystemOnboardingHandler) loadKeycloakRoleCodesForDrift(ctx context.Context, tenantID, applicationID string) ([]string, error) {
	if handler.keycloakCatalog == nil {
		return nil, errors.New("Keycloak authorization catalog is not configured")
	}
	return handler.keycloakCatalog.ListKeycloakRoleCodes(ctx, tenantID, applicationID)
}

// GetKeycloakSyncStatus returns a focused synchronization status including
// drift detection for one subsystem environment.
func (handler *SubsystemOnboardingHandler) GetKeycloakSyncStatus(writer stdhttp.ResponseWriter, request *stdhttp.Request) {
	principal, ok := subsystemPrincipal(writer, request)
	if !ok {
		return
	}
	applicationCode := strings.TrimSpace(request.URL.Query().Get("application_code"))
	environment := strings.ToLower(strings.TrimSpace(request.URL.Query().Get("environment")))
	if applicationCode == "" || environment == "" {
		handler.writeError(writer, request, application.ErrValidation)
		return
	}
	applicationID, environmentID, err := handler.service.ResolveApplicationEnvironment(request.Context(), principal.Tenant.ID, applicationCode, environment)
	if err != nil {
		handler.writeError(writer, request, err)
		return
	}
	response := KeycloakSyncStatusResponse{
		ApplicationCode: applicationCode,
		Environment:     environment,
		Realm:           handler.keycloakRealm,
	}
	// Get client mapping.
	if inspector, supported := handler.keycloakMappings.(keycloakClientMappingInspector); supported {
		mapping, loadErr := inspector.GetKeycloakClientMapping(request.Context(), principal.Tenant.ID, applicationID, environmentID)
		if loadErr != nil {
			handler.logger.Warn("load Keycloak client mapping for sync status failed",
				"application_code", applicationCode, "environment", environment, "error", loadErr)
			response.DriftError = "CLIENT_MAPPING_UNAVAILABLE"
		} else {
			response.ClientID = mapping.ClientID
			response.ClientState = mapping.Status
			response.LastSyncedAt = mapping.LastSyncedAt
		}
	}
	// Get readiness.
	readiness := handler.keycloakSwitchReadiness(request.Context(), principal.Tenant.ID, applicationCode, environment)
	response.SwitchReady = readiness.SwitchReady
	// Get drift detection if control plane is available.
	if handler.keycloakControl != nil && response.ClientID != "" {
		roleCodes, catalogErr := handler.loadKeycloakRoleCodesForDrift(request.Context(), principal.Tenant.ID, applicationID)
		if catalogErr != nil {
			handler.logger.Warn("load role catalog for drift check failed",
				"application_code", applicationCode, "environment", environment, "error", catalogErr)
			response.DriftError = "ROLE_CATALOG_UNAVAILABLE"
		} else {
			// Redirect URI is derived from the environment's base URL + path prefix +
			// /auth/callback. We pass empty here because the SyncClient endpoint does
			// full redirect URI validation; the dashboard focuses on role/mapper drift.
			redirectURI := ""
			drift, driftErr := handler.keycloakControl.DetectSubsystemKeycloakDrift(request.Context(), response.ClientID, redirectURI, roleCodes)
			if driftErr != nil {
				handler.logger.Warn("drift detection failed",
					"application_code", applicationCode, "environment", environment, "error", driftErr)
				response.DriftError = "DRIFT_DETECTION_FAILED"
			} else {
				response.DriftAvailable = true
				response.Drift = &struct {
					HasDrift       bool     `json:"has_drift"`
					MissingRoles   []string `json:"missing_roles,omitempty"`
					StaleRoles     []string `json:"stale_roles,omitempty"`
					MissingMappers []string `json:"missing_mappers,omitempty"`
					DriftedMappers []string `json:"drifted_mappers,omitempty"`
					RedirectURIOK  bool     `json:"redirect_uri_ok"`
					BrokerConfigOK bool     `json:"broker_config_ok"`
				}{
					HasDrift:       drift.HasDrift,
					MissingRoles:   drift.MissingRoles,
					StaleRoles:     drift.StaleRoles,
					MissingMappers: drift.MissingMappers,
					DriftedMappers: drift.DriftedMappers,
					RedirectURIOK:  drift.RedirectURIOK,
					BrokerConfigOK: drift.BrokerConfigOK,
				}
			}
		}
	}
	writer.Header().Set("Cache-Control", "no-store, private")
	httpresponse.WriteSuccess(writer, request, stdhttp.StatusOK, "Keycloak 同步状态查询成功", response)
}

// recoverStaleSubsystemDeployment closes the only lifecycle states that can be
// left behind by an API/Agent restart. The transition is deliberately performed
// from the authenticated status path, so an operator refreshing the page can
// recover the UI without a direct database operation or a second onboarding.
func (handler *SubsystemOnboardingHandler) recoverStaleSubsystemDeployment(ctx context.Context, state application.SubsystemDeploymentState) application.SubsystemDeploymentState {
	if !staleSubsystemDeployment(state, time.Now().UTC()) {
		return state
	}
	operation := strings.TrimSpace(state.Operation)
	if operation == "" {
		operation = "ONBOARD"
	}
	if err := handler.deploymentState.TransitionSubsystemDeployment(
		ctx, state.TenantID, state.ApplicationCode, state.Environment,
		application.SubsystemDeploymentStatusFailed, operation,
		"DEPLOYMENT_INTERRUPTED", "部署请求中断，请点击重试",
		time.Now().UTC(),
	); err != nil {
		handler.logger.Warn("stale subsystem deployment could not be recovered",
			"application_code", state.ApplicationCode, "environment", state.Environment, "error", err)
		return state
	}
	state.Status = application.SubsystemDeploymentStatusFailed
	state.LastErrorCode = "DEPLOYMENT_INTERRUPTED"
	state.LastError = "部署请求中断，请点击重试"
	state.CompletedAt = pointerToTime(time.Now().UTC())
	state.UpdatedAt = time.Now().UTC()
	return state
}

func staleSubsystemDeployment(state application.SubsystemDeploymentState, now time.Time) bool {
	if state.Status != application.SubsystemDeploymentStatusProvisioning &&
		state.Status != application.SubsystemDeploymentStatusUpdating &&
		state.Status != application.SubsystemDeploymentStatusVerifying {
		return false
	}
	if state.StartedAt == nil {
		return false
	}
	return now.Sub(state.StartedAt.UTC()) > subsystemDeploymentStaleAfter
}

func pointerToTime(value time.Time) *time.Time {
	return &value
}

func subsystemDeploymentStateNextAction(state application.SubsystemDeploymentState) string {
	if state.Status == application.SubsystemDeploymentStatusUnmanaged {
		return "该应用环境已登记，但尚未产生受控部署记录；请通过本地编排状态和健康检查确认服务运行。只有部署 Agent 已明确支持该应用时，才能执行受控运行时更新"
	}
	if state.Status != application.SubsystemDeploymentStatusFailed {
		return ""
	}
	if state.LastErrorCode == "INITIAL_ACCESS_ASSIGNMENT_FAILED" || state.LastErrorCode == "INITIAL_ACCESS_STATE_FAILED" {
		return "部署运行时已完成，但初始管理员授权尚未完成；确认目标用户有效且授权目录已同步后，点击“重试”"
	}
	return "请检查部署 Agent、Docker 和目标 API 健康状态，修复后在当前环境点击“重试”；不要重复新增接入"
}

func (handler *SubsystemOnboardingHandler) transitionDeployment(ctx context.Context, tenantID, applicationCode, environment, status, operation, errorCode, errorMessage string) error {
	if handler.deploymentState == nil {
		return nil
	}
	return handler.deploymentState.TransitionSubsystemDeployment(ctx, tenantID, applicationCode, environment, status, operation, errorCode, errorMessage, time.Now().UTC())
}

func (handler *SubsystemOnboardingHandler) markInitialAccessAssigned(ctx context.Context, tenantID, applicationCode, environment string) error {
	if handler.deploymentState == nil {
		return nil
	}
	return handler.deploymentState.MarkSubsystemInitialAccessAssigned(ctx, tenantID, applicationCode, environment, time.Now().UTC())
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

func (handler *SubsystemOnboardingHandler) writeError(writer stdhttp.ResponseWriter, request *stdhttp.Request, err error, provisioningStages ...string) {
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
			map[string]string{
				"next_action": subsystemProvisioningNextAction(err, provisioningStages...),
				"detail":      subsystemProvisioningDetail(err),
			},
		))
	default:
		handler.logger.Error("subsystem onboarding request failed", "path", request.URL.Path, "error", err)
		httpresponse.WriteError(writer, request, stdhttp.StatusInternalServerError, httperror.Internal)
	}
}

// subsystemProvisioningDetail 把脱敏后的 Agent 错误原文附到响应里，让页面直接显示目标 API
// 未能启动的具体日志原因；错误文本已被 Agent 侧去除明文凭据并限制长度。
func subsystemProvisioningDetail(err error) string {
	const limit = 4000
	message := strings.TrimSpace(err.Error())
	if len(message) > limit {
		message = message[:limit] + "...(truncated)"
	}
	return message
}

func subsystemProvisioningNextAction(err error, stages ...string) string {
	message := strings.ToLower(err.Error())
	diagnosis := "平台部署 Agent 或目标 API 暂时不可用"
	switch {
	case strings.Contains(message, "disabled"):
		diagnosis = "当前环境未启用受控部署 Agent；请升级并重新发布生产部署资产，确认 platform-api 与 subsystem-provisioner 均健康"
	case strings.Contains(message, "deployment helper is unavailable"), strings.Contains(message, "read deployment response"), strings.Contains(message, "send deployment request"):
		diagnosis = "平台 API 无法连接生产部署 Agent；请检查 subsystem-provisioner 状态和启动日志，并确认 Agent 与 platform-api 使用同一版本"
	case strings.Contains(message, "target is not allowed"):
		diagnosis = "该应用/环境未被当前 Agent 的审核清单允许，或 Agent 仍运行旧版本；请同步最新 subsystems.d 清单并同时重建 platform-api、subsystem-provisioner"
	case strings.Contains(message, "tenant is not allowed"):
		diagnosis = "当前租户不是该服务器绑定的生产租户；请核对 SUBSYSTEM_PRODUCTION_ALLOWED_TENANT_ID，禁止用其他租户覆盖现有实例"
	case strings.Contains(message, "preflight values are inconsistent"), strings.Contains(message, "integration values are inconsistent"), strings.Contains(message, "deployment request is invalid"):
		diagnosis = "页面提交的应用编码、环境、回调地址或上游地址与服务器审核清单不一致；请刷新应用接入页面并重新选择服务器接入目标"
	case strings.Contains(message, "infrastructure secrets are incomplete"):
		diagnosis = "服务器仍运行会在预检阶段检查全部基础设施密钥的旧版 Agent；请同步最新生产资产并同时重建 platform-api、subsystem-provisioner"
	case strings.Contains(message, "subsystem database credentials are incomplete"):
		diagnosis = "当前子系统实际使用的数据库凭据仍为空或占位值；这些凭据可能已绑定持久化数据，Agent 不会自动生成或轮换，请完成一次性数据库初始化"
	case strings.Contains(message, "generated runtime secret is invalid"):
		diagnosis = "目标 runtime 中已有业务密钥不是合法的 32 字节 base64 值；为避免误轮换，Agent 不会覆盖非占位值，请先备份并修正该异常值"
	case strings.Contains(message, "runtime secrets are incomplete"):
		diagnosis = "目标 runtime 仍缺少必须由前置子系统接入产生的凭据；请先完成依赖子系统接入，再重试当前目标"
	case strings.Contains(message, "runtime template"), strings.Contains(message, "runtime directory"):
		diagnosis = "Agent 无法从随发布包审核的模板初始化 runtime；请确认最新 *.env.example 已部署且 runtime 目录可写，Agent 会自动创建文件并收紧为 0600"
	case strings.Contains(message, "production environment is unavailable"):
		diagnosis = "目标子系统 runtime 尚不可用；新版 Agent 会从审核模板自动初始化，请确认 platform-api、subsystem-provisioner 和生产部署资产版本一致"
	case strings.Contains(message, "release environment is unavailable"), strings.Contains(message, "immutable digest"):
		diagnosis = "目标子系统尚未发布有效的不可变镜像 digest；请先完成该子系统镜像发布并确认 .release.env 可读"
	case strings.Contains(message, "initial administrator role"):
		diagnosis = "目标运行时已启动，但权限目录中缺少可用的初始角色；请检查目标 API 的目录同步日志"
	case strings.Contains(message, "production deployment file"), strings.Contains(message, "production deployment directory"), strings.Contains(message, "production compose configuration"):
		diagnosis = "服务器生产部署资产缺失、路径不规范或 Compose 校验失败；请重新发布平台生产部署资产并确认 Agent 健康"
	case strings.Contains(message, "contract_summary_client_id"), strings.Contains(message, "contract_summary_client_secret"), strings.Contains(message, "contract_summary_url"):
		diagnosis = "合同摘要校验已开启但运行时凭据不完整；重试不会从平台数据库恢复 OAuth 明文，请先同步包含合同摘要服务绑定的生产接入资产，或关闭合同校验后在当前环境重试"
	case strings.Contains(message, "runtime environment") || strings.Contains(message, "runtime configuration"):
		diagnosis = "Agent 无法安全更新目标运行配置；请确认 runtime 目录可写且文件不是符号链接，普通权限过宽会由 Agent 自动收紧为 0600"
	case strings.Contains(message, "start production subsystem dependencies"):
		diagnosis = "目标子系统的数据库或依赖服务未通过健康检查；请查看 subsystem-provisioner、目标数据库和依赖容器日志"
	case strings.Contains(message, "backup production subsystem database"), strings.Contains(message, "prepare production subsystem backup"):
		diagnosis = "目标子系统数据库备份失败；请检查数据库健康状态与 backups 目录权限"
	case strings.Contains(message, "migrate production subsystem database"):
		diagnosis = "目标子系统数据库迁移失败；请查看对应 migrate 容器日志并处理迁移错误，必要时使用接入前备份恢复"
	case strings.Contains(message, "start production subsystem services"):
		diagnosis = "目标 API 未能启动或通过健康检查；请查看目标 API 容器日志、runtime 配置和目录同步日志"
	case strings.Contains(message, "write production subsystem runtime configuration"):
		diagnosis = "Agent 无法安全写入目标 runtime 环境文件；请检查文件属主、0600 权限和 runtime 目录可写性"
	case strings.Contains(message, "production deployment lock is unavailable"):
		diagnosis = "服务器上已有发布或接入任务占用部署锁；请等待该任务结束，不要并行执行发布"
	case strings.Contains(message, "service credential is incomplete"), strings.Contains(message, "integration credential is incomplete"):
		diagnosis = "服务器审核清单引用的用途凭据尚未由平台控制面创建或交付；请同步最新平台镜像与清单，并确认 Agent 与 platform-api 版本一致"
	case strings.Contains(message, "compose file"):
		diagnosis = "部署 Agent 未找到生产 compose.yaml；请重新发布完整 platform/deploy/production 资产，并同时重建 platform-api、subsystem-provisioner"
	case strings.Contains(message, "environment template"):
		diagnosis = "部署 Agent 未找到目标运行配置模板；请重新发布完整生产部署资产并确认 runtime/*.env 已初始化"
	case strings.Contains(message, "docker service"):
		diagnosis = "Docker 服务不可用；请启动 Docker 并确认部署 Agent 可以访问 Docker Socket"
	case strings.Contains(message, "start subsystem containers"), strings.Contains(message, "rebuild subsystem containers"):
		diagnosis = "子系统构建或启动失败；请查看 subsystem-provisioner 与目标 API 容器日志"
	}
	if len(stages) > 0 && stages[0] == "preflight" {
		return diagnosis + "；修复后重新提交本次接入。该失败发生在预检阶段，平台尚未创建应用环境"
	}
	return diagnosis + "；修复后在当前环境点击“重试”，不要重复创建应用环境"
}
