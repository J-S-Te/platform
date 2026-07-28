package infrastructure

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/J-S-Te/Basic-Platform/backend/internal/platform/applicationregistry/application"
	mysql "github.com/go-sql-driver/mysql"
	"gorm.io/gorm"
)

// ManagementRepository persists the application/environment aggregate without handling OAuth
// clients, callback URLs, scopes, grants, or credentials.
type ManagementRepository struct {
	database *gorm.DB
}

// NewManagementRepository constructs application/environment management persistence.
func NewManagementRepository(database *gorm.DB) (*ManagementRepository, error) {
	if database == nil {
		return nil, errors.New("application registry management database must not be nil")
	}
	return &ManagementRepository{database: database}, nil
}

type managementApplicationModel struct {
	ID              string    `gorm:"column:id;primaryKey"`
	TenantID        string    `gorm:"column:tenant_id"`
	Code            string    `gorm:"column:code"`
	Name            string    `gorm:"column:name"`
	ApplicationType string    `gorm:"column:application_type"`
	OwnerOrgID      *string   `gorm:"column:owner_org_id"`
	OwnerUserID     *string   `gorm:"column:owner_user_id"`
	HomepageURL     *string   `gorm:"column:homepage_url"`
	Description     *string   `gorm:"column:description"`
	Status          string    `gorm:"column:status"`
	Version         uint64    `gorm:"column:version"`
	CreatedAt       time.Time `gorm:"column:created_at"`
	CreatedBy       *string   `gorm:"column:created_by"`
	UpdatedAt       time.Time `gorm:"column:updated_at"`
	UpdatedBy       *string   `gorm:"column:updated_by"`
}

func (managementApplicationModel) TableName() string { return "platform_application" }

type managementEnvironmentModel struct {
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

func (managementEnvironmentModel) TableName() string { return "platform_application_environment" }

// ListApplications lists one tenant's applications using stable pagination.
func (repository *ManagementRepository) ListApplications(ctx context.Context, tenantID string, query application.PageRequest) (application.PageResult[application.Application], error) {
	database := repository.database.WithContext(ctx).Model(&managementApplicationModel{}).Where("tenant_id = ?", tenantID)
	if query.Status != "" {
		database = database.Where("status = ?", query.Status)
	}
	if query.Keyword != "" {
		keyword := "%" + query.Keyword + "%"
		database = database.Where("code LIKE ? OR name LIKE ? OR description LIKE ?", keyword, keyword, keyword)
	}

	var total int64
	if err := database.Count(&total).Error; err != nil {
		return application.PageResult[application.Application]{}, err
	}

	var rows []managementApplicationModel
	if err := database.Order("created_at DESC, id DESC").Offset((query.Page - 1) * query.PageSize).Limit(query.PageSize).Find(&rows).Error; err != nil {
		return application.PageResult[application.Application]{}, err
	}
	items := make([]application.Application, 0, len(rows))
	for _, row := range rows {
		items = append(items, toApplication(row))
	}
	return application.PageResult[application.Application]{Items: items, Page: query.Page, PageSize: query.PageSize, Total: total}, nil
}

// CreateApplication writes the application registration with its initial version.
func (repository *ManagementRepository) CreateApplication(ctx context.Context, input application.ApplicationCreateInput, applicationID string, now time.Time) (application.Application, error) {
	model := managementApplicationModel{
		ID: applicationID, TenantID: input.TenantID, Code: input.Code, Name: input.Name, ApplicationType: input.ApplicationType,
		OwnerOrgID: copyString(input.OwnerOrgID), OwnerUserID: copyString(input.OwnerUserID), HomepageURL: copyString(input.HomepageURL),
		Description: copyString(input.Description), Status: input.Status, Version: 1, CreatedAt: now.UTC(), CreatedBy: stringPointer(input.OperatorID),
		UpdatedAt: now.UTC(), UpdatedBy: stringPointer(input.OperatorID),
	}
	if err := repository.database.WithContext(ctx).Create(&model).Error; err != nil {
		return application.Application{}, mapManagementError(err)
	}
	return toApplication(model), nil
}

// GetApplication resolves an application only within the specified tenant.
func (repository *ManagementRepository) GetApplication(ctx context.Context, tenantID, applicationID string) (application.Application, error) {
	var model managementApplicationModel
	if err := repository.database.WithContext(ctx).Where("tenant_id = ? AND id = ?", tenantID, applicationID).Take(&model).Error; err != nil {
		return application.Application{}, mapManagementError(err)
	}
	return toApplication(model), nil
}

// UpdateApplication applies a versioned mutation and leaves the stable code untouched.
func (repository *ManagementRepository) UpdateApplication(ctx context.Context, input application.ApplicationUpdateInput, now time.Time) (application.Application, error) {
	updates := map[string]any{
		"name":             input.Name,
		"application_type": input.ApplicationType,
		"owner_org_id":     copyString(input.OwnerOrgID),
		"owner_user_id":    copyString(input.OwnerUserID),
		"homepage_url":     copyString(input.HomepageURL),
		"description":      copyString(input.Description),
		"status":           input.Status,
		"version":          input.Version + 1,
		"updated_at":       now.UTC(),
		"updated_by":       stringPointer(input.OperatorID),
	}
	result := repository.database.WithContext(ctx).Model(&managementApplicationModel{}).
		Where("tenant_id = ? AND id = ? AND version = ?", input.TenantID, input.ApplicationID, input.Version).
		Updates(updates)
	if result.Error != nil {
		return application.Application{}, mapManagementError(result.Error)
	}
	if result.RowsAffected == 0 {
		return application.Application{}, repository.versionOrNotFound(ctx, input.TenantID, input.ApplicationID)
	}
	return repository.GetApplication(ctx, input.TenantID, input.ApplicationID)
}

// ListEnvironments lists a tenant/application-scoped environment collection.
func (repository *ManagementRepository) ListEnvironments(ctx context.Context, tenantID, applicationID string, query application.PageRequest) (application.PageResult[application.Environment], error) {
	database := repository.database.WithContext(ctx).Model(&managementEnvironmentModel{}).Where("tenant_id = ? AND application_id = ?", tenantID, applicationID)
	if query.Status != "" {
		database = database.Where("status = ?", query.Status)
	}
	if query.Keyword != "" {
		keyword := "%" + query.Keyword + "%"
		database = database.Where("environment LIKE ? OR base_url LIKE ? OR issuer_alias LIKE ? OR upstream_url LIKE ? OR path_prefix LIKE ?", keyword, keyword, keyword, keyword, keyword)
	}

	var total int64
	if err := database.Count(&total).Error; err != nil {
		return application.PageResult[application.Environment]{}, err
	}

	var rows []managementEnvironmentModel
	if err := database.Order("environment ASC, id ASC").Offset((query.Page - 1) * query.PageSize).Limit(query.PageSize).Find(&rows).Error; err != nil {
		return application.PageResult[application.Environment]{}, err
	}
	items := make([]application.Environment, 0, len(rows))
	for _, row := range rows {
		items = append(items, toEnvironment(row))
	}
	return application.PageResult[application.Environment]{Items: items, Page: query.Page, PageSize: query.PageSize, Total: total}, nil
}

// CreateEnvironment writes an isolated deployment environment beneath an application.
func (repository *ManagementRepository) CreateEnvironment(ctx context.Context, input application.EnvironmentCreateInput, environmentID string, now time.Time) (application.Environment, error) {
	model := managementEnvironmentModel{
		ID: environmentID, TenantID: input.TenantID, ApplicationID: input.ApplicationID, Environment: input.Environment,
		BaseURL: copyString(input.BaseURL), UpstreamURL: copyString(input.UpstreamURL), PathPrefix: copyString(input.PathPrefix),
		IssuerAlias: copyString(input.IssuerAlias), Metadata: copyJSON(input.Metadata), Status: input.Status,
		Version: 1, CreatedAt: now.UTC(), CreatedBy: stringPointer(input.OperatorID), UpdatedAt: now.UTC(), UpdatedBy: stringPointer(input.OperatorID),
	}
	if err := repository.database.WithContext(ctx).Create(&model).Error; err != nil {
		return application.Environment{}, mapManagementError(err)
	}
	return toEnvironment(model), nil
}

// GetEnvironment resolves an environment through both tenant and application boundaries.
func (repository *ManagementRepository) GetEnvironment(ctx context.Context, tenantID, applicationID, environmentID string) (application.Environment, error) {
	var model managementEnvironmentModel
	if err := repository.database.WithContext(ctx).Where("tenant_id = ? AND application_id = ? AND id = ?", tenantID, applicationID, environmentID).Take(&model).Error; err != nil {
		return application.Environment{}, mapManagementError(err)
	}
	return toEnvironment(model), nil
}

// UpdateEnvironment applies a versioned metadata and status mutation.
func (repository *ManagementRepository) UpdateEnvironment(ctx context.Context, input application.EnvironmentUpdateInput, now time.Time) (application.Environment, error) {
	updates := map[string]any{
		"base_url":     copyString(input.BaseURL),
		"upstream_url": copyString(input.UpstreamURL),
		"path_prefix":  copyString(input.PathPrefix),
		"issuer_alias": copyString(input.IssuerAlias),
		"metadata":     copyJSON(input.Metadata),
		"status":       input.Status,
		"version":      input.Version + 1,
		"updated_at":   now.UTC(),
		"updated_by":   stringPointer(input.OperatorID),
	}
	result := repository.database.WithContext(ctx).Model(&managementEnvironmentModel{}).
		Where("tenant_id = ? AND application_id = ? AND id = ? AND version = ?", input.TenantID, input.ApplicationID, input.EnvironmentID, input.Version).
		Updates(updates)
	if result.Error != nil {
		return application.Environment{}, mapManagementError(result.Error)
	}
	if result.RowsAffected == 0 {
		return application.Environment{}, repository.environmentVersionOrNotFound(ctx, input.TenantID, input.ApplicationID, input.EnvironmentID)
	}
	return repository.GetEnvironment(ctx, input.TenantID, input.ApplicationID, input.EnvironmentID)
}

func (repository *ManagementRepository) versionOrNotFound(ctx context.Context, tenantID, applicationID string) error {
	var count int64
	if err := repository.database.WithContext(ctx).Model(&managementApplicationModel{}).Where("tenant_id = ? AND id = ?", tenantID, applicationID).Count(&count).Error; err != nil {
		return err
	}
	if count == 0 {
		return application.ErrNotFound
	}
	return application.ErrVersionConflict
}

func (repository *ManagementRepository) environmentVersionOrNotFound(ctx context.Context, tenantID, applicationID, environmentID string) error {
	var count int64
	if err := repository.database.WithContext(ctx).Model(&managementEnvironmentModel{}).Where("tenant_id = ? AND application_id = ? AND id = ?", tenantID, applicationID, environmentID).Count(&count).Error; err != nil {
		return err
	}
	if count == 0 {
		return application.ErrNotFound
	}
	return application.ErrVersionConflict
}

func toApplication(model managementApplicationModel) application.Application {
	return application.Application{
		ID: model.ID, TenantID: model.TenantID, Code: model.Code, Name: model.Name, ApplicationType: model.ApplicationType,
		OwnerOrgID: copyString(model.OwnerOrgID), OwnerUserID: copyString(model.OwnerUserID), HomepageURL: copyString(model.HomepageURL),
		Description: copyString(model.Description), Status: model.Status, Version: model.Version, CreatedAt: model.CreatedAt, UpdatedAt: model.UpdatedAt,
	}
}

func toEnvironment(model managementEnvironmentModel) application.Environment {
	return application.Environment{
		ID: model.ID, TenantID: model.TenantID, ApplicationID: model.ApplicationID, Environment: model.Environment,
		BaseURL: copyString(model.BaseURL), UpstreamURL: copyString(model.UpstreamURL), PathPrefix: copyString(model.PathPrefix),
		IssuerAlias: copyString(model.IssuerAlias), Metadata: copyJSON(model.Metadata), Status: model.Status,
		Version: model.Version, CreatedAt: model.CreatedAt, UpdatedAt: model.UpdatedAt,
	}
}

func stringPointer(value string) *string { return &value }

func copyString(value *string) *string {
	if value == nil {
		return nil
	}
	copied := *value
	return &copied
}

func copyJSON(value json.RawMessage) json.RawMessage {
	if len(value) == 0 {
		return nil
	}
	return append(json.RawMessage(nil), value...)
}

func mapManagementError(err error) error {
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return application.ErrNotFound
	}
	// mysql.go enables GORM TranslateError, which converts duplicate-key errors into
	// gorm.ErrDuplicatedKey before they reach this repository. Map both forms so a
	// client conflict never leaks through the HTTP handler as a generic 500.
	if errors.Is(err, gorm.ErrDuplicatedKey) {
		return application.ErrConflict
	}
	var mysqlError *mysql.MySQLError
	if errors.As(err, &mysqlError) && mysqlError.Number == 1062 {
		return application.ErrConflict
	}
	return err
}
