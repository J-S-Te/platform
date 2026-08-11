package infrastructure

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/J-S-Te/Basic-Platform/internal/platform/applicationregistry/application"
	"gorm.io/gorm"
)

const (
	activeApplicationLoginTargetStatus = "ACTIVE"
	activeLoginTargetParentStatus      = "ACTIVE"
)

// LoginTargetGORMRepository resolves registered application landing targets through GORM. It is
// a runtime read model only and does not manage OAuth redirect URI registrations.
type LoginTargetGORMRepository struct {
	database *gorm.DB
}

// NewLoginTargetGORMRepository constructs the internal login-target persistence adapter.
func NewLoginTargetGORMRepository(database *gorm.DB) (*LoginTargetGORMRepository, error) {
	if database == nil {
		return nil, errors.New("application login target database must not be nil")
	}
	return &LoginTargetGORMRepository{database: database}, nil
}

type loginTargetModel struct {
	ID            string `gorm:"column:id;primaryKey"`
	TenantID      string `gorm:"column:tenant_id"`
	ApplicationID string `gorm:"column:application_id"`
	EnvironmentID string `gorm:"column:environment_id"`
	TargetCode    string `gorm:"column:target_code"`
	TargetURI     string `gorm:"column:target_uri"`
	Status        string `gorm:"column:status"`
}

func (loginTargetModel) TableName() string { return "platform_application_login_target" }

type loginTargetEnvironmentModel struct {
	ID            string          `gorm:"column:id;primaryKey"`
	TenantID      string          `gorm:"column:tenant_id"`
	ApplicationID string          `gorm:"column:application_id"`
	Environment   string          `gorm:"column:environment"`
	BaseURL       *string         `gorm:"column:base_url"`
	UpstreamURL   *string         `gorm:"column:upstream_url"`
	PathPrefix    *string         `gorm:"column:path_prefix"`
	IssuerAlias   *string         `gorm:"column:issuer_alias"`
	Metadata      json.RawMessage `gorm:"column:metadata"`
	Status        string          `gorm:"column:status"`
	Version       uint64          `gorm:"column:version"`
	CreatedAt     time.Time       `gorm:"column:created_at"`
	CreatedBy     *string         `gorm:"column:created_by"`
	UpdatedAt     time.Time       `gorm:"column:updated_at"`
	UpdatedBy     *string         `gorm:"column:updated_by"`
}

func (loginTargetEnvironmentModel) TableName() string { return "platform_application_environment" }

// FindActiveLoginTarget performs an exact lookup across all four resolver inputs. Parent joins
// enforce that the environment belongs to the same active tenant/application boundary; inactive
// or inconsistent data therefore fails closed as not found.
func (repository *LoginTargetGORMRepository) FindActiveLoginTarget(ctx context.Context, input application.LoginTargetResolveInput) (application.LoginTarget, error) {
	var model loginTargetModel
	err := activeLoginTargetQuery(repository.database.WithContext(ctx), input).Take(&model).Error
	if err != nil {
		return application.LoginTarget{}, mapManagementError(err)
	}
	return toLoginTarget(model), nil
}

// FindActiveEnvironment returns the parent environment for a login-target resolution. It enforces
// the same tenant/application/environment boundary as the target resolver and only returns rows
// that are linked to an ACTIVE parent application. The method exists so the runtime target
// resolver can expand relative TargetURIs without depending on the management repository.
func (repository *LoginTargetGORMRepository) FindActiveEnvironment(ctx context.Context, tenantID, applicationID, environmentID string) (application.Environment, error) {
	var model loginTargetEnvironmentModel
	database := repository.database.WithContext(ctx).
		Table("platform_application_environment AS environment").
		Select("environment.id, environment.tenant_id, environment.application_id, environment.environment, environment.base_url, environment.upstream_url, environment.path_prefix, environment.issuer_alias, environment.metadata, environment.status, environment.version, environment.created_at, environment.created_by, environment.updated_at, environment.updated_by").
		Joins("JOIN platform_application AS registered_application ON registered_application.id = environment.application_id AND registered_application.tenant_id = environment.tenant_id AND registered_application.status = ?", activeLoginTargetParentStatus).
		Where("environment.tenant_id = ? AND environment.application_id = ? AND environment.id = ? AND environment.status = ?",
			tenantID, applicationID, environmentID, activeLoginTargetParentStatus)
	if err := database.Take(&model).Error; err != nil {
		return application.Environment{}, mapManagementError(err)
	}
	return loginTargetEnvironmentToEnvironment(model), nil
}

func activeLoginTargetQuery(database *gorm.DB, input application.LoginTargetResolveInput) *gorm.DB {
	return database.
		Table("platform_application_login_target AS login_target").
		Select("login_target.id, login_target.tenant_id, login_target.application_id, login_target.environment_id, login_target.target_code, login_target.target_uri, login_target.status").
		Joins("JOIN platform_application AS registered_application ON registered_application.id = login_target.application_id AND registered_application.tenant_id = login_target.tenant_id AND registered_application.status = ?", activeLoginTargetParentStatus).
		Joins("JOIN platform_application_environment AS registered_environment ON registered_environment.id = login_target.environment_id AND registered_environment.tenant_id = login_target.tenant_id AND registered_environment.application_id = login_target.application_id AND registered_environment.status = ?", activeLoginTargetParentStatus).
		Where("login_target.tenant_id = ? AND login_target.application_id = ? AND login_target.environment_id = ? AND login_target.target_code = ? AND login_target.status = ?",
			input.TenantID, input.ApplicationID, input.EnvironmentID, input.TargetCode, activeApplicationLoginTargetStatus)
}

func toLoginTarget(model loginTargetModel) application.LoginTarget {
	return application.LoginTarget{
		ID:            model.ID,
		TenantID:      model.TenantID,
		ApplicationID: model.ApplicationID,
		EnvironmentID: model.EnvironmentID,
		TargetCode:    model.TargetCode,
		TargetURI:     model.TargetURI,
		Status:        model.Status,
	}
}

func loginTargetEnvironmentToEnvironment(model loginTargetEnvironmentModel) application.Environment {
	copiedBase := model.BaseURL
	if copiedBase != nil {
		value := *copiedBase
		copiedBase = &value
	}
	copiedUpstream := model.UpstreamURL
	if copiedUpstream != nil {
		value := *copiedUpstream
		copiedUpstream = &value
	}
	copiedPrefix := model.PathPrefix
	if copiedPrefix != nil {
		value := *copiedPrefix
		copiedPrefix = &value
	}
	copiedAlias := model.IssuerAlias
	if copiedAlias != nil {
		value := *copiedAlias
		copiedAlias = &value
	}
	copiedMetadata := append(json.RawMessage(nil), model.Metadata...)
	return application.Environment{
		ID: model.ID, TenantID: model.TenantID, ApplicationID: model.ApplicationID, Environment: model.Environment,
		BaseURL: copiedBase, UpstreamURL: copiedUpstream, PathPrefix: copiedPrefix,
		IssuerAlias: copiedAlias, Metadata: copiedMetadata, Status: model.Status,
		Version: model.Version, CreatedAt: model.CreatedAt, UpdatedAt: model.UpdatedAt,
	}
}
