package infrastructure

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/J-S-Te/Basic-Platform/internal/platform/applicationregistry/application"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// SubsystemOnboardingGORMRepository 编排接入涉及的多表事务以及门户目录查询。
type SubsystemOnboardingGORMRepository struct {
	database *gorm.DB
}

// ListRegisteredApplicationCodes 有意读取应用登记表而非权限过滤后的门户投影。ACTIVE 应用
// 已完成目录收编，无论环境如何都不得再出现在首次发现队列中。
func (repository *SubsystemOnboardingGORMRepository) ListRegisteredApplicationCodes(ctx context.Context, tenantID string) ([]string, error) {
	var codes []string
	err := repository.database.WithContext(ctx).Table("platform_application").
		Distinct("code").
		Where("tenant_id = ? AND status = ?", tenantID, "ACTIVE").
		Order("code ASC").
		Pluck("code", &codes).Error
	return codes, err
}

// ResolveApplicationEnvironment 把租户/应用编码/环境名解析为内部 ID。应用与环境必须同时
// ACTIVE，任何一环未命中统一返回 application.ErrNotFound，不泄露具体缺失层级。
func (repository *SubsystemOnboardingGORMRepository) ResolveApplicationEnvironment(ctx context.Context, tenantID, applicationCode, environment string) (string, string, error) {
	var row struct {
		ApplicationID string `gorm:"column:application_id"`
		EnvironmentID string `gorm:"column:environment_id"`
	}
	err := repository.database.WithContext(ctx).Table("platform_application AS application").
		Select("application.id AS application_id, environment.id AS environment_id").
		Joins("JOIN platform_application_environment AS environment ON environment.application_id = application.id AND environment.tenant_id = application.tenant_id").
		Where("application.tenant_id = ? AND application.code = ? AND application.status = ? AND environment.environment = ? AND environment.status = ?", tenantID, applicationCode, "ACTIVE", environment, "ACTIVE").
		Take(&row).Error
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return "", "", application.ErrNotFound
		}
		return "", "", err
	}
	return row.ApplicationID, row.EnvironmentID, nil
}

// ResolveApplicationEnvironmentGateway 读取指定租户应用环境的公开网关基址和路径前缀。
// 环境不存在或未激活时返回数据库错误；调用方据此禁止跳过 Redirect URI 漂移检测。
func (repository *SubsystemOnboardingGORMRepository) ResolveApplicationEnvironmentGateway(ctx context.Context, tenantID, applicationID, environmentID string) (string, string, error) {
	var row struct {
		BaseURL    *string `gorm:"column:base_url"`
		PathPrefix *string `gorm:"column:path_prefix"`
	}
	err := repository.database.WithContext(ctx).Table("platform_application_environment").
		Select("base_url, path_prefix").
		Where("tenant_id = ? AND application_id = ? AND id = ? AND status = ?", tenantID, applicationID, environmentID, "ACTIVE").
		Take(&row).Error
	if err != nil {
		return "", "", err
	}
	if row.BaseURL == nil || row.PathPrefix == nil {
		return "", "", application.ErrValidation
	}
	return *row.BaseURL, *row.PathPrefix, nil
}

// ResolveEnvironmentIssuerAlias 读取该租户/应用/环境三元组持久化的身份提供方。切换门禁
// 必须据此区分“首次从平台切到 Keycloak”与“对已运行在 Keycloak 上的环境做普通更新”。
func (repository *SubsystemOnboardingGORMRepository) ResolveEnvironmentIssuerAlias(ctx context.Context, tenantID, applicationCode, environment string) (string, error) {
	var row struct {
		IssuerAlias *string `gorm:"column:issuer_alias"`
	}
	err := repository.database.WithContext(ctx).Table("platform_application_environment AS environment").
		Select("environment.issuer_alias").
		Joins("JOIN platform_application AS application ON application.id = environment.application_id AND application.tenant_id = environment.tenant_id").
		Where("environment.tenant_id = ? AND application.code = ? AND environment.environment = ?", tenantID, applicationCode, environment).
		Take(&row).Error
	if err != nil {
		return "", err
	}
	if row.IssuerAlias == nil || strings.TrimSpace(*row.IssuerAlias) == "" {
		return "platform", nil
	}
	return strings.ToLower(strings.TrimSpace(*row.IssuerAlias)), nil
}

type subsystemDeploymentStateModel struct {
	TenantID                string     `gorm:"column:tenant_id"`
	ApplicationID           string     `gorm:"column:application_id"`
	EnvironmentID           string     `gorm:"column:environment_id"`
	ApplicationCode         string     `gorm:"column:application_code"`
	Environment             string     `gorm:"column:environment_code"`
	InitialAdminUserID      *string    `gorm:"column:initial_admin_user_id"`
	InitialAccessAssignedAt *time.Time `gorm:"column:initial_access_assigned_at"`
	Status                  string     `gorm:"column:status"`
	Operation               string     `gorm:"column:operation"`
	Generation              uint64     `gorm:"column:generation"`
	AttemptCount            uint       `gorm:"column:attempt_count"`
	DesiredManifestChecksum *string    `gorm:"column:desired_manifest_checksum"`
	AppliedManifestChecksum *string    `gorm:"column:applied_manifest_checksum"`
	ManifestDriftStatus     string     `gorm:"column:manifest_drift_status;default:UNKNOWN"`
	ManifestLastAppliedAt   *time.Time `gorm:"column:manifest_last_applied_at"`
	ManifestLastVerifiedAt  *time.Time `gorm:"column:manifest_last_verified_at"`
	LastErrorCode           *string    `gorm:"column:last_error_code"`
	LastError               *string    `gorm:"column:last_error_message"`
	StartedAt               *time.Time `gorm:"column:started_at"`
	CompletedAt             *time.Time `gorm:"column:completed_at"`
	CreatedAt               time.Time  `gorm:"column:created_at"`
	UpdatedAt               time.Time  `gorm:"column:updated_at"`
}

type subsystemServiceInstanceModel struct {
	TenantID        string     `gorm:"column:tenant_id"`
	ApplicationID   string     `gorm:"column:application_id"`
	EnvironmentID   string     `gorm:"column:environment_id"`
	ApplicationCode string     `gorm:"column:application_code"`
	Environment     string     `gorm:"column:environment_code"`
	ServiceName     string     `gorm:"column:service_name"`
	ServiceRole     string     `gorm:"column:service_role"`
	Protocol        string     `gorm:"column:protocol"`
	InternalHost    string     `gorm:"column:internal_host"`
	InternalPort    uint       `gorm:"column:internal_port"`
	PathPrefix      string     `gorm:"column:path_prefix"`
	HealthEndpoint  string     `gorm:"column:health_endpoint"`
	Version         string     `gorm:"column:version"`
	Status          string     `gorm:"column:status"`
	LastSeenAt      *time.Time `gorm:"column:last_seen_at"`
	CreatedAt       time.Time  `gorm:"column:created_at"`
	UpdatedAt       time.Time  `gorm:"column:updated_at"`
}

// TableName 指定 GORM 行映射的服务实例表名。
func (subsystemServiceInstanceModel) TableName() string { return "subsystem_service_instance" }

// UpsertSubsystemServiceInstance 以租户/环境/服务名/角色为键写入或刷新服务实例快照，
// 在事务中先查后写，避免并发上报产生重复行。
func (repository *SubsystemOnboardingGORMRepository) UpsertSubsystemServiceInstance(ctx context.Context, value application.SubsystemServiceInstance) error {
	now := value.UpdatedAt
	if now.IsZero() {
		now = time.Now().UTC()
	}
	model := subsystemServiceInstanceModel{
		TenantID: value.TenantID, ApplicationID: value.ApplicationID, EnvironmentID: value.EnvironmentID,
		ApplicationCode: value.ApplicationCode, Environment: value.Environment, ServiceName: value.ServiceName,
		ServiceRole: value.ServiceRole, Protocol: value.Protocol, InternalHost: value.InternalHost,
		InternalPort: value.InternalPort, PathPrefix: value.PathPrefix, HealthEndpoint: value.HealthEndpoint,
		Version: value.Version, Status: value.Status, LastSeenAt: value.LastSeenAt, CreatedAt: now, UpdatedAt: now,
	}
	return repository.database.WithContext(ctx).Transaction(func(transaction *gorm.DB) error {
		var existing subsystemServiceInstanceModel
		result := transaction.Where("tenant_id = ? AND environment_id = ? AND service_name = ? AND service_role = ?", value.TenantID, value.EnvironmentID, value.ServiceName, value.ServiceRole).First(&existing)
		if errors.Is(result.Error, gorm.ErrRecordNotFound) {
			return transaction.Create(&model).Error
		}
		if result.Error != nil {
			return result.Error
		}
		return transaction.Model(&subsystemServiceInstanceModel{}).
			Where("tenant_id = ? AND environment_id = ? AND service_name = ? AND service_role = ?", value.TenantID, value.EnvironmentID, value.ServiceName, value.ServiceRole).
			Updates(map[string]any{"application_id": model.ApplicationID, "application_code": model.ApplicationCode, "environment_code": model.Environment, "protocol": model.Protocol, "internal_host": model.InternalHost, "internal_port": model.InternalPort, "path_prefix": model.PathPrefix, "health_endpoint": model.HealthEndpoint, "version": model.Version, "status": model.Status, "last_seen_at": model.LastSeenAt, "updated_at": now}).Error
	})
}

// ListSubsystemServiceInstances 按租户边界列出服务实例快照，应用/环境过滤为可选条件，
// 结果按应用、环境、服务名、角色稳定排序。
func (repository *SubsystemOnboardingGORMRepository) ListSubsystemServiceInstances(ctx context.Context, tenantID, applicationCode, environment string) ([]application.SubsystemServiceInstance, error) {
	query := repository.database.WithContext(ctx).Where("tenant_id = ?", tenantID)
	if applicationCode != "" {
		query = query.Where("application_code = ?", applicationCode)
	}
	if environment != "" {
		query = query.Where("environment_code = ?", environment)
	}
	var rows []subsystemServiceInstanceModel
	if err := query.Order("application_code ASC, environment_code ASC, service_name ASC, service_role ASC").Find(&rows).Error; err != nil {
		return nil, err
	}
	result := make([]application.SubsystemServiceInstance, 0, len(rows))
	for _, row := range rows {
		result = append(result, subsystemServiceInstanceFromModel(row))
	}
	return result, nil
}

// MarkUnavailableSubsystemServiceInstances 把心跳早于截止时间的实例批量标记为不可用；
// last_seen_at 为空的行同样视为失联，防止永不心跳的记录长期占据可用状态。
func (repository *SubsystemOnboardingGORMRepository) MarkUnavailableSubsystemServiceInstances(ctx context.Context, before, now time.Time) error {
	return repository.database.WithContext(ctx).Model(&subsystemServiceInstanceModel{}).
		Where("last_seen_at IS NULL OR last_seen_at < ?", before).
		Updates(map[string]any{"status": application.SubsystemServiceStatusUnavailable, "updated_at": now}).Error
}

func subsystemServiceInstanceFromModel(row subsystemServiceInstanceModel) application.SubsystemServiceInstance {
	return application.SubsystemServiceInstance{TenantID: row.TenantID, ApplicationID: row.ApplicationID, EnvironmentID: row.EnvironmentID, ApplicationCode: row.ApplicationCode, Environment: row.Environment, ServiceName: row.ServiceName, ServiceRole: row.ServiceRole, Protocol: row.Protocol, InternalHost: row.InternalHost, InternalPort: row.InternalPort, PathPrefix: row.PathPrefix, HealthEndpoint: row.HealthEndpoint, Version: row.Version, Status: row.Status, LastSeenAt: row.LastSeenAt, CreatedAt: row.CreatedAt, UpdatedAt: row.UpdatedAt}
}

// TableName 指定 GORM 行映射的部署状态表名。
func (subsystemDeploymentStateModel) TableName() string { return "subsystem_deployment_state" }

// NewSubsystemOnboardingGORMRepository 构造接入仓储；数据库句柄为 nil 时返回错误。
func NewSubsystemOnboardingGORMRepository(database *gorm.DB) (*SubsystemOnboardingGORMRepository, error) {
	if database == nil {
		return nil, errors.New("subsystem onboarding database must not be nil")
	}
	return &SubsystemOnboardingGORMRepository{database: database}, nil
}

// CreateSubsystem 用途：原子创建接入目录、客户端、初始状态及可选向导代次绑定。
// 参数：ctx 为上下文，write 为已校验的写模型，now 为创建时间。
// 返回值：返回创建资源；任一写入或租约检查失败则全部回滚。
func (repository *SubsystemOnboardingGORMRepository) CreateSubsystem(ctx context.Context, write application.SubsystemOnboardingWrite, now time.Time) (application.SubsystemOnboardingResult, error) {
	var result application.SubsystemOnboardingResult
	// 应用、环境、登录目标、浏览器客户端、目录发布客户端和初始部署状态必须同成同败。
	// 只要任一子资源写入失败，事务回滚，避免门户出现无法完成 OAuth 登录的半接入卡片。
	err := repository.database.WithContext(ctx).Transaction(func(transaction *gorm.DB) error {
		if write.AccessOperation != nil {
			if _, err := lockAccessOperation(ctx, transaction, *write.AccessOperation, time.Now().UTC()); err != nil {
				return err
			}
			if write.AccessOperation.Action != application.SubsystemAccessActionOnboard ||
				write.AccessOperation.TenantID != write.Application.TenantID ||
				write.AccessOperation.ApplicationCode != write.Application.Code ||
				write.AccessOperation.EnvironmentCode != write.Environment.Environment {
				return application.ErrSubsystemAccessOperationConflict
			}
		}
		management := &ManagementRepository{database: transaction}
		createdApplication, err := management.CreateApplication(ctx, write.Application, write.ApplicationID, now)
		if err != nil {
			if !errors.Is(err, application.ErrConflict) {
				return err
			}
			createdApplication, err = repository.findApplicationByCode(ctx, transaction, write.Application.TenantID, write.Application.Code)
			if err != nil {
				return err
			}
			if createdApplication.Status != "DRAFT" && createdApplication.Status != "ACTIVE" {
				return application.ErrConflict
			}
		}

		// 应用可能被复用，因此所有子资源写入必须绑定持久化后的权威应用 ID，而不是生成的
		// 候选 ID。
		write.Environment.ApplicationID = createdApplication.ID
		write.LoginTarget.ApplicationID = createdApplication.ID
		write.OAuthClient.ApplicationID = createdApplication.ID
		write.CatalogPublisherOAuthClient.ApplicationID = createdApplication.ID
		for index := range write.ServiceClients {
			write.ServiceClients[index].OAuthClient.ApplicationID = createdApplication.ID
		}

		// 接入有意设计为只创建：已存在的环境可能已拥有无法安全找回的 LoginTarget 与 OAuth
		// 客户端 secret。必须在任何子资源写入前检出冲突，返回可操作的冲突错误而不是通用的
		// 重复键响应。
		existingEnvironment, findErr := repository.findEnvironmentByCode(ctx, transaction, write.Environment.TenantID, createdApplication.ID, write.Environment.Environment)
		if findErr == nil {
			return &application.SubsystemOnboardingConflict{
				ApplicationCode: createdApplication.Code,
				Environment:     existingEnvironment.Environment,
				Status:          existingEnvironment.Status,
			}
		}
		if !errors.Is(findErr, application.ErrNotFound) {
			return findErr
		}

		createdEnvironment, err := management.CreateEnvironment(ctx, write.Environment, write.EnvironmentID, now)
		if err != nil {
			return err
		}

		loginTargets := &LoginTargetGORMRepository{database: transaction}
		createdLoginTarget, err := loginTargets.CreateLoginTarget(ctx, write.LoginTarget, write.LoginTargetID, now)
		if err != nil {
			return err
		}

		oauthClients := &OAuthClientManagementRepository{database: transaction}
		createdOAuthClient, err := oauthClients.CreateOAuthClient(ctx, write.OAuthClient, write.OAuthClientID, write.OAuthClientSecret, now)
		if err != nil {
			return err
		}
		createdCatalogPublisherOAuthClient, err := oauthClients.CreateOAuthClient(ctx, write.CatalogPublisherOAuthClient, write.CatalogPublisherOAuthClientID, write.CatalogPublisherOAuthClientSecret, now)
		if err != nil {
			return err
		}
		createdServiceCredentials := make([]application.SubsystemServiceCredential, 0, len(write.ServiceClients))
		for _, serviceClient := range write.ServiceClients {
			created, createErr := oauthClients.CreateOAuthClient(ctx, serviceClient.OAuthClient, serviceClient.OAuthClientID, serviceClient.OAuthClientSecret, now)
			if createErr != nil {
				return createErr
			}
			createdServiceCredentials = append(createdServiceCredentials, application.SubsystemServiceCredential{
				Purpose: serviceClient.Purpose, OAuthClient: created,
			})
		}
		if err := transaction.Create(&subsystemDeploymentStateModel{
			TenantID:            createdApplication.TenantID,
			ApplicationID:       createdApplication.ID,
			EnvironmentID:       createdEnvironment.ID,
			ApplicationCode:     createdApplication.Code,
			Environment:         createdEnvironment.Environment,
			InitialAdminUserID:  optionalStringPointer(write.InitialAdminUserID),
			Status:              application.SubsystemDeploymentStatusProvisioning,
			Operation:           "ONBOARD",
			Generation:          1,
			AttemptCount:        1,
			ManifestDriftStatus: "UNKNOWN",
			StartedAt:           timePointer(now.UTC()),
			CreatedAt:           now.UTC(),
			UpdatedAt:           now.UTC(),
		}).Error; err != nil {
			return err
		}

		if write.AccessOperation != nil {
			op := *write.AccessOperation
			if err := bindInitialAccessDeployment(ctx, transaction, op, createdApplication.ID, createdEnvironment.ID, now); err != nil {
				return err
			}
		}
		result = application.SubsystemOnboardingResult{
			Application: createdApplication, Environment: createdEnvironment,
			LoginTarget: createdLoginTarget, OAuthClient: createdOAuthClient,
			CatalogPublisherOAuthClient: createdCatalogPublisherOAuthClient,
			ServiceCredentials:          createdServiceCredentials,
		}
		return nil
	})
	return result, err
}

// CreateSubsystemDirectory 只持久化子系统登记中的目录部分。它有意不创建 OAuth 客户端、
// 服务凭据或部署状态：三者分别属于认证提供方集成与运行时部署工作流。
func (repository *SubsystemOnboardingGORMRepository) CreateSubsystemDirectory(ctx context.Context, write application.SubsystemDirectoryRegistrationWrite, now time.Time) (application.SubsystemDirectoryRegistrationResult, error) {
	var result application.SubsystemDirectoryRegistrationResult
	err := repository.database.WithContext(ctx).Transaction(func(transaction *gorm.DB) error {
		management := &ManagementRepository{database: transaction}
		createdApplication, err := management.CreateApplication(ctx, write.Application, write.ApplicationID, now)
		if err != nil {
			if !errors.Is(err, application.ErrConflict) {
				return err
			}
			createdApplication, err = repository.findApplicationByCode(ctx, transaction, write.Application.TenantID, write.Application.Code)
			if err != nil {
				return err
			}
			if createdApplication.Status != "DRAFT" && createdApplication.Status != "ACTIVE" {
				return application.ErrConflict
			}
		}

		write.Environment.ApplicationID = createdApplication.ID
		write.LoginTarget.ApplicationID = createdApplication.ID
		existingEnvironment, findErr := repository.findEnvironmentByCode(ctx, transaction, write.Environment.TenantID, createdApplication.ID, write.Environment.Environment)
		if findErr == nil {
			return &application.SubsystemOnboardingConflict{
				ApplicationCode: createdApplication.Code,
				Environment:     existingEnvironment.Environment,
				Status:          existingEnvironment.Status,
			}
		}
		if !errors.Is(findErr, application.ErrNotFound) {
			return findErr
		}

		createdEnvironment, err := management.CreateEnvironment(ctx, write.Environment, write.EnvironmentID, now)
		if err != nil {
			return err
		}
		loginTargets := &LoginTargetGORMRepository{database: transaction}
		createdLoginTarget, err := loginTargets.CreateLoginTarget(ctx, write.LoginTarget, write.LoginTargetID, now)
		if err != nil {
			return err
		}
		result = application.SubsystemDirectoryRegistrationResult{
			Application: createdApplication, Environment: createdEnvironment, LoginTarget: createdLoginTarget,
		}
		return nil
	})
	return result, err
}

// findApplicationByCode 只在创建冲突时检索导致重复的确切租户内应用。它有意保持私有、
// 仅限接入事务使用，请求不能借它跨租户边界或覆盖既有应用。
func (repository *SubsystemOnboardingGORMRepository) findApplicationByCode(ctx context.Context, database *gorm.DB, tenantID, code string) (application.Application, error) {
	var model managementApplicationModel
	if err := database.WithContext(ctx).Where("tenant_id = ? AND code = ?", tenantID, code).Take(&model).Error; err != nil {
		return application.Application{}, mapManagementError(err)
	}
	return toApplication(model), nil
}

// findEnvironmentByCode 为只创建冲突检查查询既有租户内环境；调用方必须已在同一事务内
// 解析出应用。
func (repository *SubsystemOnboardingGORMRepository) findEnvironmentByCode(ctx context.Context, database *gorm.DB, tenantID, applicationID, environment string) (application.Environment, error) {
	var model managementEnvironmentModel
	if err := database.WithContext(ctx).
		Where("tenant_id = ? AND application_id = ? AND environment = ?", tenantID, applicationID, environment).
		Take(&model).Error; err != nil {
		return application.Environment{}, mapManagementError(err)
	}
	return toEnvironment(model), nil
}

type portalApplicationRow struct {
	ApplicationID    string  `gorm:"column:application_id"`
	Code             string  `gorm:"column:code"`
	Name             string  `gorm:"column:name"`
	Description      *string `gorm:"column:description"`
	EnvironmentID    string  `gorm:"column:environment_id"`
	Environment      string  `gorm:"column:environment"`
	BaseURL          string  `gorm:"column:base_url"`
	PathPrefix       *string `gorm:"column:path_prefix"`
	TargetCode       string  `gorm:"column:target_code"`
	TargetURI        string  `gorm:"column:target_uri"`
	IssuerAlias      string  `gorm:"column:issuer_alias"`
	MappingStatus    string  `gorm:"column:mapping_status"`
	ProjectionStatus string  `gorm:"column:projection_status"`
	PendingCount     int64   `gorm:"column:pending_count"`
	RunningCount     int64   `gorm:"column:running_count"`
	FailedCount      int64   `gorm:"column:failed_count"`
}

const (
	portalProjectionNotRequired   = "NOT_REQUIRED"
	portalProjectionNotConfigured = "NOT_CONFIGURED"
	portalProjectionMissing       = "MISSING"
	portalProjectionPending       = "PENDING"
	portalProjectionRunning       = "RUNNING"
	portalProjectionFailed        = "FAILED"
	portalProjectionSynced        = "SYNCED"
)

func portalProjectionReadiness(row portalApplicationRow) application.PortalProjectionReadiness {
	if !strings.EqualFold(strings.TrimSpace(row.IssuerAlias), "keycloak") {
		return application.PortalProjectionReadiness{Status: portalProjectionNotRequired, Ready: true}
	}
	if !strings.EqualFold(strings.TrimSpace(row.MappingStatus), "SYNCED") {
		return application.PortalProjectionReadiness{Status: portalProjectionNotConfigured, NextAction: "该环境尚未完成 Keycloak Client 同步，请联系管理员完成接入。"}
	}
	if row.FailedCount > 0 {
		return application.PortalProjectionReadiness{Status: portalProjectionFailed, NextAction: "账号权限同步失败，请联系管理员重试 Keycloak 投影。"}
	}
	if row.RunningCount > 0 || strings.EqualFold(strings.TrimSpace(row.ProjectionStatus), "SYNCING") {
		return application.PortalProjectionReadiness{Status: portalProjectionRunning, NextAction: "账号权限正在同步，请稍后重试。"}
	}
	if row.PendingCount > 0 || strings.EqualFold(strings.TrimSpace(row.ProjectionStatus), portalProjectionPending) {
		return application.PortalProjectionReadiness{Status: portalProjectionPending, NextAction: "账号权限正在等待同步，请稍后重试。"}
	}
	if strings.EqualFold(strings.TrimSpace(row.ProjectionStatus), portalProjectionFailed) {
		return application.PortalProjectionReadiness{Status: portalProjectionFailed, NextAction: "账号权限同步失败，请联系管理员重试 Keycloak 投影。"}
	}
	if strings.EqualFold(strings.TrimSpace(row.ProjectionStatus), portalProjectionSynced) {
		return application.PortalProjectionReadiness{Status: portalProjectionSynced, Ready: true}
	}
	return application.PortalProjectionReadiness{Status: portalProjectionMissing, NextAction: "账号尚未投影到 Keycloak，请联系管理员同步账号权限。"}
}

// ListPortalApplications 为每个活跃应用返回一个首选的活跃环境/目标。未显式指定环境时的
// 偏好顺序为 prod、staging、test、dev，其余按字典序。
func (repository *SubsystemOnboardingGORMRepository) ListPortalApplications(ctx context.Context, tenantID, userID, environment string) ([]application.PortalApplication, error) {
	accessFilter, accessArgs := portalApplicationAccessFilter(userID)
	query := repository.database.WithContext(ctx).
		Table("platform_application AS application").
		Select(`application.id AS application_id, application.code, application.name, application.description,
			environment.id AS environment_id, environment.environment, environment.base_url, environment.path_prefix,
			target.target_code, target.target_uri, COALESCE(environment.issuer_alias, 'platform') AS issuer_alias,
			COALESCE(client_mapping.status, '') AS mapping_status, COALESCE(user_projection.status, '') AS projection_status,
			COALESCE(projection_queue.pending_count, 0) AS pending_count,
			COALESCE(projection_queue.running_count, 0) AS running_count,
			COALESCE(projection_queue.failed_count, 0) AS failed_count`).
		Joins("JOIN platform_application_environment AS environment ON environment.application_id = application.id AND environment.tenant_id = application.tenant_id").
		Joins("JOIN platform_application_login_target AS target ON target.environment_id = environment.id AND target.application_id = application.id AND target.tenant_id = application.tenant_id").
		Joins("JOIN subsystem_deployment_state AS deployment ON deployment.tenant_id = application.tenant_id AND deployment.application_id = application.id AND deployment.environment_id = environment.id AND deployment.status = ?", application.SubsystemDeploymentStatusReady).
		Joins("LEFT JOIN keycloak_application_client_mapping AS client_mapping ON client_mapping.tenant_id = application.tenant_id AND client_mapping.application_id = application.id AND client_mapping.environment_id = environment.id").
		Joins("LEFT JOIN keycloak_authorization_projection AS user_projection ON user_projection.tenant_id = application.tenant_id AND user_projection.identity_id = ? AND user_projection.application_id = application.id AND user_projection.environment_id = environment.id", userID).
		Joins(`LEFT JOIN (
			SELECT tenant_id, identity_id, application_id, environment_id,
				SUM(CASE WHEN status = 'PENDING' THEN 1 ELSE 0 END) AS pending_count,
				SUM(CASE WHEN status = 'RUNNING' THEN 1 ELSE 0 END) AS running_count,
				SUM(CASE WHEN status = 'FAILED' THEN 1 ELSE 0 END) AS failed_count
			FROM keycloak_authorization_outbox
			WHERE identity_id = ? AND environment_id IS NOT NULL AND status IN ('PENDING', 'RUNNING', 'FAILED')
			GROUP BY tenant_id, identity_id, application_id, environment_id
		) AS projection_queue ON projection_queue.tenant_id = application.tenant_id AND projection_queue.identity_id = ? AND projection_queue.application_id = application.id AND projection_queue.environment_id = environment.id`, userID, userID).
		Where("application.tenant_id = ? AND application.status = ? AND environment.status = ? AND target.status = ?", tenantID, "ACTIVE", "ACTIVE", "ACTIVE").
		Where(accessFilter, accessArgs...)
	if environment != "" {
		query = query.Where("environment.environment = ?", environment)
	}
	query = query.Order(`application.code ASC,
		CASE environment.environment WHEN 'prod' THEN 0 WHEN 'staging' THEN 1 WHEN 'test' THEN 2 WHEN 'dev' THEN 3 ELSE 4 END ASC,
		environment.environment ASC,
		CASE target.target_code WHEN 'home' THEN 0 ELSE 1 END ASC,
		target.target_code ASC`)

	var rows []portalApplicationRow
	if err := query.Scan(&rows).Error; err != nil {
		return nil, err
	}
	seen := make(map[string]struct{}, len(rows))
	items := make([]application.PortalApplication, 0, len(rows))
	for _, row := range rows {
		// SQL 排序已把环境和 home 目标的优先级固定；这里按应用取首条，确保同一应用
		// 不因登记多个环境/目标而在门户重复展示。
		if _, exists := seen[row.ApplicationID]; exists {
			continue
		}
		seen[row.ApplicationID] = struct{}{}
		projection := portalProjectionReadiness(row)
		items = append(items, application.PortalApplication{
			ApplicationID: row.ApplicationID, Code: row.Code, Name: row.Name, Description: row.Description,
			EnvironmentID: row.EnvironmentID, Environment: row.Environment, PathPrefix: row.PathPrefix,
			TargetCode: row.TargetCode, TargetURI: row.TargetURI, PublicURL: row.BaseURL,
			Allowed: projection.Ready, Projection: projection,
		})
	}
	return items, nil
}

// TransitionSubsystemDeployment 用登记的应用/环境编码记录一次生命周期转移。错误详情对
// 运维安全，入库前被有意截断；凭据与命令输出绝不能进入该表。
func (repository *SubsystemOnboardingGORMRepository) TransitionSubsystemDeployment(ctx context.Context, tenantID, applicationCode, environment, status, operation, errorCode, errorMessage string, now time.Time) error {
	status = strings.TrimSpace(status)
	operation = strings.TrimSpace(operation)
	errorCode = strings.TrimSpace(errorCode)
	errorMessage = strings.TrimSpace(errorMessage)
	if len(errorCode) > 128 {
		errorCode = errorCode[:128]
	}
	if len(errorMessage) > 1000 {
		errorMessage = errorMessage[:1000]
	}
	updates := map[string]any{
		"status":             status,
		"operation":          operation,
		"last_error_code":    nullableString(errorCode),
		"last_error_message": nullableString(errorMessage),
		"updated_at":         now.UTC(),
	}
	if status == application.SubsystemDeploymentStatusProvisioning || status == application.SubsystemDeploymentStatusUpdating || status == application.SubsystemDeploymentStatusVerifying || status == application.SubsystemDeploymentStatusDraining {
		// 非终态开始新的尝试并清空完成时间；终态只收口当前尝试。状态接口因此可以区分
		// “上次失败”与“本次仍在执行”，而无需暴露部署命令输出。
		// generation 标识一次生命周期尝试；终态转移保持同一 generation，状态轮询因此能
		// 区分“重试”与“同一尝试的收口更新”。
		if status != application.SubsystemDeploymentStatusProvisioning {
			updates["generation"] = gorm.Expr("generation + 1")
		}
		updates["attempt_count"] = gorm.Expr("attempt_count + 1")
		updates["started_at"] = now.UTC()
		updates["completed_at"] = nil
	}
	if status == application.SubsystemDeploymentStatusReady || status == application.SubsystemDeploymentStatusFailed || status == application.SubsystemDeploymentStatusOffboarded {
		updates["completed_at"] = now.UTC()
	}
	// 状态机守卫：只有在位状态是目标状态的合法前驱（或与目标相同，幂等收口）时才允许
	// 覆写，并且要求 generation 在读取和写入之间未变化（CAS）。WHERE 同时约束 status 与
	// generation，并发转移只有第一个能命中，避免“最后一次赢”的状态覆盖。
	scope := repository.database.WithContext(ctx).Model(&subsystemDeploymentStateModel{}).
		Where("tenant_id = ? AND application_code = ? AND environment_code = ?", strings.TrimSpace(tenantID), strings.TrimSpace(applicationCode), strings.ToLower(strings.TrimSpace(environment)))
	var current subsystemDeploymentStateModel
	if err := scope.Session(&gorm.Session{}).Take(&current).Error; err != nil {
		if !errors.Is(err, gorm.ErrRecordNotFound) {
			return err
		}
		// 记录缺失：ADOPT 仍允许建档，其余操作维持原有的 NotFound 语义。
		if !(operation == "ADOPT" && status == application.SubsystemDeploymentStatusUpdating) {
			return application.ErrNotFound
		}
		var adoptionScope struct {
			ApplicationID string `gorm:"column:application_id"`
			EnvironmentID string `gorm:"column:environment_id"`
		}
		if err := repository.database.WithContext(ctx).Table("platform_application AS application").
			Select("application.id AS application_id, environment.id AS environment_id").
			Joins("JOIN platform_application_environment AS environment ON environment.application_id = application.id AND environment.tenant_id = application.tenant_id").
			Where("application.tenant_id = ? AND application.code = ? AND application.status = ? AND environment.environment = ? AND environment.status = ?", strings.TrimSpace(tenantID), strings.TrimSpace(applicationCode), "ACTIVE", strings.ToLower(strings.TrimSpace(environment)), "ACTIVE").
			Take(&adoptionScope).Error; err != nil {
			return mapManagementError(err)
		}
		return repository.database.WithContext(ctx).Create(&subsystemDeploymentStateModel{
			TenantID: strings.TrimSpace(tenantID), ApplicationID: adoptionScope.ApplicationID, EnvironmentID: adoptionScope.EnvironmentID,
			ApplicationCode: strings.TrimSpace(applicationCode), Environment: strings.ToLower(strings.TrimSpace(environment)),
			Status: status, Operation: operation, Generation: 1, AttemptCount: 1, ManifestDriftStatus: "UNKNOWN", StartedAt: &now, CreatedAt: now, UpdatedAt: now,
		}).Error
	}
	if current.Status != status && !application.SubsystemDeploymentTransitionAllowed(current.Status, status) {
		return fmt.Errorf("%w: %s -> %s", application.ErrSubsystemDeploymentTransition, current.Status, status)
	}
	result := scope.Session(&gorm.Session{}).
		Where("status = ? AND generation = ?", current.Status, current.Generation).
		Updates(updates)
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected != 1 {
		if current.Status == status {
			// 同状态幂等：目标转移已经在并发竞争中完成，无需重写。
			return nil
		}
		return fmt.Errorf("%w: %s -> %s (concurrent update)", application.ErrSubsystemDeploymentTransition, current.Status, status)
	}
	return nil
}

// ClaimSubsystemDeployment 在签发凭据前取行锁。同状态转移不算抢占：否则两个都观察到
// READY 的请求可能同时部署。
func (repository *SubsystemOnboardingGORMRepository) ClaimSubsystemDeployment(ctx context.Context, tenantID, code, environment, operation string, now time.Time) (generation uint64, err error) {
	err = repository.database.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var row subsystemDeploymentStateModel
		err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where("tenant_id = ? AND application_code = ? AND environment_code = ?", tenantID, code, environment).Take(&row).Error
		if err != nil && !errors.Is(err, gorm.ErrRecordNotFound) {
			return err
		}
		if err == nil && (row.Status == application.SubsystemDeploymentStatusProvisioning || row.Status == application.SubsystemDeploymentStatusUpdating || row.Status == application.SubsystemDeploymentStatusVerifying || row.Status == application.SubsystemDeploymentStatusDraining || row.Status == application.SubsystemDeploymentStatusDiscarding) {
			return application.ErrSubsystemDeploymentTransition
		}
		repo := &SubsystemOnboardingGORMRepository{database: tx}
		if err := repo.TransitionSubsystemDeployment(ctx, tenantID, code, environment, application.SubsystemDeploymentStatusUpdating, operation, "", "", now); err != nil {
			return err
		}
		state, err := repo.GetSubsystemDeploymentState(ctx, tenantID, code, environment)
		generation = state.Generation
		return err
	})
	return
}

// CompleteSubsystemDeployment 的终态写入由抢占成功的尝试持有；过期的后台任务晚归时，
// 不得以“当前记录状态”覆盖他人的结果。
func (repository *SubsystemOnboardingGORMRepository) CompleteSubsystemDeployment(ctx context.Context, tenantID, code, environment string, generation uint64, status, operation, errorCode, message string, now time.Time) error {
	return repository.database.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var row subsystemDeploymentStateModel
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where("tenant_id = ? AND application_code = ? AND environment_code = ?", tenantID, code, environment).Take(&row).Error; err != nil {
			return mapManagementError(err)
		}
		if row.Generation != generation {
			return application.ErrSubsystemDeploymentTransition
		}
		return (&SubsystemOnboardingGORMRepository{database: tx}).TransitionSubsystemDeployment(ctx, tenantID, code, environment, status, operation, errorCode, message, now)
	})
}

// RecoverStaleSubsystemDeployment 用途：仅清理无人持有的过期代次，保留持久向导的恢复权。
// 参数：ctx 为上下文，state 为枚举快照，cutoff 为过期界限，now 为当前时间。
// 返回值：返回是否完成回收；代次变化或向导仍持有槽位时返回 false。
func (repository *SubsystemOnboardingGORMRepository) RecoverStaleSubsystemDeployment(ctx context.Context, state application.SubsystemDeploymentState, cutoff, now time.Time) (bool, error) {
	if state.StartedAt == nil || !state.StartedAt.Before(cutoff) {
		return false, nil
	}
	if state.Status != application.SubsystemDeploymentStatusProvisioning && state.Status != application.SubsystemDeploymentStatusUpdating && state.Status != application.SubsystemDeploymentStatusVerifying && state.Status != application.SubsystemDeploymentStatusDraining && state.Status != application.SubsystemDeploymentStatusDiscarding {
		return false, nil
	}
	// 丢弃进程在占用之后崩溃时，记录会停在 DISCARDING 且没有任何编排会继续推进它。
	// 看护把它收口回失败，运维即可重新发起丢弃或重试部署；错误码区分两者，便于排查。
	errorCode, errorMessage := "DEPLOYMENT_INTERRUPTED", "部署请求中断，请点击重试"
	if state.Status == application.SubsystemDeploymentStatusDiscarding {
		errorCode, errorMessage = "DISCARD_INTERRUPTED", "失败部署清理中断，可重新发起清理或重试部署"
	}
	result := repository.database.WithContext(ctx).Model(&subsystemDeploymentStateModel{}).
		Where("tenant_id = ? AND application_code = ? AND environment_code = ? AND generation = ? AND status = ? AND started_at = ? AND started_at < ?", state.TenantID, state.ApplicationCode, state.Environment, state.Generation, state.Status, state.StartedAt.UTC(), cutoff.UTC()).
		Where(`NOT EXISTS (
   SELECT 1 FROM subsystem_access_deployment_attempt AS attempt
   JOIN subsystem_access_operation AS access_op
    ON access_op.tenant_id = attempt.tenant_id AND access_op.id = attempt.operation_id
   WHERE attempt.tenant_id = subsystem_deployment_state.tenant_id
    AND attempt.environment_id = subsystem_deployment_state.environment_id
    AND attempt.generation = subsystem_deployment_state.generation
    AND (attempt.phase = 'DISPATCHED' OR (access_op.active_slot IS NOT NULL AND attempt.phase = 'PREPARED'))
  )`).
		Updates(map[string]any{"status": application.SubsystemDeploymentStatusFailed, "last_error_code": errorCode, "last_error_message": errorMessage, "completed_at": now.UTC(), "updated_at": now.UTC()})
	return result.RowsAffected == 1, result.Error
}

// GetSubsystemDeploymentState 只返回租户范围内的生命周期元数据。
func (repository *SubsystemOnboardingGORMRepository) GetSubsystemDeploymentState(ctx context.Context, tenantID, applicationCode, environment string) (application.SubsystemDeploymentState, error) {
	var model subsystemDeploymentStateModel
	if err := repository.database.WithContext(ctx).
		Where("tenant_id = ? AND application_code = ? AND environment_code = ?", strings.TrimSpace(tenantID), strings.TrimSpace(applicationCode), strings.ToLower(strings.TrimSpace(environment))).
		Take(&model).Error; err != nil {
		return application.SubsystemDeploymentState{}, mapManagementError(err)
	}
	return deploymentStateFromModel(model), nil
}

// ClaimFailedSubsystemDeploymentForDiscard 原子占用一条失败部署记录用于丢弃。
//
// 丢弃流程必须先占用再销毁基础设施。旧实现先 Teardown、再按 status=FAILED 删除记录：
// 并发的「重试」把记录推进为 UPDATING 后，删除会被正确拒绝，但 Teardown 已经拆掉了重试
// 刚部署起来的环境。占用要求 status=PROVISION_FAILED 且 generation 与调用方读取时一致
// （CAS），只有抢占成功的调用方才能继续 Teardown；重试在 DISCARDING 期间无法接管。
func (repository *SubsystemOnboardingGORMRepository) ClaimFailedSubsystemDeploymentForDiscard(ctx context.Context, tenantID, applicationCode, environment string, expectedGeneration uint64, now time.Time) (bool, error) {
	// 占用时刷新 started_at 并清空 completed_at：看护任务据此判定"丢弃是否已经中断"，
	// 否则继承上一次失败尝试的旧 started_at 会让刚占用的记录立刻被当成僵死状态。
	result := repository.database.WithContext(ctx).Model(&subsystemDeploymentStateModel{}).
		Where("tenant_id = ? AND application_code = ? AND environment_code = ? AND status = ? AND generation = ?",
			strings.TrimSpace(tenantID), strings.TrimSpace(applicationCode), strings.ToLower(strings.TrimSpace(environment)),
			application.SubsystemDeploymentStatusFailed, expectedGeneration).
		Updates(map[string]any{
			"status":       application.SubsystemDeploymentStatusDiscarding,
			"generation":   gorm.Expr("generation + 1"),
			"started_at":   now.UTC(),
			"completed_at": nil,
			"updated_at":   now.UTC(),
		})
	if result.Error != nil {
		return false, result.Error
	}
	return result.RowsAffected == 1, nil
}

// DiscardClaimedSubsystemDeployment 删除已占用的失败记录。
//
// 只匹配 DISCARDING + 占用时的 generation：占用后不会有其他写入能推进该记录（重试在
// DISCARDING 期间被拒绝），因此删除失败说明记录已被看护任务收口或人工处理，按冲突返回。
func (repository *SubsystemOnboardingGORMRepository) DiscardClaimedSubsystemDeployment(ctx context.Context, tenantID, applicationCode, environment string, claimedGeneration uint64) error {
	result := repository.database.WithContext(ctx).
		Where("tenant_id = ? AND application_code = ? AND environment_code = ? AND status = ? AND generation = ?",
			strings.TrimSpace(tenantID), strings.TrimSpace(applicationCode), strings.ToLower(strings.TrimSpace(environment)),
			application.SubsystemDeploymentStatusDiscarding, claimedGeneration).
		Delete(&subsystemDeploymentStateModel{})
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected != 1 {
		return application.ErrValidation
	}
	return nil
}

// ResolveSubsystemHealthTargets 把每个活跃应用环境投影为容器网络内的健康探测 URL。
// Upstream 优先于 BaseURL：平台 API 容器可直接访问 Compose 网络，而 BaseURL 是面向浏览器的。
// 健康路径来自环境元数据（默认 /healthz）。
func (repository *SubsystemOnboardingGORMRepository) ResolveSubsystemHealthTargets(ctx context.Context, tenantID string) ([]application.SubsystemHealthTarget, error) {
	var rows []struct {
		Code        string  `gorm:"column:code"`
		Environment string  `gorm:"column:environment"`
		UpstreamURL *string `gorm:"column:upstream_url"`
		BaseURL     *string `gorm:"column:base_url"`
		Metadata    *string `gorm:"column:metadata"`
	}
	err := repository.database.WithContext(ctx).
		Table("platform_application_environment AS environment").
		Select("application.code AS code, environment.environment AS environment, environment.upstream_url AS upstream_url, environment.base_url AS base_url, environment.metadata AS metadata").
		Joins("JOIN platform_application AS application ON application.id = environment.application_id AND application.tenant_id = environment.tenant_id").
		Where("environment.tenant_id = ? AND environment.status = ? AND application.status = ?", strings.TrimSpace(tenantID), "ACTIVE", "ACTIVE").
		Scan(&rows).Error
	if err != nil {
		return nil, err
	}
	targets := make([]application.SubsystemHealthTarget, 0, len(rows))
	for _, row := range rows {
		origin := strings.TrimSpace(dereferenceString(row.UpstreamURL))
		if origin == "" {
			origin = strings.TrimSpace(dereferenceString(row.BaseURL))
		}
		if origin == "" {
			continue
		}
		healthPath := "/healthz"
		if row.Metadata != nil && strings.TrimSpace(*row.Metadata) != "" {
			var metadata struct {
				HealthPath string `json:"health_path"`
			}
			if json.Unmarshal([]byte(*row.Metadata), &metadata) == nil && strings.TrimSpace(metadata.HealthPath) != "" {
				healthPath = strings.TrimSpace(metadata.HealthPath)
			}
		}
		targets = append(targets, application.SubsystemHealthTarget{
			ApplicationCode: row.Code,
			Environment:     row.Environment,
			HealthURL:       strings.TrimRight(origin, "/") + healthPath,
		})
	}
	return targets, nil
}

func deploymentStateFromModel(model subsystemDeploymentStateModel) application.SubsystemDeploymentState {
	return application.SubsystemDeploymentState{
		TenantID: model.TenantID, ApplicationID: model.ApplicationID, EnvironmentID: model.EnvironmentID,
		ApplicationCode: model.ApplicationCode, Environment: model.Environment, Status: model.Status,
		InitialAdminUserID: modelString(model.InitialAdminUserID), InitialAccessAssignedAt: model.InitialAccessAssignedAt,
		Operation: model.Operation, Generation: model.Generation, AttemptCount: model.AttemptCount,
		DesiredManifestChecksum: dereferenceString(model.DesiredManifestChecksum), AppliedManifestChecksum: dereferenceString(model.AppliedManifestChecksum),
		ManifestDriftStatus: model.ManifestDriftStatus, ManifestLastAppliedAt: model.ManifestLastAppliedAt, ManifestLastVerifiedAt: model.ManifestLastVerifiedAt,
		LastErrorCode: dereferenceString(model.LastErrorCode), LastError: dereferenceString(model.LastError),
		StartedAt: model.StartedAt, CompletedAt: model.CompletedAt, CreatedAt: model.CreatedAt, UpdatedAt: model.UpdatedAt,
	}
}

// ListInFlightSubsystemDeployments 返回仍处于非终态（PROVISIONING/UPDATING/VERIFYING/
// DRAINING）的部署状态行，供后台看护任务识别编排已死、无人收口的记录。只投影状态机
// 需要的字段来源行本身；不暴露部署命令输出或凭据。
func (repository *SubsystemOnboardingGORMRepository) ListInFlightSubsystemDeployments(ctx context.Context) ([]application.SubsystemDeploymentState, error) {
	var rows []subsystemDeploymentStateModel
	if err := repository.database.WithContext(ctx).
		Where("status IN ?", []string{
			application.SubsystemDeploymentStatusProvisioning,
			application.SubsystemDeploymentStatusUpdating,
			application.SubsystemDeploymentStatusVerifying,
			application.SubsystemDeploymentStatusDraining,
			// 丢弃占用态也属于"有编排正在进行"：进程崩溃后必须由看护收口回失败态，
			// 否则记录会永远占用 code+environment，既不能丢弃也不能重试。
			application.SubsystemDeploymentStatusDiscarding,
		}).
		Order("started_at ASC").
		Find(&rows).Error; err != nil {
		return nil, err
	}
	states := make([]application.SubsystemDeploymentState, 0, len(rows))
	for _, row := range rows {
		states = append(states, application.SubsystemDeploymentState{
			TenantID: row.TenantID, ApplicationID: row.ApplicationID, EnvironmentID: row.EnvironmentID,
			ApplicationCode: row.ApplicationCode, Environment: row.Environment,
			Status: row.Status, Operation: row.Operation, Generation: row.Generation,
			AttemptCount: row.AttemptCount, StartedAt: row.StartedAt, CompletedAt: row.CompletedAt,
			DesiredManifestChecksum: dereferenceString(row.DesiredManifestChecksum),
			AppliedManifestChecksum: dereferenceString(row.AppliedManifestChecksum),
			LastErrorCode:           dereferenceString(row.LastErrorCode), LastError: dereferenceString(row.LastError),
		})
	}
	return states, nil
}

// SetSubsystemDesiredManifest 记录控制面本轮期望清单校验和；与已应用值不同时立即标记
// DRIFT，使部署前的漂移可被观测。记录不存在时返回 ErrNotFound。
func (repository *SubsystemOnboardingGORMRepository) SetSubsystemDesiredManifest(ctx context.Context, tenantID, applicationCode, environment, checksum string, now time.Time) error {
	checksum = strings.TrimSpace(checksum)
	if checksum == "" {
		return application.ErrValidation
	}
	result := repository.database.WithContext(ctx).Model(&subsystemDeploymentStateModel{}).
		Where("tenant_id = ? AND application_code = ? AND environment_code = ?", strings.TrimSpace(tenantID), strings.TrimSpace(applicationCode), strings.ToLower(strings.TrimSpace(environment))).
		Updates(map[string]any{
			"desired_manifest_checksum": checksum,
			"manifest_drift_status":     gorm.Expr("CASE WHEN applied_manifest_checksum = ? THEN 'IN_SYNC' ELSE 'DRIFT' END", checksum),
			"updated_at":                now.UTC(),
		})
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected != 1 {
		return application.ErrNotFound
	}
	return nil
}

// MarkSubsystemManifestApplied 在部署 Agent 成功且握手校验通过后记录实际应用清单。
func (repository *SubsystemOnboardingGORMRepository) MarkSubsystemManifestApplied(ctx context.Context, tenantID, applicationCode, environment, checksum string, now time.Time) error {
	checksum = strings.TrimSpace(checksum)
	if checksum == "" {
		return application.ErrValidation
	}
	result := repository.database.WithContext(ctx).Model(&subsystemDeploymentStateModel{}).
		Where("tenant_id = ? AND application_code = ? AND environment_code = ? AND desired_manifest_checksum = ?", strings.TrimSpace(tenantID), strings.TrimSpace(applicationCode), strings.ToLower(strings.TrimSpace(environment)), checksum).
		Updates(map[string]any{"applied_manifest_checksum": checksum, "manifest_drift_status": "IN_SYNC", "manifest_last_applied_at": now.UTC(), "manifest_last_verified_at": now.UTC(), "updated_at": now.UTC()})
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected != 1 {
		return application.ErrVersionConflict
	}
	return nil
}

// GetSubsystemDeploymentContext 直接从控制面登记解析应用/环境标识，不依赖部署状态。
// 重试不能走门户投影：PROVISION_FAILED 环境被有意隐藏在那里。
func (repository *SubsystemOnboardingGORMRepository) GetSubsystemDeploymentContext(ctx context.Context, tenantID, applicationCode, environment string) (application.SubsystemDeploymentState, error) {
	tenantID = strings.TrimSpace(tenantID)
	applicationCode = strings.TrimSpace(applicationCode)
	environment = strings.ToLower(strings.TrimSpace(environment))
	var model subsystemDeploymentStateModel
	if err := repository.database.WithContext(ctx).
		Table("subsystem_deployment_state AS deployment").
		Select("deployment.*").
		Joins("JOIN platform_application AS application ON application.tenant_id = deployment.tenant_id AND application.id = deployment.application_id AND application.code = ?", applicationCode).
		Joins("JOIN platform_application_environment AS environment ON environment.tenant_id = deployment.tenant_id AND environment.application_id = deployment.application_id AND environment.id = deployment.environment_id AND environment.environment = ?", environment).
		Where("deployment.tenant_id = ? AND deployment.application_code = ? AND deployment.environment_code = ?", tenantID, applicationCode, environment).
		Take(&model).Error; err != nil {
		return application.SubsystemDeploymentState{}, mapManagementError(err)
	}
	return deploymentStateFromModel(model), nil
}

// MarkSubsystemInitialAccessAssigned 独立于 READY 记录授权侧的生效结果。若后续状态写入
// 或 HTTP 响应失败，重试可以看到访问已分配完成，不会再次授予管理员角色。收编（ADOPT）在
// 管理员未知前就创建生命周期行，因此同一原子更新会同时写入角色接收人。
func (repository *SubsystemOnboardingGORMRepository) MarkSubsystemInitialAccessAssigned(ctx context.Context, tenantID, applicationCode, environment, userID string, now time.Time) error {
	result := repository.database.WithContext(ctx).Model(&subsystemDeploymentStateModel{}).
		Where("tenant_id = ? AND application_code = ? AND environment_code = ? AND initial_access_assigned_at IS NULL", strings.TrimSpace(tenantID), strings.TrimSpace(applicationCode), strings.ToLower(strings.TrimSpace(environment))).
		Updates(map[string]any{
			"initial_admin_user_id":      strings.TrimSpace(userID),
			"initial_access_assigned_at": now.UTC(),
			"updated_at":                 now.UTC(),
		})
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected == 0 {
		var count int64
		if err := repository.database.WithContext(ctx).Model(&subsystemDeploymentStateModel{}).
			Where("tenant_id = ? AND application_code = ? AND environment_code = ? AND initial_access_assigned_at IS NOT NULL", strings.TrimSpace(tenantID), strings.TrimSpace(applicationCode), strings.ToLower(strings.TrimSpace(environment))).
			Count(&count).Error; err != nil {
			return err
		}
		if count != 1 {
			return application.ErrNotFound
		}
	}
	return nil
}

func timePointer(value time.Time) *time.Time { return &value }

func nullableString(value string) any {
	if strings.TrimSpace(value) == "" {
		return nil
	}
	return value
}

func modelString(value *string) string {
	if value == nil {
		return ""
	}
	return *value
}

func dereferenceString(value *string) string { return modelString(value) }

func optionalStringPointer(value string) *string {
	value = strings.TrimSpace(value)
	if value == "" {
		return nil
	}
	return &value
}

// portalApplicationAccessFilter 与有效应用授权使用相同的角色来源与目录策略：
// 当前 USER 或活跃任职继承的 APPLICATION 角色按 ID 去重，并遵守应用配置的
// max_effective_roles（缺省或零表示不限）。租户超级管理员可直接进入业务应用，
// 不要求额外物理绑定；旧直授权限或空角色目录不能单独构成有效应用授权。
func portalApplicationAccessFilter(userID string) (string, []any) {
	return `(
			application.code = 'platform'
			OR EXISTS (
				SELECT 1
				FROM authz_role_binding AS platform_admin_binding
				JOIN authz_role AS platform_admin_role
					ON platform_admin_role.id = platform_admin_binding.role_id
					AND platform_admin_role.tenant_id = platform_admin_binding.tenant_id
					AND platform_admin_role.status = 'ACTIVE'
					AND platform_admin_role.role_type = 'PLATFORM'
				JOIN platform_application AS platform_admin_application
					ON platform_admin_application.id = platform_admin_role.application_id
					AND platform_admin_application.tenant_id = platform_admin_role.tenant_id
					AND platform_admin_application.code = 'platform'
					AND platform_admin_application.status = 'ACTIVE'
				WHERE platform_admin_binding.tenant_id = application.tenant_id
					AND platform_admin_binding.subject_type = 'USER'
					AND platform_admin_binding.subject_id = ?
					AND platform_admin_binding.scope_type = 'TENANT'
					AND platform_admin_binding.scope_id = ''
					AND platform_admin_binding.status = 'ACTIVE'
					AND (platform_admin_binding.valid_from IS NULL OR platform_admin_binding.valid_from <= UTC_TIMESTAMP(3))
					AND (platform_admin_binding.valid_until IS NULL OR platform_admin_binding.valid_until > UTC_TIMESTAMP(3))
					AND platform_admin_role.code = 'platform-super-admin'
			)
			OR EXISTS (
				SELECT COUNT(DISTINCT assigned_role.id) FROM authz_role_binding AS access_assignment
				JOIN authz_role AS assigned_role ON assigned_role.id = access_assignment.role_id
				LEFT JOIN authz_application_authorization_policy AS access_policy
					ON access_policy.tenant_id = application.tenant_id
					AND access_policy.application_id = application.id
				WHERE access_assignment.tenant_id = application.tenant_id
					AND access_assignment.application_id = application.id
					AND (
						(access_assignment.subject_type = 'USER' AND access_assignment.subject_id = ?)
						OR (
							access_assignment.subject_type IN ('ORG_UNIT', 'POSITION')
							AND EXISTS (
								SELECT 1
								FROM iam_membership AS membership
								JOIN iam_org_unit AS organization
									ON organization.id = membership.org_unit_id
									AND organization.tenant_id = membership.tenant_id
									AND organization.status = 'ACTIVE'
								JOIN iam_position AS position
									ON position.id = membership.position_id
									AND position.tenant_id = membership.tenant_id
									AND position.org_unit_id = membership.org_unit_id
									AND position.status = 'ACTIVE'
								WHERE membership.tenant_id = access_assignment.tenant_id
									AND membership.user_id = ?
									AND membership.status = 'ACTIVE'
									AND membership.inherit_authorization = 1
									AND (membership.valid_from IS NULL OR membership.valid_from <= UTC_TIMESTAMP(3))
									AND (membership.valid_until IS NULL OR membership.valid_until > UTC_TIMESTAMP(3))
									AND (
										(access_assignment.subject_type = 'ORG_UNIT' AND access_assignment.subject_id = membership.org_unit_id)
										OR (access_assignment.subject_type = 'POSITION' AND access_assignment.subject_id = membership.position_id)
									)
							)
						)
					)
					AND access_assignment.status = 'ACTIVE'
					AND (access_assignment.valid_from IS NULL OR access_assignment.valid_from <= UTC_TIMESTAMP(3))
					AND (access_assignment.valid_until IS NULL OR access_assignment.valid_until > UTC_TIMESTAMP(3))
					AND (
						(access_assignment.scope_type = 'TENANT' AND access_assignment.scope_id = '')
						OR (access_assignment.scope_type = 'ENVIRONMENT' AND access_assignment.scope_id = environment.id)
					)
					AND assigned_role.tenant_id = application.tenant_id
					AND assigned_role.application_id = application.id
					AND assigned_role.status = 'ACTIVE'
					AND assigned_role.role_type = 'APPLICATION'
				HAVING COUNT(DISTINCT assigned_role.id) > 0
					AND (COALESCE(MAX(access_policy.max_effective_roles), 0) = 0
						OR COUNT(DISTINCT assigned_role.id) <= MAX(access_policy.max_effective_roles))
			)
		)`, []any{userID, userID, userID}
}
