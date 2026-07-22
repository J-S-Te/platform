package infrastructure

import (
	"context"
	"errors"

	"github.com/J-S-Te/Basic-Platform/backend/internal/platform/applicationregistry/application"
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
