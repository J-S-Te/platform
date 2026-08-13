package infrastructure

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/J-S-Te/Basic-Platform/internal/platform/applicationregistry/application"
	"gorm.io/gorm"
)

// SubsystemOnboardingGORMRepository coordinates the multi-table onboarding transaction and the
// active portal catalog query.
type SubsystemOnboardingGORMRepository struct {
	database *gorm.DB
}

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
		return "", "", err
	}
	return row.ApplicationID, row.EnvironmentID, nil
}

// ResolveEnvironmentIssuerAlias reads the persisted provider for the exact
// tenant/application/environment tuple. The cutover gate must distinguish a
// first platform -> Keycloak switch from an ordinary update of an environment
// that is already running on Keycloak.
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

func (subsystemServiceInstanceModel) TableName() string { return "subsystem_service_instance" }

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

func (repository *SubsystemOnboardingGORMRepository) MarkUnavailableSubsystemServiceInstances(ctx context.Context, before, now time.Time) error {
	return repository.database.WithContext(ctx).Model(&subsystemServiceInstanceModel{}).
		Where("last_seen_at IS NULL OR last_seen_at < ?", before).
		Updates(map[string]any{"status": application.SubsystemServiceStatusUnavailable, "updated_at": now}).Error
}

func subsystemServiceInstanceFromModel(row subsystemServiceInstanceModel) application.SubsystemServiceInstance {
	return application.SubsystemServiceInstance{TenantID: row.TenantID, ApplicationID: row.ApplicationID, EnvironmentID: row.EnvironmentID, ApplicationCode: row.ApplicationCode, Environment: row.Environment, ServiceName: row.ServiceName, ServiceRole: row.ServiceRole, Protocol: row.Protocol, InternalHost: row.InternalHost, InternalPort: row.InternalPort, PathPrefix: row.PathPrefix, HealthEndpoint: row.HealthEndpoint, Version: row.Version, Status: row.Status, LastSeenAt: row.LastSeenAt, CreatedAt: row.CreatedAt, UpdatedAt: row.UpdatedAt}
}

func (subsystemDeploymentStateModel) TableName() string { return "subsystem_deployment_state" }

// NewSubsystemOnboardingGORMRepository constructs the onboarding repository.
func NewSubsystemOnboardingGORMRepository(database *gorm.DB) (*SubsystemOnboardingGORMRepository, error) {
	if database == nil {
		return nil, errors.New("subsystem onboarding database must not be nil")
	}
	return &SubsystemOnboardingGORMRepository{database: database}, nil
}

// CreateSubsystem persists all control-plane resources atomically. A previously registered
// application with the same tenant-scoped code is reused only to add a new environment; existing
// environments, login targets and OAuth clients are never overwritten by this create-only flow.
func (repository *SubsystemOnboardingGORMRepository) CreateSubsystem(ctx context.Context, write application.SubsystemOnboardingWrite, now time.Time) (application.SubsystemOnboardingResult, error) {
	var result application.SubsystemOnboardingResult
	// 应用、环境、登录目标、浏览器客户端、目录发布客户端和初始部署状态必须同成同败。
	// 只要任一子资源写入失败，事务回滚，避免门户出现无法完成 OAuth 登录的半接入卡片。
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

		// The application might have been reused, so always bind every child write to
		// the canonical persisted application ID rather than the generated candidate ID.
		write.Environment.ApplicationID = createdApplication.ID
		write.LoginTarget.ApplicationID = createdApplication.ID
		write.OAuthClient.ApplicationID = createdApplication.ID
		write.CatalogPublisherOAuthClient.ApplicationID = createdApplication.ID
		for index := range write.ServiceClients {
			write.ServiceClients[index].OAuthClient.ApplicationID = createdApplication.ID
		}

		// Onboarding is intentionally create-only: an existing environment can already
		// own a LoginTarget and OAuth client whose secret cannot be recovered safely.
		// Detect it before any child write and return an actionable conflict instead of
		// a generic duplicate-key response.
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
			TenantID:           createdApplication.TenantID,
			ApplicationID:      createdApplication.ID,
			EnvironmentID:      createdEnvironment.ID,
			ApplicationCode:    createdApplication.Code,
			Environment:        createdEnvironment.Environment,
			InitialAdminUserID: optionalStringPointer(write.InitialAdminUserID),
			Status:             application.SubsystemDeploymentStatusProvisioning,
			Operation:          "ONBOARD",
			Generation:         1,
			AttemptCount:       1,
			StartedAt:          timePointer(now.UTC()),
			CreatedAt:          now.UTC(),
			UpdatedAt:          now.UTC(),
		}).Error; err != nil {
			return err
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

// CreateSubsystemDirectory persists the application catalogue portion of a
// subsystem registration.  It deliberately does not create an OAuth client,
// a service credential or deployment state: those belong to the authentication
// provider integration and runtime deployment workflows respectively.
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

// findApplicationByCode retrieves only the exact tenant-scoped application that caused a
// duplicate create attempt. It is intentionally private to the onboarding transaction so a
// request cannot use it to cross a tenant boundary or overwrite an existing application.
func (repository *SubsystemOnboardingGORMRepository) findApplicationByCode(ctx context.Context, database *gorm.DB, tenantID, code string) (application.Application, error) {
	var model managementApplicationModel
	if err := database.WithContext(ctx).Where("tenant_id = ? AND code = ?", tenantID, code).Take(&model).Error; err != nil {
		return application.Application{}, mapManagementError(err)
	}
	return toApplication(model), nil
}

// findEnvironmentByCode looks up an existing tenant/application environment for the create-only
// conflict check. Its caller must already have resolved the application in the same transaction.
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

// ListPortalApplications returns one preferred active environment/target per active application.
// Preference order without an explicit environment is prod, staging, test, dev, then lexical.
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

// TransitionSubsystemDeployment records a lifecycle transition using the registered application
// and environment codes. Error details are operator-safe and deliberately truncated before they
// enter the database; credentials and command output never belong in this table.
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
		// A generation identifies one lifecycle attempt; terminal transitions keep the same
		// generation so status polling can distinguish retries from completion updates.
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
	result := repository.database.WithContext(ctx).Model(&subsystemDeploymentStateModel{}).
		Where("tenant_id = ? AND application_code = ? AND environment_code = ?", strings.TrimSpace(tenantID), strings.TrimSpace(applicationCode), strings.ToLower(strings.TrimSpace(environment))).
		Updates(updates)
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected != 1 {
		return application.ErrNotFound
	}
	return nil
}

// GetSubsystemDeploymentState returns only tenant-scoped lifecycle metadata.
func (repository *SubsystemOnboardingGORMRepository) GetSubsystemDeploymentState(ctx context.Context, tenantID, applicationCode, environment string) (application.SubsystemDeploymentState, error) {
	var model subsystemDeploymentStateModel
	if err := repository.database.WithContext(ctx).
		Where("tenant_id = ? AND application_code = ? AND environment_code = ?", strings.TrimSpace(tenantID), strings.TrimSpace(applicationCode), strings.ToLower(strings.TrimSpace(environment))).
		Take(&model).Error; err != nil {
		return application.SubsystemDeploymentState{}, mapManagementError(err)
	}
	return deploymentStateFromModel(model), nil
}

func deploymentStateFromModel(model subsystemDeploymentStateModel) application.SubsystemDeploymentState {
	return application.SubsystemDeploymentState{
		TenantID: model.TenantID, ApplicationID: model.ApplicationID, EnvironmentID: model.EnvironmentID,
		ApplicationCode: model.ApplicationCode, Environment: model.Environment, Status: model.Status,
		InitialAdminUserID: modelString(model.InitialAdminUserID), InitialAccessAssignedAt: model.InitialAccessAssignedAt,
		Operation: model.Operation, Generation: model.Generation, AttemptCount: model.AttemptCount,
		LastErrorCode: dereferenceString(model.LastErrorCode), LastError: dereferenceString(model.LastError),
		StartedAt: model.StartedAt, CompletedAt: model.CompletedAt, CreatedAt: model.CreatedAt, UpdatedAt: model.UpdatedAt,
	}
}

// GetSubsystemDeploymentContext resolves application/environment identifiers from control-plane
// registration regardless of deployment status. Retry cannot use the portal projection because
// PROVISION_FAILED environments are intentionally hidden there.
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

// MarkSubsystemInitialAccessAssigned records the authorization side effect independently from
// READY. If a later state write or HTTP response fails, retry can see that access is already
// complete and will not grant the administrator role again.
func (repository *SubsystemOnboardingGORMRepository) MarkSubsystemInitialAccessAssigned(ctx context.Context, tenantID, applicationCode, environment string, now time.Time) error {
	result := repository.database.WithContext(ctx).Model(&subsystemDeploymentStateModel{}).
		Where("tenant_id = ? AND application_code = ? AND environment_code = ? AND initial_access_assigned_at IS NULL", strings.TrimSpace(tenantID), strings.TrimSpace(applicationCode), strings.ToLower(strings.TrimSpace(environment))).
		Updates(map[string]any{"initial_access_assigned_at": now.UTC(), "updated_at": now.UTC()})
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

// portalApplicationAccessFilter keeps portal visibility aligned with the effective authorization
// subject model. In addition to a direct USER binding, an active membership can contribute the
// role bound to its active organization or position. The role binding is still constrained to the
// tenant or the environment row currently being considered by the outer portal query.
func portalApplicationAccessFilter(userID string) (string, []any) {
	// 可见性由数据库中的有效授权事实决定，而不是前端缓存：直接用户绑定、开启继承的
	// 在职组织/岗位绑定或用户直授权限均可贡献访问。所有路径都同时约束租户、应用、
	// 环境范围和生效时间；合同系统另要求恰好一个有效角色，避免角色冲突进入子系统。
	return `(
			application.code = 'platform'
			OR NOT EXISTS (
				SELECT 1 FROM authz_role AS catalog_role
				WHERE catalog_role.tenant_id = application.tenant_id
					AND catalog_role.application_id = application.id
					AND catalog_role.status = 'ACTIVE'
					AND catalog_role.role_type <> 'COMPATIBILITY'
			)
			OR EXISTS (
				SELECT 1 FROM authz_role_binding AS access_assignment
				JOIN authz_role AS assigned_role ON assigned_role.id = access_assignment.role_id
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
					AND assigned_role.role_type <> 'COMPATIBILITY'
			)
			OR EXISTS (
				SELECT 1 FROM authz_user_permission AS direct_permission
				JOIN authz_permission AS assigned_permission ON assigned_permission.id = direct_permission.permission_id
				WHERE direct_permission.tenant_id = application.tenant_id
					AND direct_permission.application_id = application.id
					AND direct_permission.user_id = ?
					AND assigned_permission.tenant_id = application.tenant_id
					AND assigned_permission.application_id = application.id
					AND assigned_permission.status = 'ACTIVE'
			)
		)
		AND (
			application.code <> 'contract_management'
			OR (
				SELECT COUNT(DISTINCT contract_role.code)
				FROM authz_role_binding AS contract_assignment
				JOIN authz_role AS contract_role ON contract_role.id = contract_assignment.role_id
				WHERE contract_assignment.tenant_id = application.tenant_id
					AND contract_assignment.application_id = application.id
					AND (
						(contract_assignment.subject_type = 'USER' AND contract_assignment.subject_id = ?)
						OR (
							contract_assignment.subject_type IN ('ORG_UNIT', 'POSITION')
							AND EXISTS (
								SELECT 1
								FROM iam_membership AS contract_membership
								JOIN iam_org_unit AS contract_organization
									ON contract_organization.id = contract_membership.org_unit_id
									AND contract_organization.tenant_id = contract_membership.tenant_id
									AND contract_organization.status = 'ACTIVE'
								JOIN iam_position AS contract_position
									ON contract_position.id = contract_membership.position_id
									AND contract_position.tenant_id = contract_membership.tenant_id
									AND contract_position.org_unit_id = contract_membership.org_unit_id
									AND contract_position.status = 'ACTIVE'
								WHERE contract_membership.tenant_id = contract_assignment.tenant_id
									AND contract_membership.user_id = ?
									AND contract_membership.status = 'ACTIVE'
									AND contract_membership.inherit_authorization = 1
									AND (contract_membership.valid_from IS NULL OR contract_membership.valid_from <= UTC_TIMESTAMP(3))
									AND (contract_membership.valid_until IS NULL OR contract_membership.valid_until > UTC_TIMESTAMP(3))
									AND (
										(contract_assignment.subject_type = 'ORG_UNIT' AND contract_assignment.subject_id = contract_membership.org_unit_id)
										OR (contract_assignment.subject_type = 'POSITION' AND contract_assignment.subject_id = contract_membership.position_id)
									)
							)
						)
					)
					AND contract_assignment.status = 'ACTIVE'
					AND (contract_assignment.valid_from IS NULL OR contract_assignment.valid_from <= UTC_TIMESTAMP(3))
					AND (contract_assignment.valid_until IS NULL OR contract_assignment.valid_until > UTC_TIMESTAMP(3))
					AND (
						(contract_assignment.scope_type = 'TENANT' AND contract_assignment.scope_id = '')
						OR (contract_assignment.scope_type = 'ENVIRONMENT' AND contract_assignment.scope_id = environment.id)
					)
					AND contract_role.tenant_id = application.tenant_id
					AND contract_role.application_id = application.id
					AND contract_role.status = 'ACTIVE'
					AND contract_role.role_type <> 'COMPATIBILITY'
			) = 1
		)`, []any{userID, userID, userID, userID, userID}
}
