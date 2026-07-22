package infrastructure

import (
	"context"
	"time"

	"github.com/J-S-Te/Basic-Platform/backend/internal/platform/applicationregistry/application"
	"gorm.io/gorm"
)

// loginTargetManagementModel is kept separate from the minimal runtime lookup model so control
// plane fields such as name, version and operator timestamps cannot accidentally leak into the
// anonymous post-login resolver.
type loginTargetManagementModel struct {
	ID            string    `gorm:"column:id;primaryKey"`
	TenantID      string    `gorm:"column:tenant_id"`
	ApplicationID string    `gorm:"column:application_id"`
	EnvironmentID string    `gorm:"column:environment_id"`
	TargetCode    string    `gorm:"column:target_code"`
	Name          string    `gorm:"column:name"`
	TargetURI     string    `gorm:"column:target_uri"`
	Status        string    `gorm:"column:status"`
	Version       uint64    `gorm:"column:version"`
	CreatedAt     time.Time `gorm:"column:created_at"`
	CreatedBy     *string   `gorm:"column:created_by"`
	UpdatedAt     time.Time `gorm:"column:updated_at"`
	UpdatedBy     *string   `gorm:"column:updated_by"`
}

func (loginTargetManagementModel) TableName() string { return "platform_application_login_target" }

// EnsureLoginTargetBoundary verifies that the requested environment belongs to the supplied
// tenant/application pair. Status is deliberately not filtered here: administrators may prepare
// targets before an application environment is activated, while runtime resolution independently
// requires every parent and the target itself to be ACTIVE.
func (repository *LoginTargetGORMRepository) EnsureLoginTargetBoundary(ctx context.Context, tenantID, applicationID, environmentID string) error {
	var count int64
	err := repository.database.WithContext(ctx).
		Table("platform_application_environment AS environment").
		Joins("JOIN platform_application AS application ON application.id = environment.application_id AND application.tenant_id = environment.tenant_id").
		Where("environment.tenant_id = ? AND environment.application_id = ? AND environment.id = ?", tenantID, applicationID, environmentID).
		Limit(1).
		Count(&count).Error
	if err != nil {
		return err
	}
	if count != 1 {
		return application.ErrNotFound
	}
	return nil
}

// ListLoginTargets returns one exact environment's targets with bounded stable pagination.
func (repository *LoginTargetGORMRepository) ListLoginTargets(ctx context.Context, tenantID, applicationID, environmentID string, query application.PageRequest) (application.PageResult[application.LoginTargetManagementItem], error) {
	database := loginTargetManagementScope(repository.database.WithContext(ctx), tenantID, applicationID, environmentID)
	if query.Status != "" {
		database = database.Where("status = ?", query.Status)
	}
	if query.Keyword != "" {
		keyword := "%" + query.Keyword + "%"
		database = database.Where("target_code LIKE ? OR name LIKE ? OR target_uri LIKE ?", keyword, keyword, keyword)
	}

	var total int64
	if err := database.Count(&total).Error; err != nil {
		return application.PageResult[application.LoginTargetManagementItem]{}, err
	}

	var rows []loginTargetManagementModel
	if err := database.Order("created_at DESC, id DESC").Offset((query.Page - 1) * query.PageSize).Limit(query.PageSize).Find(&rows).Error; err != nil {
		return application.PageResult[application.LoginTargetManagementItem]{}, err
	}
	items := make([]application.LoginTargetManagementItem, 0, len(rows))
	for _, row := range rows {
		items = append(items, toLoginTargetManagementItem(row))
	}
	return application.PageResult[application.LoginTargetManagementItem]{Items: items, Page: query.Page, PageSize: query.PageSize, Total: total}, nil
}

// CreateLoginTarget persists a registry-approved landing URI with an initial optimistic version.
func (repository *LoginTargetGORMRepository) CreateLoginTarget(ctx context.Context, input application.LoginTargetCreateInput, identifier string, now time.Time) (application.LoginTargetManagementItem, error) {
	model := loginTargetManagementModel{
		ID: identifier, TenantID: input.TenantID, ApplicationID: input.ApplicationID, EnvironmentID: input.EnvironmentID,
		TargetCode: input.TargetCode, Name: input.Name, TargetURI: input.TargetURI, Status: input.Status, Version: 1,
		CreatedAt: now.UTC(), CreatedBy: stringPointer(input.OperatorID), UpdatedAt: now.UTC(), UpdatedBy: stringPointer(input.OperatorID),
	}
	if err := repository.database.WithContext(ctx).Create(&model).Error; err != nil {
		return application.LoginTargetManagementItem{}, mapManagementError(err)
	}
	return toLoginTargetManagementItem(model), nil
}

// GetLoginTarget retrieves a target only from the exact parent boundary.
func (repository *LoginTargetGORMRepository) GetLoginTarget(ctx context.Context, tenantID, applicationID, environmentID, loginTargetID string) (application.LoginTargetManagementItem, error) {
	var model loginTargetManagementModel
	if err := loginTargetManagementScope(repository.database.WithContext(ctx), tenantID, applicationID, environmentID).
		Where("id = ?", loginTargetID).
		Take(&model).Error; err != nil {
		return application.LoginTargetManagementItem{}, mapManagementError(err)
	}
	return toLoginTargetManagementItem(model), nil
}

// UpdateLoginTarget updates only mutable target data under the current version. A zero-row update
// is resolved as either a not-found boundary or an optimistic-lock conflict without exposing
// cross-tenant existence.
func (repository *LoginTargetGORMRepository) UpdateLoginTarget(ctx context.Context, input application.LoginTargetUpdateInput, now time.Time) (application.LoginTargetManagementItem, error) {
	updates := map[string]any{
		"name": input.Name, "target_uri": input.TargetURI, "status": input.Status, "version": input.Version + 1,
		"updated_at": now.UTC(), "updated_by": stringPointer(input.OperatorID),
	}
	result := loginTargetManagementScope(repository.database.WithContext(ctx), input.TenantID, input.ApplicationID, input.EnvironmentID).
		Where("id = ? AND version = ?", input.LoginTargetID, input.Version).
		Updates(updates)
	if result.Error != nil {
		return application.LoginTargetManagementItem{}, mapManagementError(result.Error)
	}
	if result.RowsAffected == 0 {
		return application.LoginTargetManagementItem{}, repository.loginTargetVersionOrNotFound(ctx, input)
	}
	return repository.GetLoginTarget(ctx, input.TenantID, input.ApplicationID, input.EnvironmentID, input.LoginTargetID)
}

func (repository *LoginTargetGORMRepository) loginTargetVersionOrNotFound(ctx context.Context, input application.LoginTargetUpdateInput) error {
	item, err := repository.GetLoginTarget(ctx, input.TenantID, input.ApplicationID, input.EnvironmentID, input.LoginTargetID)
	if err != nil {
		return err
	}
	if item.Version != input.Version {
		return application.ErrVersionConflict
	}
	return application.ErrNotFound
}

func loginTargetManagementScope(database *gorm.DB, tenantID, applicationID, environmentID string) *gorm.DB {
	return database.Model(&loginTargetManagementModel{}).
		Where("tenant_id = ? AND application_id = ? AND environment_id = ?", tenantID, applicationID, environmentID)
}

func toLoginTargetManagementItem(model loginTargetManagementModel) application.LoginTargetManagementItem {
	return application.LoginTargetManagementItem{
		ID: model.ID, TenantID: model.TenantID, ApplicationID: model.ApplicationID, EnvironmentID: model.EnvironmentID,
		TargetCode: model.TargetCode, Name: model.Name, TargetURI: model.TargetURI, Status: model.Status, Version: model.Version,
		CreatedAt: model.CreatedAt, UpdatedAt: model.UpdatedAt,
	}
}
