package bootstrap

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	applicationregistryapplication "github.com/J-S-Te/Basic-Platform/internal/platform/applicationregistry/application"
	applicationregistryinfrastructure "github.com/J-S-Te/Basic-Platform/internal/platform/applicationregistry/infrastructure"
	applicationregistryhttp "github.com/J-S-Te/Basic-Platform/internal/platform/applicationregistry/interfaces/http"
	applicationaccess "github.com/J-S-Te/Basic-Platform/internal/platform/authorization/applicationaccess"
	filetaskapplication "github.com/J-S-Te/Basic-Platform/internal/platform/filetask/application"
	filetaskinfrastructure "github.com/J-S-Te/Basic-Platform/internal/platform/filetask/infrastructure"
	filetaskhttp "github.com/J-S-Te/Basic-Platform/internal/platform/filetask/interfaces/http"
	identityapplication "github.com/J-S-Te/Basic-Platform/internal/platform/identity/application"
	identityinfrastructure "github.com/J-S-Te/Basic-Platform/internal/platform/identity/infrastructure"
	identityhttp "github.com/J-S-Te/Basic-Platform/internal/platform/identity/interfaces/http"
	keycloakauthorizationinfrastructure "github.com/J-S-Te/Basic-Platform/internal/platform/keycloakauthorization/infrastructure"
	notificationapplication "github.com/J-S-Te/Basic-Platform/internal/platform/notification/application"
	notificationdomain "github.com/J-S-Te/Basic-Platform/internal/platform/notification/domain"
	notificationinfrastructure "github.com/J-S-Te/Basic-Platform/internal/platform/notification/infrastructure"
	notificationhttp "github.com/J-S-Te/Basic-Platform/internal/platform/notification/interfaces/http"
	"github.com/J-S-Te/Basic-Platform/internal/shared/config"
	"github.com/J-S-Te/Basic-Platform/internal/shared/ulid"
	httptransport "github.com/J-S-Te/Basic-Platform/internal/transport/http"
	"gorm.io/gorm"
)

// buildOperationalModules 只通过各模块公开的应用契约完成装配，不让路由层接触仓储实现。
// 这里同样禁止 AutoMigrate：进程启动与 schema 发布必须保持两个独立生命周期。
func buildOperationalModules(cfg config.Config, database *gorm.DB, logger *slog.Logger, applicationAccessService *applicationaccess.Service, applicationManagementService *applicationregistryapplication.ManagementService, oauthClientManagementService *applicationregistryapplication.OAuthClientManagementService) (httptransport.OperationalModules, error) {
	if database == nil || logger == nil || applicationAccessService == nil {
		return httptransport.OperationalModules{}, errors.New("operational module dependencies must not be nil")
	}

	loginTargetRepository, err := applicationregistryinfrastructure.NewLoginTargetGORMRepository(database)
	if err != nil {
		return httptransport.OperationalModules{}, err
	}
	loginTargetService, err := applicationregistryapplication.NewLoginTargetManagementService(loginTargetRepository, ulid.Generator{}, applicationregistryapplication.SystemClock{})
	if err != nil {
		return httptransport.OperationalModules{}, err
	}
	loginTargetHandler, err := applicationregistryhttp.NewLoginTargetManagementHandler(loginTargetService, logger)
	if err != nil {
		return httptransport.OperationalModules{}, err
	}

	subsystemRepository, err := applicationregistryinfrastructure.NewSubsystemOnboardingGORMRepository(database)
	if err != nil {
		return httptransport.OperationalModules{}, err
	}
	subsystemServiceRouteHandler, err := applicationregistryhttp.NewSubsystemServiceRouteHandler(subsystemRepository)
	if err != nil {
		return httptransport.OperationalModules{}, err
	}
	subsystemService, err := applicationregistryapplication.NewSubsystemOnboardingService(
		subsystemRepository, ulid.Generator{}, applicationregistryapplication.SystemClock{},
		applicationregistryapplication.RedirectURIValidationPolicy{
			AllowInsecureHTTP: cfg.Auth.OAuthClientAllowInsecureHTTPRedirectURIs,
		},
	)
	if err != nil {
		return httptransport.OperationalModules{}, err
	}
	provisioningCapabilities := applicationregistryapplication.SubsystemProvisioningCapabilities{
		Enabled: cfg.SubsystemOnboarding.Enabled,
		Mode:    cfg.SubsystemOnboarding.Mode,
	}
	// 生产环境的可接入目标来自随发布包审核的清单；本地环境则使用固定的开发能力集合。
	// 这里先解析能力，再注入 handler，保证页面展示的选项与 Agent 实际允许的目标一致。
	if strings.EqualFold(strings.TrimSpace(cfg.SubsystemOnboarding.Mode), "production") {
		if cfg.SubsystemOnboarding.Enabled {
			provisioningCapabilities, err = applicationregistryinfrastructure.LoadProductionSubsystemCapabilities(
				cfg.SubsystemOnboarding.ProductionDeployRoot,
				cfg.SubsystemOnboarding.ProductionProfilesDirectory,
			)
			if err != nil {
				return httptransport.OperationalModules{}, fmt.Errorf("load production subsystem capabilities: %w", err)
			}
			provisioningCapabilities.Enabled = true
		}
	} else {
		provisioningCapabilities.SupportedEnvironments = []string{"dev", "test", "staging", "prod"}
		provisioningCapabilities.DefaultEnvironment = "dev"
		provisioningCapabilities.DefaultClientType = "confidential"
	}
	subsystemProvisioner, err := applicationregistryinfrastructure.NewUnixSocketSubsystemProvisioner(
		cfg.SubsystemOnboarding.Enabled,
		cfg.SubsystemOnboarding.SocketPath,
		cfg.SubsystemOnboarding.Timeout,
		provisioningCapabilities,
	)
	if err != nil {
		return httptransport.OperationalModules{}, err
	}
	// API 进程只持有 Unix Socket 客户端；Docker Socket、Compose 文件和宿主机写权限均留在
	// 隔离 Agent 中。这样即使管理 API 被调用，也不能直接把平台容器权限扩大为宿主机权限。
	notificationRepository, err := notificationinfrastructure.NewRepository(database)
	if err != nil {
		return httptransport.OperationalModules{}, err
	}
	inboxPolicy, err := notificationinfrastructure.NewInboxPolicy(database)
	if err != nil {
		return httptransport.OperationalModules{}, err
	}
	recipientResolver, err := notificationinfrastructure.NewRecipientResolver(database)
	if err != nil {
		return httptransport.OperationalModules{}, err
	}
	notificationService, err := notificationapplication.NewService(notificationRepository, inboxPolicy, recipientResolver, ulid.Generator{}, notificationapplication.SystemClock{})
	if err != nil {
		return httptransport.OperationalModules{}, err
	}
	// API 只持有受限 Unix Socket 客户端，Docker Socket 和宿主机文件写权限留在隔离的部署 Agent；
	// 这样后台管理权限不会直接等价为容器宿主机权限。接入生命周期通知依赖通知服务，先装配通知服务。
	subsystemHandler, err := applicationregistryhttp.NewSubsystemOnboardingHandlerWithNotifications(
		subsystemService,
		subsystemProvisioner,
		subsystemInitialAccessManager{
			applicationAccess:              applicationAccessService,
			initialAdminRolesByApplication: initialAdminRolesByApplication(provisioningCapabilities),
			fromManifest:                   cfg.SubsystemOnboarding.InitialAdminRolesFromManifest,
			logger:                         logger,
		},
		cfg.Auth.OIDCIssuer,
		logger,
		onboardingNotificationSink{service: notificationService},
		subsystemRepository,
	)
	if err != nil {
		return httptransport.OperationalModules{}, err
	}
	// Keycloak is optional.  Keeping this explicit means a page can show it as
	// unavailable instead of letting an operator save an issuer alias that the
	// deployment Agent cannot honour.
	subsystemHandler.ConfigureKeycloak(
		cfg.Keycloak.Enabled,
		keycloakRealmIssuer(cfg.Keycloak.PublicURL, cfg.Keycloak.Realm),
		cfg.Keycloak.Realm,
	)
	subsystemHandler.ConfigureDefaultIssuerAlias(cfg.SubsystemOnboarding.DefaultIssuerAlias)
	if cfg.Keycloak.Enabled {
		mappingStore, mappingErr := keycloakauthorizationinfrastructure.NewClientMappingStore(database)
		if mappingErr != nil {
			return httptransport.OperationalModules{}, mappingErr
		}
		subsystemHandler.ConfigureKeycloakControlPlane(applicationregistryhttp.NewKeycloakControlPlaneWithCredentials(
			cfg.Keycloak.AdminURL, cfg.Keycloak.Realm, applicationregistryhttp.KeycloakControlPlaneCredentials{
				ServiceAccountClientID:     cfg.Keycloak.AdminClientID,
				ServiceAccountClientSecret: cfg.Keycloak.AdminClientSecret,
				Username:                   cfg.Keycloak.AdminUsername,
				Password:                   cfg.Keycloak.AdminPassword,
			},
			cfg.Keycloak.BrokerClientID, cfg.Keycloak.BrokerClientSecret, cfg.Auth.OIDCIssuer, cfg.Keycloak.PlatformBackchannelURL,
		))
		subsystemHandler.ConfigureKeycloakBroker(keycloakBrokerRegistrar{applications: applicationManagementService, oauth: oauthClientManagementService, publicURL: cfg.Keycloak.PublicURL, realm: cfg.Keycloak.Realm})
		subsystemHandler.ConfigureKeycloakAuthorizationCatalog(keycloakAuthorizationCatalogAdapter{service: applicationAccessService})
		subsystemHandler.ConfigureKeycloakClientMappingStore(mappingStore)
		readinessStore, readinessErr := keycloakauthorizationinfrastructure.NewSwitchReadinessStore(database)
		if readinessErr != nil {
			return httptransport.OperationalModules{}, readinessErr
		}
		subsystemHandler.ConfigureKeycloakSwitchReadinessInspector(readinessStore)
	}
	notificationHandler, err := notificationhttp.NewHandler(notificationService, logger)
	if err != nil {
		return httptransport.OperationalModules{}, err
	}

	localStore, err := filetaskinfrastructure.NewLocalStore(cfg.FileStorageRoot)
	if err != nil {
		return httptransport.OperationalModules{}, err
	}
	fileRepository, err := filetaskinfrastructure.NewGORMRepository(database)
	if err != nil {
		return httptransport.OperationalModules{}, err
	}
	fileService, err := filetaskapplication.NewFileService(fileRepository, localStore, ulid.Generator{}, filetaskapplication.SystemClock{}, filetaskapplication.DefaultUploadPolicy())
	if err != nil {
		return httptransport.OperationalModules{}, err
	}
	jobService, err := filetaskapplication.NewJobService(fileRepository, ulid.Generator{}, filetaskapplication.SystemClock{})
	if err != nil {
		return httptransport.OperationalModules{}, err
	}
	fileTaskHandler, err := filetaskhttp.NewHandler(fileService, jobService, logger)
	if err != nil {
		return httptransport.OperationalModules{}, err
	}

	personnelRepo := identityinfrastructure.NewPersonnelChangeGORMRepository(database)
	personnelHandoverChecker, err := identityinfrastructure.NewPersonnelHandoverGORMChecker(database)
	if err != nil {
		return httptransport.OperationalModules{}, err
	}
	// 人员异动服务与通知、交接检查在组合根统一装配；HTTP handler 和后台 worker 随后共享
	// 同一个 service，保证状态机校验不会因入口不同而产生分叉。
	personnelService, err := identityapplication.NewPersonnelChangeService(personnelRepo, ulid.Generator{}, identityapplication.SystemClock{}, personnelHandoverChecker)
	if err != nil {
		return httptransport.OperationalModules{}, err
	}
	personnelService.SetNotifier(notificationService)
	personnelHandler := identityhttp.NewPersonnelChangeHandler(personnelService)

	return httptransport.OperationalModules{
		LoginTargets:           loginTargetHandler,
		SubsystemOnboarding:    subsystemHandler,
		SubsystemServiceRoutes: subsystemServiceRouteHandler,
		Notifications:          notificationHandler,
		FilesAndJobs:           fileTaskHandler,
		AccessApplier:          subsystemProvisioner,
		PersonnelChanges:       personnelHandler,
		AuthorizationOverview:  identityhttp.NewAuthorizationOverviewHandler(database),
	}, nil
}

type keycloakAuthorizationCatalogAdapter struct{ service *applicationaccess.Service }

func (adapter keycloakAuthorizationCatalogAdapter) ListKeycloakRoleCodes(ctx context.Context, tenantID, applicationID string) ([]string, error) {
	view, err := adapter.service.GetCatalog(ctx, tenantID, applicationID)
	if err != nil {
		return nil, err
	}
	roles := make([]string, 0, len(view.Roles))
	for _, role := range view.Roles {
		roles = append(roles, role.Code)
	}
	return roles, nil
}

// subsystemInitialAccessManager 在子系统目录已经发布时分配约定的初始管理员角色。
// 真正的角色有效性仍由通用授权服务校验，接入流程不能绕开可委派权限和角色目录边界。
type subsystemInitialAccessManager struct {
	applicationAccess *applicationaccess.Service
	// initialAdminRolesByApplication 由 subsystems.d 清单声明（B1 解耦）；fromManifest=false
	// 时仍走平台硬编码默认，保证既有行为不变。
	initialAdminRolesByApplication map[string][]string
	fromManifest                   bool
	logger                         *slog.Logger
}

// initialAdminRolesByApplication 从能力列表构建“应用编码 → 初始管理员角色”映射；
// 仅收集清单显式声明了 initial_admin_roles 的目标。
func initialAdminRolesByApplication(capabilities applicationregistryapplication.SubsystemProvisioningCapabilities) map[string][]string {
	rolesByApplication := make(map[string][]string)
	for _, target := range capabilities.Targets {
		if target.InitialAdminRoles == nil {
			continue
		}
		rolesByApplication[target.ApplicationCode] = append([]string(nil), target.InitialAdminRoles...)
	}
	return rolesByApplication
}

// onboardingNotificationSink 把子系统接入生命周期事件转成租户内站内通知，投递给操作人。
type onboardingNotificationSink struct {
	service *notificationapplication.Service
}

func (sink onboardingNotificationSink) SendSubsystemLifecycle(ctx context.Context, input applicationregistryhttp.SubsystemLifecycleNotification) error {
	if sink.service == nil {
		return nil
	}
	templateCode := "subsystem.lifecycle.succeeded"
	if !input.Succeeded {
		templateCode = "subsystem.lifecycle.failed"
	}
	variables := map[string]string{
		"application_name": input.ApplicationName,
		"application_code": input.ApplicationCode,
		"environment":      input.Environment,
	}
	if input.Detail != "" {
		variables["detail"] = input.Detail
	}
	idempotency := fmt.Sprintf("subsystem.lifecycle.%s.%s.%s.%d", input.ApplicationCode, input.Environment, input.OperatorID, time.Now().UTC().UnixNano())
	_, err := sink.service.Create(ctx, notificationapplication.CreateInput{
		TenantID:       input.TenantID,
		OperatorID:     input.OperatorID,
		TemplateCode:   templateCode,
		Category:       "subsystem",
		Variables:      variables,
		Recipients:     []notificationdomain.RecipientTarget{{Type: notificationdomain.RecipientTypeUser, ID: input.OperatorID}},
		IdempotencyKey: idempotency,
	})
	return err
}

func (manager subsystemInitialAccessManager) AssignInitialAdministrator(
	ctx context.Context,
	tenantID string,
	applicationCode string,
	userID string,
	operatorID string,
) (string, error) {
	if manager.applicationAccess == nil {
		return "", errors.New("application authorization service is unavailable")
	}
	roleCodes := manager.initialAdministratorRoles(applicationCode)
	// customer_portal 面向外部客户：执行接入的内部管理员不能自动获得客户角色或门户入口，
	// 外部客户身份和数据范围必须由 CRM 邀请流程逐个建立。
	if len(roleCodes) == 0 {
		return "", nil
	}
	roles := make([]applicationaccess.RoleInput, 0, len(roleCodes))
	for _, roleCode := range roleCodes {
		roles = append(roles, applicationaccess.RoleInput{RoleCode: roleCode, ScopeType: "APPLICATION"})
	}
	_, err := manager.applicationAccess.UpdateAccess(ctx, applicationaccess.UpdateAccessInput{
		TenantID: tenantID, UserID: userID, OperatorID: operatorID, Roles: roles, RolesProvided: true,
	}, applicationCode)
	if err != nil {
		// Provision returns only after the subsystem has published its role catalog. Treat a missing
		// or disabled administrator role as an incomplete deployment instead of silently marking the
		// environment READY without granting the operator access. Retry remains safe and idempotent.
		if errors.Is(err, applicationaccess.ErrValidation) {
			return "", fmt.Errorf("%w: initial administrator role is unavailable", applicationregistryapplication.ErrSubsystemProvisioningUnavailable)
		}
		switch {
		case errors.Is(err, applicationaccess.ErrNotFound):
			return "", applicationregistryapplication.ErrNotFound
		default:
			return "", err
		}
	}
	return strings.Join(roleCodes, ","), nil
}

// hardcodedInitialSubsystemAdministratorRoles 是平台内置默认：未开启清单驱动或清单未声明时
// 使用，保证既有子系统行为不变。
func hardcodedInitialSubsystemAdministratorRoles(applicationCode string) []string {
	switch strings.TrimSpace(applicationCode) {
	case "customer_and_opportunity":
		// CRM 不创建绕过业务范围的“万能管理员”。三个目录角色共同覆盖运营职责，同时仍受
		// max_effective_roles=3 和各角色数据范围约束。
		return []string{"sales_director", "team_lead", "technical_lead"}
	case "customer_portal":
		return nil
	}
	return []string{"admin"}
}

// initialAdministratorRoles 在开启清单驱动时优先使用清单声明的角色；清单未声明或开关关闭时
// 回退到平台硬编码默认。清单值与默认不一致只记 WARN，不阻断接入（兜底：行为始终可预期）。
func (manager subsystemInitialAccessManager) initialAdministratorRoles(applicationCode string) []string {
	code := strings.TrimSpace(applicationCode)
	if manager.fromManifest {
		if roles, declared := manager.initialAdminRolesByApplication[code]; declared {
			defaultRoles := hardcodedInitialSubsystemAdministratorRoles(code)
			if !equalStringSlices(roles, defaultRoles) {
				logger := manager.logger
				if logger == nil {
					logger = slog.Default()
				}
				logger.Warn("subsystem initial admin roles differ from platform default",
					"application_code", code, "manifest_roles", roles, "default_roles", defaultRoles)
			}
			return roles
		}
	}
	return hardcodedInitialSubsystemAdministratorRoles(code)
}

func equalStringSlices(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}

func keycloakRealmIssuer(publicURL, realm string) string {
	publicURL = strings.TrimRight(strings.TrimSpace(publicURL), "/")
	realm = strings.Trim(strings.TrimSpace(realm), "/")
	if publicURL == "" || realm == "" {
		return ""
	}
	return publicURL + "/realms/" + realm
}
