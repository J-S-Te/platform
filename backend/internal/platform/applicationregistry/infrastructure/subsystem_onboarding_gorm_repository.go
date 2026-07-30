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
		write.CatalogPublisherOAuthClient.ApplicationID = createdApplication.ID

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

		result = application.SubsystemOnboardingResult{
			Application: createdApplication, Environment: createdEnvironment,
			LoginTarget: createdLoginTarget, OAuthClient: createdOAuthClient,
			CatalogPublisherOAuthClient: createdCatalogPublisherOAuthClient,
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
func (repository *SubsystemOnboardingGORMRepository) ListPortalApplications(ctx context.Context, tenantID, userID, environment string) ([]application.PortalApplication, error) {
	accessFilter, accessArgs := portalApplicationAccessFilter(userID)
	query := repository.database.WithContext(ctx).
		Table("platform_application AS application").
		Select(`application.id AS application_id, application.code, application.name, application.description,
			environment.id AS environment_id, environment.environment, environment.base_url, environment.path_prefix,
			target.target_code, target.target_uri`).
		Joins("JOIN platform_application_environment AS environment ON environment.application_id = application.id AND environment.tenant_id = application.tenant_id").
		Joins("JOIN platform_application_login_target AS target ON target.environment_id = environment.id AND target.application_id = application.id AND target.tenant_id = application.tenant_id").
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

// portalApplicationAccessFilter keeps portal visibility aligned with the effective authorization
// subject model. In addition to a direct USER binding, an active membership can contribute the
// role bound to its active organization or position. The role binding is still constrained to the
// tenant or the environment row currently being considered by the outer portal query.
func portalApplicationAccessFilter(userID string) (string, []any) {
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
