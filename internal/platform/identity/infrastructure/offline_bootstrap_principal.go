package infrastructure

import (
	"context"
	"fmt"
	"time"

	"github.com/J-S-Te/Basic-Platform/internal/platform/identity/application"
	"github.com/J-S-Te/Basic-Platform/internal/platform/identity/domain"
	"github.com/J-S-Te/Basic-Platform/internal/shared/authctx"
	"gorm.io/gorm"
)

// OfflineBootstrapPrincipal resolves only the persisted first administrator.
// It is for a host-root maintenance process, never an HTTP authentication path.
// No session, token, password reset or synthetic role is created.
func (repository *GORMRepository) OfflineBootstrapPrincipal(ctx context.Context, now time.Time) (authctx.Principal, error) {
	var row principalProjection
	result := offlineBootstrapProjectionQuery(repository.database.WithContext(ctx), now).Find(&row)
	if result.Error != nil {
		return authctx.Principal{}, fmt.Errorf("resolve offline bootstrap administrator: %w", result.Error)
	}
	if result.RowsAffected != 1 {
		return authctx.Principal{}, application.ErrUnauthenticated
	}
	roles, err := repository.findRoles(ctx, row.TenantID, row.UserID, row.AccountID, now.UTC())
	if err != nil {
		return authctx.Principal{}, err
	}
	principal := authctx.Principal{Tenant: authctx.ReferenceName{ID: row.TenantID, Name: row.TenantName, Code: row.TenantCode}, User: authctx.ReferenceName{ID: row.UserID, Name: row.UserName}, Account: authctx.ReferenceName{ID: row.AccountID, Name: row.AccountName}}
	allowed := false
	for _, role := range roles {
		principal.Roles = append(principal.Roles, authctx.ReferenceName{ID: role.ID, Code: role.Code, Name: role.Name})
		if role.Code == application.BootstrapSuperAdminRoleCode {
			allowed = true
		}
	}
	if !allowed {
		return authctx.Principal{}, application.ErrUnauthenticated
	}
	principal.PermissionCodes, err = repository.findPermissionCodes(ctx, row.TenantID, row.UserID, row.AccountID, now.UTC())
	if err != nil {
		return authctx.Principal{}, err
	}
	return principal, nil
}

func offlineBootstrapProjectionQuery(db *gorm.DB, now time.Time) *gorm.DB {
	return db.Table("iam_bootstrap_state AS bootstrap").
		Select(`tenant.id AS tenant_id, tenant.name AS tenant_name, tenant.code AS tenant_code,
   user.id AS user_id, user.display_name AS user_name, account.id AS account_id, COALESCE(account.username, account.id) AS account_name`).
		Joins("JOIN iam_tenant AS tenant ON tenant.id = bootstrap.tenant_id AND tenant.code = ? AND tenant.status = ?", application.BootstrapTenantCode, domain.StatusActive).
		Joins("JOIN iam_account AS account ON account.id = bootstrap.first_super_admin_account_id AND account.tenant_id = tenant.id AND account.user_id = bootstrap.first_super_admin_user_id AND account.status = ? AND (account.valid_until IS NULL OR account.valid_until > ?) AND (account.locked_until IS NULL OR account.locked_until <= ?)", domain.StatusActive, now.UTC(), now.UTC()).
		Joins("JOIN iam_user AS user ON user.id = account.user_id AND user.tenant_id = tenant.id AND user.deleted_at IS NULL AND user.status = ? AND (user.valid_until IS NULL OR user.valid_until > ?)", domain.StatusActive, now.UTC()).
		Limit(1)
}
