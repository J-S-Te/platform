// Package infrastructure implements the owner directory with tenant-scoped SQL.
package infrastructure

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/J-S-Te/Basic-Platform/backend/internal/platform/ownerdirectory/application"
	"github.com/J-S-Te/Basic-Platform/backend/internal/platform/ownerdirectory/domain"
	"gorm.io/gorm"
)

type Repository struct {
	db  *gorm.DB
	now func() time.Time
}

func NewRepository(db *gorm.DB) (*Repository, error) {
	if db == nil {
		return nil, errors.New("owner directory database must not be nil")
	}
	return &Repository{db: db, now: time.Now}, nil
}

type userRow struct {
	UserID      string `gorm:"column:user_id"`
	DisplayName string `gorm:"column:display_name"`
}

type membershipRow struct {
	UserID           string `gorm:"column:user_id"`
	OrganizationID   string `gorm:"column:organization_id"`
	OrganizationName string `gorm:"column:organization_name"`
	IsPrimary        bool   `gorm:"column:is_primary"`
}

func (repository *Repository) List(ctx context.Context, tenantID, applicationID, environmentID string, query application.Query) (domain.Page, error) {
	now := repository.now().UTC()
	authorized := repository.db.WithContext(ctx).Table("iam_user AS user").
		Select("DISTINCT user.id AS user_id, user.display_name AS display_name").
		Joins("JOIN iam_membership AS membership ON membership.tenant_id = user.tenant_id AND membership.user_id = user.id").
		Joins("JOIN iam_org_unit AS organization ON organization.tenant_id = membership.tenant_id AND organization.id = membership.org_unit_id").
		Where("user.tenant_id = ? AND user.status = ? AND user.deleted_at IS NULL AND user.employment_status <> ?", tenantID, "ACTIVE", "EXTERNAL_CUSTOMER").
		Where("membership.status = ? AND (membership.valid_from IS NULL OR membership.valid_from <= ?) AND (membership.valid_until IS NULL OR membership.valid_until > ?)", "ACTIVE", now, now).
		Where("organization.status = ?", "ACTIVE").
		Where(`EXISTS (
			SELECT 1 FROM authz_role_binding AS binding
			JOIN authz_role AS role ON role.id = binding.role_id AND role.tenant_id = binding.tenant_id AND role.application_id = binding.application_id
			WHERE binding.tenant_id = user.tenant_id AND binding.application_id = ?
			AND binding.status = ? AND role.status = ? AND role.role_type <> ?
			AND (binding.valid_from IS NULL OR binding.valid_from <= ?)
			AND (binding.valid_until IS NULL OR binding.valid_until > ?)
			AND ((binding.scope_type = ? AND binding.scope_id = '') OR (binding.scope_type = ? AND binding.scope_id = ?))
			AND ((binding.subject_type = ? AND binding.subject_id = user.id)
				OR (membership.inherit_authorization = 1 AND binding.subject_type = ? AND binding.subject_id = membership.org_unit_id)
				OR (membership.inherit_authorization = 1 AND binding.subject_type = ? AND binding.subject_id = membership.position_id
					AND EXISTS (SELECT 1 FROM iam_position AS position WHERE position.tenant_id = membership.tenant_id AND position.id = membership.position_id AND position.org_unit_id = membership.org_unit_id AND position.status = ?)))
		)`, applicationID, "ACTIVE", "ACTIVE", "COMPATIBILITY", now, now, "TENANT", "ENVIRONMENT", environmentID, "USER", "ORG_UNIT", "POSITION", "ACTIVE")
	if query.UserID != "" {
		authorized = authorized.Where("user.id = ?", query.UserID)
	} else if query.Keyword != "" {
		like := "%" + strings.ReplaceAll(strings.ReplaceAll(query.Keyword, "\\", "\\\\"), "%", "\\%") + "%"
		like = strings.ReplaceAll(like, "_", "\\_")
		authorized = authorized.Where("user.display_name LIKE ? ESCAPE '\\\\' OR user.id = ?", like, query.Keyword)
	}

	var total int64
	if err := repository.db.WithContext(ctx).Table("(?) AS authorized_users", authorized).Count(&total).Error; err != nil {
		return domain.Page{}, err
	}
	rows := make([]userRow, 0, query.PageSize)
	if err := authorized.Order("user.display_name ASC, user.id ASC").Offset((query.Page - 1) * query.PageSize).Limit(query.PageSize).Scan(&rows).Error; err != nil {
		return domain.Page{}, err
	}
	result := domain.Page{Items: make([]domain.User, 0, len(rows)), Page: query.Page, PageSize: query.PageSize, Total: total}
	if len(rows) == 0 {
		return result, nil
	}
	userIDs := make([]string, 0, len(rows))
	byID := make(map[string]*domain.User, len(rows))
	for _, row := range rows {
		item := domain.User{ID: row.UserID, DisplayName: row.DisplayName, Organizations: []domain.Organization{}}
		result.Items = append(result.Items, item)
		userIDs = append(userIDs, row.UserID)
		byID[row.UserID] = &result.Items[len(result.Items)-1]
	}
	memberships := make([]membershipRow, 0)
	if err := repository.db.WithContext(ctx).Table("iam_membership AS membership").
		Select("membership.user_id, organization.id AS organization_id, organization.name AS organization_name, membership.is_primary").
		Joins("JOIN iam_org_unit AS organization ON organization.tenant_id = membership.tenant_id AND organization.id = membership.org_unit_id").
		Where("membership.tenant_id = ? AND membership.user_id IN ? AND membership.status = ?", tenantID, userIDs, "ACTIVE").
		Where("(membership.valid_from IS NULL OR membership.valid_from <= ?) AND (membership.valid_until IS NULL OR membership.valid_until > ?)", now, now).
		Where("organization.status = ?", "ACTIVE").
		Where(`EXISTS (
			SELECT 1 FROM authz_role_binding AS binding
			JOIN authz_role AS role ON role.id = binding.role_id AND role.tenant_id = binding.tenant_id AND role.application_id = binding.application_id
			WHERE binding.tenant_id = membership.tenant_id AND binding.application_id = ?
			AND binding.status = ? AND role.status = ? AND role.role_type <> ?
			AND (binding.valid_from IS NULL OR binding.valid_from <= ?)
			AND (binding.valid_until IS NULL OR binding.valid_until > ?)
			AND ((binding.scope_type = ? AND binding.scope_id = '') OR (binding.scope_type = ? AND binding.scope_id = ?))
			AND ((binding.subject_type = ? AND binding.subject_id = membership.user_id)
				OR (membership.inherit_authorization = 1 AND binding.subject_type = ? AND binding.subject_id = membership.org_unit_id)
				OR (membership.inherit_authorization = 1 AND binding.subject_type = ? AND binding.subject_id = membership.position_id
					AND EXISTS (SELECT 1 FROM iam_position AS position WHERE position.tenant_id = membership.tenant_id AND position.id = membership.position_id AND position.org_unit_id = membership.org_unit_id AND position.status = ?)))
		)`, applicationID, "ACTIVE", "ACTIVE", "COMPATIBILITY", now, now, "TENANT", "ENVIRONMENT", environmentID, "USER", "ORG_UNIT", "POSITION", "ACTIVE").
		Order("membership.is_primary DESC, organization.name ASC, organization.id ASC").Scan(&memberships).Error; err != nil {
		return domain.Page{}, err
	}
	for _, membership := range memberships {
		if user := byID[membership.UserID]; user != nil {
			user.Organizations = append(user.Organizations, domain.Organization{ID: membership.OrganizationID, Name: membership.OrganizationName, IsPrimary: membership.IsPrimary})
		}
	}
	return result, nil
}
