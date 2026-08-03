package bootstrap

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"

	applicationregistryapplication "github.com/J-S-Te/Basic-Platform/backend/internal/platform/applicationregistry/application"
	applicationregistryinfrastructure "github.com/J-S-Te/Basic-Platform/backend/internal/platform/applicationregistry/infrastructure"
	applicationregistryhttp "github.com/J-S-Te/Basic-Platform/backend/internal/platform/applicationregistry/interfaces/http"
	applicationaccess "github.com/J-S-Te/Basic-Platform/backend/internal/platform/authorization/applicationaccess"
	filetaskapplication "github.com/J-S-Te/Basic-Platform/backend/internal/platform/filetask/application"
	filetaskinfrastructure "github.com/J-S-Te/Basic-Platform/backend/internal/platform/filetask/infrastructure"
	filetaskhttp "github.com/J-S-Te/Basic-Platform/backend/internal/platform/filetask/interfaces/http"
	notificationapplication "github.com/J-S-Te/Basic-Platform/backend/internal/platform/notification/application"
	notificationinfrastructure "github.com/J-S-Te/Basic-Platform/backend/internal/platform/notification/infrastructure"
	notificationhttp "github.com/J-S-Te/Basic-Platform/backend/internal/platform/notification/interfaces/http"
	"github.com/J-S-Te/Basic-Platform/backend/internal/shared/config"
	"github.com/J-S-Te/Basic-Platform/backend/internal/shared/ulid"
	httptransport "github.com/J-S-Te/Basic-Platform/backend/internal/transport/http"
	"gorm.io/gorm"
)

// buildOperationalModules 只通过各模块公开的应用契约完成装配，不让路由层接触仓储实现。
// 这里同样禁止 AutoMigrate：进程启动与 schema 发布必须保持两个独立生命周期。
func buildOperationalModules(cfg config.Config, database *gorm.DB, logger *slog.Logger, applicationAccessService *applicationaccess.Service) (httptransport.OperationalModules, error) {
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
	// API 只持有受限 Unix Socket 客户端，Docker Socket 和宿主机文件写权限留在隔离的部署 Agent；
	// 这样后台管理权限不会直接等价为容器宿主机权限。
	subsystemHandler, err := applicationregistryhttp.NewSubsystemOnboardingHandler(
		subsystemService,
		subsystemProvisioner,
		subsystemInitialAccessManager{applicationAccess: applicationAccessService},
		cfg.Auth.OIDCIssuer,
		logger,
		subsystemRepository,
	)
	if err != nil {
		return httptransport.OperationalModules{}, err
	}

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

	return httptransport.OperationalModules{
		LoginTargets:        loginTargetHandler,
		SubsystemOnboarding: subsystemHandler,
		Notifications:       notificationHandler,
		FilesAndJobs:        fileTaskHandler,
		AccessApplier:       subsystemProvisioner,
	}, nil
}

// subsystemInitialAccessManager 在子系统目录已经发布时分配约定的初始管理员角色。
// 真正的角色有效性仍由通用授权服务校验，接入流程不能绕开可委派权限和角色目录边界。
type subsystemInitialAccessManager struct {
	applicationAccess *applicationaccess.Service
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
	roleCodes := initialSubsystemAdministratorRoles(applicationCode)
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

func initialSubsystemAdministratorRoles(applicationCode string) []string {
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
