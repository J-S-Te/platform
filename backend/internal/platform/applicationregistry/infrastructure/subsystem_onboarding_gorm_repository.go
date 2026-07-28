package infrastructure

import (
	"context"
	"errors"
	"time"

	"github.com/J-S-Te/Basic-Platform/backend/internal/platform/applicationregistry/application"
	"gorm.io/gorm"
)

// SubsystemOnboardingGORMRepository coordinates the multi-table onboarding transaction and the
// active portal catalog query.
type SubsystemOnboardingGORMRepository struct {
	database *gorm.DB
}

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

		result = application.SubsystemOnboardingResult{
			Application: createdApplication, Environment: createdEnvironment,
			LoginTarget: createdLoginTarget, OAuthClient: createdOAuthClient,
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

type portalApplicationRow struct {
	ApplicationID string  `gorm:"column:application_id"`
	Code          string  `gorm:"column:code"`
	Name          string  `gorm:"column:name"`
	Description   *string `gorm:"column:description"`
	EnvironmentID string  `gorm:"column:environment_id"`
	Environment   string  `gorm:"column:environment"`
	BaseURL       string  `gorm:"column:base_url"`
	PathPrefix    *string `gorm:"column:path_prefix"`
	TargetCode    string  `gorm:"column:target_code"`
	TargetURI     string  `gorm:"column:target_uri"`
}

// ListPortalApplications returns one preferred active environment/target per active application.
// Preference order without an explicit environment is prod, staging, test, dev, then lexical.
func (repository *SubsystemOnboardingGORMRepository) ListPortalApplications(ctx context.Context, tenantID, environment string) ([]application.PortalApplication, error) {
	query := repository.database.WithContext(ctx).
		Table("platform_application AS application").
		Select(`application.id AS application_id, application.code, application.name, application.description,
			environment.id AS environment_id, environment.environment, environment.base_url, environment.path_prefix,
			target.target_code, target.target_uri`).
		Joins("JOIN platform_application_environment AS environment ON environment.application_id = application.id AND environment.tenant_id = application.tenant_id").
		Joins("JOIN platform_application_login_target AS target ON target.environment_id = environment.id AND target.application_id = application.id AND target.tenant_id = application.tenant_id").
		Where("application.tenant_id = ? AND application.status = ? AND environment.status = ? AND target.status = ?", tenantID, "ACTIVE", "ACTIVE", "ACTIVE")
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
		if _, exists := seen[row.ApplicationID]; exists {
			continue
		}
		seen[row.ApplicationID] = struct{}{}
		items = append(items, application.PortalApplication{
			ApplicationID: row.ApplicationID, Code: row.Code, Name: row.Name, Description: row.Description,
			EnvironmentID: row.EnvironmentID, Environment: row.Environment, PathPrefix: row.PathPrefix,
			TargetCode: row.TargetCode, TargetURI: row.TargetURI, PublicURL: row.BaseURL,
		})
	}
	return items, nil
}
