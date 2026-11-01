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
	keycloakauthorizationapplication "github.com/J-S-Te/Basic-Platform/internal/platform/keycloakauthorization/application"
	keycloakauthorizationinfrastructure "github.com/J-S-Te/Basic-Platform/internal/platform/keycloakauthorization/infrastructure"
	notificationapplication "github.com/J-S-Te/Basic-Platform/internal/platform/notification/application"
	notificationdomain "github.com/J-S-Te/Basic-Platform/internal/platform/notification/domain"
	notificationinfrastructure "github.com/J-S-Te/Basic-Platform/internal/platform/notification/infrastructure"
	notificationhttp "github.com/J-S-Te/Basic-Platform/internal/platform/notification/interfaces/http"
	tenantclone "github.com/J-S-Te/Basic-Platform/internal/platform/tenantclone"
	tenantclonehttp "github.com/J-S-Te/Basic-Platform/internal/platform/tenantclone/interfaces/http"
	"github.com/J-S-Te/Basic-Platform/internal/shared/config"
	"github.com/J-S-Te/Basic-Platform/internal/shared/ulid"
	httptransport "github.com/J-S-Te/Basic-Platform/internal/transport/http"
	"gorm.io/gorm"
)

// buildOperationalModules 只通过各模块公开的应用契约完成装配，不让路由层接触仓储实现。
// 参数：cfg限定部署与认证，database为平台库，logger负责追踪；
//
//	applicationAccessService解析入口，applicationManagementService管理目录，
//	oauthClientManagementService管理凭据，personnelAudit及personnelHandover负责人员审计与交接。
//
// 返回值：完整运行模块；依赖或接线失败返回错误，启动不执行AutoMigrate。
func buildOperationalModules(cfg config.Config, database *gorm.DB, logger *slog.Logger, applicationAccessService *applicationaccess.Service, applicationManagementService *applicationregistryapplication.ManagementService, oauthClientManagementService *applicationregistryapplication.OAuthClientManagementService, personnelAudit identityapplication.PersonnelChangeAuditSink, personnelHandover *identityapplication.HandoverOrchestrator) (httptransport.OperationalModules, error) {
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
	subsystemServiceRouteHandler, err := applicationregistryhttp.NewSubsystemServiceRouteHandler(subsystemRepository, applicationAccessService)
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
	// 清单与部署 Agent 客户端的解析提取为共用函数：API 与 Worker 使用同一份口径。
	provisioningCapabilities, err := resolveSubsystemProvisioningCapabilities(cfg)
	if err != nil {
		return httptransport.OperationalModules{}, err
	}
	subsystemProvisioner, err := newSubsystemAccessProvisioner(cfg)
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
	centralSessionPolicy, err := applicationregistryapplication.NewCentralSessionRuntimePolicy(cfg.Auth.CentralSessionEnabled, cfg.HTTP.PublicBaseURL)
	if err != nil {
		return httptransport.OperationalModules{}, err
	}
	subsystemHandler.ConfigureCentralSessionPolicy(*centralSessionPolicy)
	// Keycloak 是可选模块。保持显式声明，让页面可以把它显示为"不可用"，
	// 而不是让运维保存一条部署 Agent 无法兑现的 issuer 别名。
	subsystemHandler.ConfigureKeycloak(
		cfg.Keycloak.Enabled,
		keycloakRealmIssuer(cfg.Keycloak.PublicURL, cfg.Keycloak.Realm),
		cfg.Keycloak.Realm,
	)
	subsystemHandler.ConfigureKeycloakTransportPolicy(cfg.Keycloak.RequireHTTPS)

	// 统一接入向导：操作/阶段仓储 + 事实与就绪适配器 + 服务 + 处理器。
	// 部署 Agent（subsystemProvisioner）同时提供能力清单与运行批准事实，二者来自同一受控来源；
	// 镜像与认证观察期待接入时以"未验证"上报，不默认就绪。
	subsystemAccessRepository, err := applicationregistryinfrastructure.NewSubsystemAccessOperationGORMRepository(database)
	if err != nil {
		return httptransport.OperationalModules{}, err
	}
	// 镜像就绪由部署 Agent 上报（协议只增不改）：未上报的旧 Agent 保持「未验证」。
	subsystemImageReadiness, err := applicationregistryinfrastructure.NewSubsystemAccessImageReadinessAdapter(subsystemProvisioner)
	if err != nil {
		return httptransport.OperationalModules{}, err
	}
	subsystemAccessReadiness, err := applicationregistryinfrastructure.NewSubsystemAccessReadinessAdapter(database, subsystemProvisioner, subsystemImageReadiness, subsystemHandler)
	if err != nil {
		return httptransport.OperationalModules{}, err
	}
	subsystemConfiguration, err := applicationregistryinfrastructure.NewSubsystemAccessConfigurationTracker(database, subsystemProvisioner, subsystemProvisioner)
	if err != nil {
		return httptransport.OperationalModules{}, err
	}
	if strings.EqualFold(cfg.SubsystemOnboarding.Mode, "production") {
		subsystemAccessReadiness.SetBuiltinRuntimeSource(applicationregistryinfrastructure.BuiltinRuntimeComponentCatalog{})
		subsystemConfiguration.SetBuiltinRuntimeSource(applicationregistryinfrastructure.BuiltinRuntimeComponentCatalog{}, subsystemProvisioner)
	}
	subsystemConfiguration.SetRuntimeRegistrationCheck(subsystemProvisioner)
	subsystemAccessReadiness.SetConfigurationDriftReader(subsystemConfiguration)
	// 登记查询器与阶段执行器的目录登记端口共用同一实现，保证口径一致。
	subsystemAccessRegistry, err := applicationregistryinfrastructure.NewSubsystemAccessRegistryGORMReader(database)
	if err != nil {
		return httptransport.OperationalModules{}, err
	}
	subsystemAccessFacts, err := applicationregistryinfrastructure.NewSubsystemAccessFactsAdapter(database, subsystemProvisioner, subsystemAccessReadiness, subsystemAccessRegistry)
	if err != nil {
		return httptransport.OperationalModules{}, err
	}
	subsystemAccessService, err := applicationregistryapplication.NewSubsystemAccessService(
		subsystemAccessRepository, subsystemAccessFacts, ulid.Generator{}, applicationregistryapplication.SystemClock{})
	if err != nil {
		return httptransport.OperationalModules{}, err
	}
	manualRecovery, err := applicationregistryinfrastructure.NewSubsystemAccessManualRecoveryAdapter(database, subsystemProvisioner)
	if err != nil {
		return httptransport.OperationalModules{}, err
	}
	subsystemAccessService.SetManualRecovery(manualRecovery)
	subsystemAccessHandler, err := applicationregistryhttp.NewSubsystemAccessHandler(subsystemAccessService, logger)
	if err != nil {
		return httptransport.OperationalModules{}, err
	}
	subsystemHandler.ConfigureDefaultIssuerAlias(cfg.SubsystemOnboarding.DefaultIssuerAlias)
	subsystemHandler.ConfigureSubsystemServiceCredentials(oauthClientManagementService)
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
		).WithCentralSessionGovernance(cfg.Auth.CentralSessionEnabled))
		subsystemHandler.ConfigureKeycloakBroker(keycloakBrokerRegistrar{applications: applicationManagementService, oauth: oauthClientManagementService, publicURL: cfg.Keycloak.PublicURL, realm: cfg.Keycloak.Realm, environment: keycloakBrokerEnvironment(cfg.Environment)})
		subsystemHandler.ConfigureKeycloakAuthorizationCatalog(keycloakAuthorizationCatalogAdapter{service: applicationAccessService})
		subsystemHandler.ConfigureKeycloakClientMappingStore(mappingStore)
		readinessStore, readinessErr := keycloakauthorizationinfrastructure.NewSwitchReadinessStore(database)
		if readinessErr != nil {
			return httptransport.OperationalModules{}, readinessErr
		}
		subsystemHandler.ConfigureKeycloakSwitchReadinessInspector(readinessStore)
		cutoverLifecycleStore, lifecycleErr := keycloakauthorizationinfrastructure.NewCutoverLifecycleStore(database)
		if lifecycleErr != nil {
			return httptransport.OperationalModules{}, lifecycleErr
		}
		subsystemHandler.ConfigureKeycloakCutoverLifecycle(cutoverLifecycleStore)
		projectionOperationsStore, operationsErr := keycloakauthorizationinfrastructure.NewProjectionOperationsStore(database)
		if operationsErr != nil {
			return httptransport.OperationalModules{}, operationsErr
		}
		projectionOperations, operationsErr := keycloakauthorizationapplication.NewOperations(projectionOperationsStore)
		if operationsErr != nil {
			return httptransport.OperationalModules{}, operationsErr
		}
		subsystemHandler.ConfigureKeycloakProjectionOperations(projectionOperations)
	}
	notificationHandler, err := notificationhttp.NewHandler(notificationService, logger)
	if err != nil {
		return httptransport.OperationalModules{}, err
	}

	fileRepository, err := filetaskinfrastructure.NewGORMRepository(database)
	if err != nil {
		return httptransport.OperationalModules{}, err
	}
	jobService, err := filetaskapplication.NewJobService(fileRepository, ulid.Generator{}, filetaskapplication.SystemClock{})
	if err != nil {
		return httptransport.OperationalModules{}, err
	}
	fileTaskHandler, err := filetaskhttp.NewJobHandler(jobService, logger)
	if err != nil {
		return httptransport.OperationalModules{}, err
	}

	personnelRepo := identityinfrastructure.NewPersonnelChangeGORMRepository(database)
	personnelHandoverChecker, err := identityinfrastructure.NewPersonnelHandoverGORMChecker(database)
	if err != nil {
		return httptransport.OperationalModules{}, err
	}
	// HTTP 入口在组合根装配通知与交接检查；独立 Worker 进程会装配同一仓储和状态机，
	// 并单独注入通知服务，避免进程边界造成执行完成通知丢失。
	personnelService, err := identityapplication.NewPersonnelChangeService(personnelRepo, ulid.Generator{}, identityapplication.SystemClock{}, personnelHandoverChecker)
	if err != nil {
		return httptransport.OperationalModules{}, err
	}
	personnelService.SetNotifier(notificationService)
	// 注入审计写入端后超管例外审批才可用；缺失时身份服务按 fail-closed 拒绝例外。
	personnelService.SetAuditSink(personnelAudit)
	// 仅在配置启用时注入跨系统交接编排；未启用时保持既有 HandoverChecker 闸门行为。
	if personnelHandover != nil {
		personnelService.SetHandoverCoordinator(personnelHandover)
	}
	personnelHandler := identityhttp.NewPersonnelChangeHandler(personnelService)
	keycloakIntegrationHandler, err := applicationregistryhttp.NewKeycloakIntegrationHandler(subsystemHandler)
	if err != nil {
		return httptransport.OperationalModules{}, err
	}
	tenantCloneHandler, err := tenantclonehttp.NewHandler(&tenantclone.Service{DB: database})
	if err != nil {
		return httptransport.OperationalModules{}, err
	}

	return httptransport.OperationalModules{
		LoginTargets:        loginTargetHandler,
		SubsystemOnboarding: subsystemHandler,
		SubsystemAccess:     subsystemAccessHandler,
		KeycloakIntegration: keycloakIntegrationHandler,
		// 组合根按职责显式注入边界；当前实现仍由旧编排适配器承载，后续可无协议变更替换为独立 Handler。
		BrokerHandler:            subsystemHandler,
		DeploymentStatusHandler:  subsystemHandler,
		SubsystemServiceRoutes:   subsystemServiceRouteHandler,
		Notifications:            notificationHandler,
		AsyncJobs:                fileTaskHandler,
		AccessApplier:            subsystemProvisioner,
		PersonnelChanges:         personnelHandler,
		AuthorizationOverview:    identityhttp.NewAuthorizationOverviewHandler(database),
		TenantAuthorizationClone: tenantCloneHandler,
	}, nil
}

type keycloakAuthorizationCatalogAdapter struct{ service *applicationaccess.Service }

// ListKeycloakRoleCodes 返回应用当前授权目录中的全部角色编码，供 Keycloak
// Client 角色对账使用。
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

// SendSubsystemLifecycle 把子系统接入生命周期事件投递为站内通知；通知服务未
// 配置时静默跳过（可选依赖）。
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

// AssignInitialAdministrator 给接入操作人授予该应用清单声明的初始管理员角色；
// 无声明角色时直接返回成功，不写入任何绑定。
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
	// customer_portal 的接入操作人只会获得独立的 portal_super_admin 内部运营角色；
	// 外部客户身份和 portal_customer 数据范围仍必须由 CRM 邀请流程逐个建立。
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
		// Provision 只有在子系统发布角色目录后才返回。管理员角色缺失或停用应视为
		// 部署未完成，而不是在不授予运维访问权的情况下悄悄把环境标记为 READY。
		// 重试保持安全且幂等。
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
	case "settlement":
		// Settlement 拥有刻意带命名空间的角色目录。回退到遗留的通用 "admin" 角色
		// 将授予不了任何权限，也绝不能让部署成功的运行时显得已就绪。
		return []string{"settlement_admin"}
	case "data_analysis":
		// Data analysis 拥有业务特定的管理员角色，其生产开通清单声明的是同一角色。
		// 在清单驱动的初始访问仍为可选项期间，保留此回退。
		return []string{"dashboard_admin"}
	case "customer_and_opportunity":
		// CRM 不创建绕过业务范围的“万能管理员”。三个目录角色共同覆盖运营职责，同时仍受
		// max_effective_roles=10 和各角色数据范围约束。
		return []string{"sales_director", "team_lead", "technical_director"}
	case "customer_portal":
		// 门户客户角色与内部运营角色严格分离。初始接入人只能获得内部角色，不能被
		// 投影成任意客户身份；Portal 服务端还会强制该角色使用全局数据范围。
		return []string{"portal_super_admin"}
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
