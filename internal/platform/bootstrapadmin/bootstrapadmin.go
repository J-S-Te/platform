// Package bootstrapadmin 标识并保护部署初始化创建的超级管理员账号。
//
// iam_bootstrap_state 中的不可变记录由受控部署流程写入，指向首个超级管理员。当该记录缺失
// 或不一致时，本包绝不回退到用户名、显示名或“其他管理员”：一律以 ErrConfiguration 失败关闭，
// 且不改动既有业务数据。
package bootstrapadmin

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"gorm.io/gorm"
)

const (
	activeStatus            = "ACTIVE"
	bootstrapTenantCode     = "default"
	platformApplicationCode = "platform"
	superAdminRoleCode      = "platform-super-admin"
)

var (
	// ErrConfiguration 表示初始化记录不一致。调用方必须失败关闭，
	// 绝不能用普通管理员顶替受保护的部署初始化管理员。
	ErrConfiguration = errors.New("bootstrap administrator configuration is invalid")
	// ErrProtected 表示该操作会削弱部署初始化管理员，已被保护规则拒绝。
	ErrProtected = errors.New("bootstrap administrator is protected")
)

// Identity 是从 iam_bootstrap_state 解析出的部署初始化管理员标识。
type Identity struct {
	TenantID  string
	UserID    string
	AccountID string
}

// ProtectsUser 判断给定用户 ID 是否就是部署初始化的保护对象。
func (identity Identity) ProtectsUser(userID string) bool {
	userID = strings.TrimSpace(userID)
	return identity.UserID != "" && userID != "" && identity.UserID == userID
}

// ProtectsAccount 判断给定账号 ID 是否就是部署初始化管理员的登录账号。
func (identity Identity) ProtectsAccount(accountID string) bool {
	accountID = strings.TrimSpace(accountID)
	return identity.AccountID != "" && accountID != "" && identity.AccountID == accountID
}

type snapshot struct {
	TenantID                string
	TenantCode              string
	StateExists             bool
	StateTenantID           string
	StateUserID             string
	StateAccountID          string
	UserExists              bool
	UserDeleted             bool
	UserStatus              string
	AccountExists           bool
	AccountUserID           string
	AccountStatus           string
	GrantActive             bool
	SuperAdminBindingExists bool
}

// evaluate 把只读快照转换为受保护身份。它是纯函数，使失败关闭的判定不依赖数据库即可测试。
func evaluate(state snapshot) (Identity, bool, error) {
	if !state.StateExists {
		// default 租户已存在 platform 超级管理员绑定，说明部署确实初始化过，但不可变记录丢失。
		// 此时必须上报异常，而不是挑选一个替补管理员。
		if state.TenantCode == bootstrapTenantCode && state.SuperAdminBindingExists {
			return Identity{}, false, fmt.Errorf("%w: initialization record is missing", ErrConfiguration)
		}
		return Identity{}, false, nil
	}
	if state.StateTenantID != state.TenantID || strings.TrimSpace(state.StateUserID) == "" || strings.TrimSpace(state.StateAccountID) == "" {
		return Identity{}, false, fmt.Errorf("%w: initialization record is incomplete", ErrConfiguration)
	}
	if !state.UserExists || state.UserDeleted || !strings.EqualFold(state.UserStatus, activeStatus) {
		return Identity{}, false, fmt.Errorf("%w: initialized user is missing, deleted or inactive", ErrConfiguration)
	}
	if !state.AccountExists || state.AccountUserID != state.StateUserID || !strings.EqualFold(state.AccountStatus, activeStatus) {
		return Identity{}, false, fmt.Errorf("%w: initialized login account is missing, unlinked or inactive", ErrConfiguration)
	}
	if !state.GrantActive {
		return Identity{}, false, fmt.Errorf("%w: initialized super administrator grant is missing or inactive", ErrConfiguration)
	}
	return Identity{TenantID: state.TenantID, UserID: state.StateUserID, AccountID: state.StateAccountID}, true, nil
}

// Resolve 加载指定租户的部署初始化管理员。受控的首管理员初始化尚未执行时第二个返回值为
// false；记录存在但不一致时返回包装 ErrConfiguration 的错误。
func Resolve(ctx context.Context, database *gorm.DB, tenantID string) (Identity, bool, error) {
	if database == nil {
		return Identity{}, false, fmt.Errorf("%w: database is unavailable", ErrConfiguration)
	}
	tenantID = strings.TrimSpace(tenantID)
	if tenantID == "" {
		return Identity{}, false, fmt.Errorf("%w: tenant is required", ErrConfiguration)
	}
	state, err := readSnapshot(database.WithContext(ctx), tenantID)
	if err != nil {
		return Identity{}, false, err
	}
	return evaluate(state)
}

type stateRow struct {
	TenantID  string `gorm:"column:tenant_id"`
	UserID    string `gorm:"column:first_super_admin_user_id"`
	AccountID string `gorm:"column:first_super_admin_account_id"`
}

type tenantRow struct {
	Code string `gorm:"column:code"`
}

type userRow struct {
	Status    string     `gorm:"column:status"`
	DeletedAt *time.Time `gorm:"column:deleted_at"`
}

type accountRow struct {
	UserID *string `gorm:"column:user_id"`
	Status string  `gorm:"column:status"`
}

func readSnapshot(database *gorm.DB, tenantID string) (snapshot, error) {
	state := snapshot{TenantID: tenantID}

	var recorded stateRow
	err := database.Table("iam_bootstrap_state").
		Select("tenant_id, first_super_admin_user_id, first_super_admin_account_id").
		Where("tenant_id = ?", tenantID).
		Take(&recorded).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		var tenant tenantRow
		result := database.Table("iam_tenant").Select("code").Where("id = ?", tenantID).Take(&tenant)
		if errors.Is(result.Error, gorm.ErrRecordNotFound) {
			return snapshot{}, fmt.Errorf("%w: tenant record is missing", ErrConfiguration)
		}
		if result.Error != nil {
			return snapshot{}, fmt.Errorf("read bootstrap tenant: %w", result.Error)
		}
		state.TenantCode = strings.TrimSpace(tenant.Code)
		if state.TenantCode == bootstrapTenantCode {
			exists, bindErr := superAdminBindingExists(database, tenantID, "", false, time.Time{})
			if bindErr != nil {
				return snapshot{}, bindErr
			}
			state.SuperAdminBindingExists = exists
		}
		return state, nil
	}
	if err != nil {
		return snapshot{}, fmt.Errorf("read bootstrap administrator state: %w", err)
	}

	state.StateExists = true
	state.StateTenantID = strings.TrimSpace(recorded.TenantID)
	state.StateUserID = strings.TrimSpace(recorded.UserID)
	state.StateAccountID = strings.TrimSpace(recorded.AccountID)
	if state.StateUserID == "" || state.StateAccountID == "" {
		return state, nil
	}

	var user userRow
	result := database.Table("iam_user").
		Select("status, deleted_at").
		Where("tenant_id = ? AND id = ?", tenantID, state.StateUserID).
		Take(&user)
	switch {
	case errors.Is(result.Error, gorm.ErrRecordNotFound):
	case result.Error != nil:
		return snapshot{}, fmt.Errorf("read bootstrap administrator user: %w", result.Error)
	default:
		state.UserExists = true
		state.UserStatus = strings.TrimSpace(user.Status)
		state.UserDeleted = user.DeletedAt != nil
	}

	var account accountRow
	result = database.Table("iam_account").
		Select("user_id, status").
		Where("tenant_id = ? AND id = ?", tenantID, state.StateAccountID).
		Take(&account)
	switch {
	case errors.Is(result.Error, gorm.ErrRecordNotFound):
	case result.Error != nil:
		return snapshot{}, fmt.Errorf("read bootstrap administrator account: %w", result.Error)
	default:
		state.AccountExists = true
		state.AccountStatus = strings.TrimSpace(account.Status)
		if account.UserID != nil {
			state.AccountUserID = strings.TrimSpace(*account.UserID)
		}
	}

	active, err := superAdminBindingExists(database, tenantID, state.StateUserID, true, time.Now().UTC())
	if err != nil {
		return snapshot{}, err
	}
	state.GrantActive = active
	state.SuperAdminBindingExists = active
	return state, nil
}

// superAdminBindingExists 统计内建 platform 超级管理员角色上的 USER 绑定。
// activeOnly 为 true 时还要求绑定是无租户范围限制、状态 ACTIVE 且当前时间在有效期内。
func superAdminBindingExists(database *gorm.DB, tenantID, userID string, activeOnly bool, now time.Time) (bool, error) {
	query := database.Table("authz_role_binding AS binding").
		Joins("JOIN authz_role AS role ON role.id = binding.role_id AND role.tenant_id = binding.tenant_id AND role.application_id = binding.application_id").
		Joins("JOIN platform_application AS application ON application.id = binding.application_id AND application.tenant_id = binding.tenant_id").
		Where("binding.tenant_id = ? AND binding.subject_type = ?", tenantID, "USER").
		Where("application.code = ? AND role.code = ?", platformApplicationCode, superAdminRoleCode)
	if userID != "" {
		query = query.Where("binding.subject_id = ?", userID)
	}
	if activeOnly {
		query = query.
			Where("binding.status = ? AND role.status = ? AND application.status = ?", activeStatus, activeStatus, activeStatus).
			Where("binding.scope_type = ? AND binding.scope_id = ?", "TENANT", "").
			Where("(binding.valid_from IS NULL OR binding.valid_from <= ?)", now).
			Where("(binding.valid_until IS NULL OR binding.valid_until > ?)", now)
	}
	var count int64
	if err := query.Count(&count).Error; err != nil {
		return false, fmt.Errorf("read bootstrap administrator grant: %w", err)
	}
	return count > 0, nil
}

// UserUpdateViolation 判断一次管理端更新是否会禁用或提前终止受保护用户，命中则必须拒绝。
func UserUpdateViolation(identity Identity, userID string, status *string, updateValidity bool, validUntil *time.Time) bool {
	if !identity.ProtectsUser(userID) {
		return false
	}
	if status != nil && !strings.EqualFold(strings.TrimSpace(*status), activeStatus) {
		return true
	}
	return updateValidity && validUntil != nil
}

// AccountUpdateViolation 判断一次管理端更新是否会禁用或提前终止受保护账号，命中则必须拒绝。
func AccountUpdateViolation(identity Identity, accountID, status string, updateValidity bool, validUntil *time.Time) bool {
	if !identity.ProtectsAccount(accountID) {
		return false
	}
	if !strings.EqualFold(strings.TrimSpace(status), activeStatus) {
		return true
	}
	return updateValidity && validUntil != nil
}
