package infrastructure

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/J-S-Te/Basic-Platform/internal/platform/authorization/applicationaccess"
	projectionapplication "github.com/J-S-Te/Basic-Platform/internal/platform/keycloakauthorization/application"
	projectionworker "github.com/J-S-Te/Basic-Platform/internal/platform/keycloakauthorization/worker"
	"gorm.io/gorm"
)

type projectionSourceRow struct {
	EnvironmentID    string `gorm:"column:environment_id"`
	KeycloakClientID string `gorm:"column:keycloak_client_id"`
	ApplicationCode  string `gorm:"column:application_code"`
}

type projectionUserRow struct {
	DisplayName string     `gorm:"column:display_name"`
	Email       *string    `gorm:"column:email"`
	Status      string     `gorm:"column:status"`
	DeletedAt   *time.Time `gorm:"column:deleted_at"`
}

type ProjectionSource struct {
	database *gorm.DB
	access   *applicationaccess.Service
}

func NewProjectionSource(database *gorm.DB, access *applicationaccess.Service) (*ProjectionSource, error) {
	if database == nil || access == nil {
		return nil, fmt.Errorf("Keycloak projection source dependencies must not be nil")
	}
	return &ProjectionSource{database: database, access: access}, nil
}

func (source *ProjectionSource) LoadAuthorizationProjection(ctx context.Context, event projectionworker.Event) (projectionapplication.Snapshot, error) {
	// The projection table is output state, so it must never be required before
	// the first synchronization. Client mappings are the authoritative input.
	var row projectionSourceRow
	err := keycloakClientMappingQuery(source.database.WithContext(ctx), event).Take(&row).Error
	if err != nil {
		return projectionapplication.Snapshot{}, fmt.Errorf("load Keycloak application Client mapping: %w", err)
	}
	var user projectionUserRow
	if err := projectionUserQuery(source.database.WithContext(ctx), event).Take(&user).Error; err != nil {
		return projectionapplication.Snapshot{}, fmt.Errorf("load Keycloak projection user: %w", err)
	}
	enabled := projectionUserEnabled(user)
	base := projectionapplication.Snapshot{TenantID: event.TenantID, IdentityID: event.IdentityID, ApplicationID: event.ApplicationID, EnvironmentID: row.EnvironmentID, ApplicationCode: strings.TrimSpace(row.ApplicationCode), KeycloakClientID: strings.TrimSpace(row.KeycloakClientID), DisplayName: strings.TrimSpace(user.DisplayName), UserEnabled: enabled}
	if user.Email != nil {
		base.Email = strings.TrimSpace(*user.Email)
	}
	// Disabled and business-deleted users are deliberately projected with no
	// authorization grants. applicationaccess only resolves claims for active
	// users, while this projection must still disable the existing Keycloak user.
	if !enabled {
		return base, nil
	}
	authorization, err := source.access.ResolveKeycloakAuthorizationSnapshot(ctx, event.TenantID, event.IdentityID, event.ApplicationID)
	if err != nil {
		return projectionapplication.Snapshot{}, err
	}
	base.TenantID = authorization.TenantID
	base.IdentityID = authorization.IdentityID
	base.ApplicationID = authorization.ApplicationID
	base.ApplicationCode = authorization.ApplicationCode
	base.PersonID = authorization.PersonID
	base.PrimaryOrganizationID = authorization.PrimaryOrgID
	base.OrganizationIDs = authorization.OrganizationIDs
	base.Roles = authorization.Roles
	base.Permissions = authorization.Permissions
	base.RoleConfigHash = authorization.RoleConfigHash
	base.AuthorizationRevision = authorization.AuthzRevision
	return base, nil
}

func keycloakClientMappingQuery(database *gorm.DB, event projectionworker.Event) *gorm.DB {
	return database.Table("keycloak_application_client_mapping AS client_mapping").
		Select("client_mapping.environment_id, client_mapping.keycloak_client_id, platform_application.code AS application_code").
		Joins("JOIN platform_application ON platform_application.tenant_id = client_mapping.tenant_id AND platform_application.id = client_mapping.application_id").
		Where("client_mapping.tenant_id = ? AND client_mapping.application_id = ? AND client_mapping.status = ?", event.TenantID, event.ApplicationID, "SYNCED").
		Where("client_mapping.environment_id = ?", event.EnvironmentID).
		Order("environment_id ASC")
}

func projectionUserQuery(database *gorm.DB, event projectionworker.Event) *gorm.DB {
	return database.Table("iam_user AS user_record").
		Select("display_name, email, status, deleted_at").
		Where("user_record.tenant_id = ? AND user_record.id = ?", event.TenantID, event.IdentityID)
}

func projectionUserEnabled(user projectionUserRow) bool {
	return strings.EqualFold(strings.TrimSpace(user.Status), "ACTIVE") && user.DeletedAt == nil
}

var _ projectionapplication.Source = (*ProjectionSource)(nil)
