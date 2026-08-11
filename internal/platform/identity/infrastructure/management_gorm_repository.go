package infrastructure

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/J-S-Te/Basic-Platform/internal/platform/identity/application"
	"github.com/J-S-Te/Basic-Platform/internal/platform/identity/domain"
	"github.com/J-S-Te/Basic-Platform/internal/shared/ulid"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// ListUsers reads tenant-scoped users and leaves mobile masking to the application layer.
func (repository *GORMRepository) ListUsers(ctx context.Context, tenantID string, query application.PageRequest) (application.PageResult[domain.User], error) {
	// IAM 用户就是任职关系中的人员主档案；没有任何任职记录的历史/外部孤儿用户
	// 不进入用户模块，避免用户列表与人员列表出现两套事实来源。
	database := applyUserFilter(repository.database.WithContext(ctx).Model(&userModel{}).
		Where("EXISTS (SELECT 1 FROM iam_membership AS membership WHERE membership.tenant_id = iam_user.tenant_id AND membership.user_id = iam_user.id)"), tenantID, query)
	var total int64
	if err := database.Count(&total).Error; err != nil {
		return application.PageResult[domain.User]{}, fmt.Errorf("count users: %w", err)
	}

	var rows []userModel
	if err := database.Order("created_at DESC, id DESC").Limit(query.PageSize).Offset(pageOffset(query)).Find(&rows).Error; err != nil {
		return application.PageResult[domain.User]{}, fmt.Errorf("list users: %w", err)
	}
	items := make([]domain.User, 0, len(rows))
	for _, row := range rows {
		items = append(items, toDomainUser(row))
	}
	return application.PageResult[domain.User]{Items: items, Page: query.Page, PageSize: query.PageSize, Total: total}, nil
}

// CreateUser preserves the single-user repository contract while delegating to the same atomic
// user-and-role-binding transaction used by batch creation.
func (repository *GORMRepository) CreateUser(ctx context.Context, write application.UserWrite) (domain.User, error) {
	users, err := repository.CreateUsers(ctx, []application.UserWrite{write})
	if err != nil {
		return domain.User{}, err
	}
	return users[0], nil
}

// CreateUsers persists users, ordinary-user role bindings, and the authorization policy revision
// in one transaction. A missing platform application or baseline role is treated as deployment
// seed corruption instead of silently creating users without their required role.
func (repository *GORMRepository) CreateUsers(ctx context.Context, writes []application.UserWrite) ([]domain.User, error) {
	if len(writes) == 0 {
		return nil, application.ErrValidation
	}

	users := make([]domain.User, 0, len(writes))
	err := repository.database.WithContext(ctx).Transaction(func(transaction *gorm.DB) error {
		// 整批用户必须属于同一租户；应用角色先整体解析再开始写入，任何歧义、越界或
		// 角色数量冲突都会使整批失败，避免 CSV 导入留下半批成功数据。
		tenantID := writes[0].TenantID
		var platformApplication bootstrapApplicationModel
		result := transaction.Where(
			"tenant_id = ? AND code = ? AND status = ?",
			tenantID, application.DefaultPlatformApplicationCode, domain.StatusActive,
		).First(&platformApplication)
		if errors.Is(result.Error, gorm.ErrRecordNotFound) {
			return fmt.Errorf("resolve platform application: %w", application.ErrNotFound)
		}
		if result.Error != nil {
			return fmt.Errorf("resolve platform application: %w", result.Error)
		}

		var ordinaryUserRole bootstrapRoleModel
		result = transaction.Where(
			"tenant_id = ? AND application_id = ? AND code = ? AND status = ?",
			tenantID, platformApplication.ID, application.DefaultUserRoleCode, domain.StatusActive,
		).First(&ordinaryUserRole)
		if errors.Is(result.Error, gorm.ErrRecordNotFound) {
			return fmt.Errorf("resolve ordinary-user role: %w", application.ErrNotFound)
		}
		if result.Error != nil {
			return fmt.Errorf("resolve ordinary-user role: %w", result.Error)
		}

		resolvedApplicationRoles, err := resolveImportedApplicationRoles(transaction, tenantID, writes)
		if err != nil {
			return err
		}

		now := time.Now().UTC()
		changedApplications := map[string]struct{}{platformApplication.ID: {}}
		for _, write := range writes {
			if write.TenantID != tenantID || write.ID == "" || write.RoleBindingID == "" || write.OperatorID == "" {
				return application.ErrValidation
			}
			row := userModel{
				ID: write.ID, TenantID: write.TenantID, EmployeeNo: nullableString(write.EmployeeNo),
				DisplayName: write.DisplayName, Email: nullableString(write.Email),
				MobileCiphertext: nullableBytes(write.MobileCiphertext), MobileHash: nullableBytes(write.MobileHash),
				EmploymentStatus: "EMPLOYED", Status: write.Status, Version: 1,
				CreatedAt: now, CreatedBy: nullableString(&write.OperatorID), UpdatedAt: now, UpdatedBy: nullableString(&write.OperatorID),
			}
			if err := transaction.Create(&row).Error; err != nil {
				return mapWriteError(err, "create user")
			}
			binding := bootstrapRoleBindingModel{
				ID: write.RoleBindingID, TenantID: tenantID, ApplicationID: platformApplication.ID,
				RoleID: ordinaryUserRole.ID, SubjectType: "USER", SubjectID: write.ID,
				ScopeType: "TENANT", ScopeID: "", Status: domain.StatusActive, GrantOrigin: "SYSTEM", Version: 1,
				CreatedAt: now, CreatedBy: nullableString(&write.OperatorID), UpdatedAt: now, UpdatedBy: nullableString(&write.OperatorID),
			}
			if err := transaction.Create(&binding).Error; err != nil {
				return mapWriteError(err, "bind ordinary-user role")
			}
			for _, imported := range write.ApplicationRoleBindings {
				resolved, ok := resolvedApplicationRoles[importedApplicationRoleKey(imported)]
				if !ok {
					return application.ErrValidation
				}
				applicationBinding := bootstrapRoleBindingModel{
					ID: imported.ID, TenantID: tenantID, ApplicationID: resolved.ApplicationID,
					RoleID: resolved.RoleID, SubjectType: "USER", SubjectID: write.ID,
					ScopeType: "TENANT", ScopeID: "", Status: domain.StatusActive,
					GrantOrigin: "MANUAL", Version: 1,
					CreatedAt: now, CreatedBy: nullableString(&write.OperatorID), UpdatedAt: now, UpdatedBy: nullableString(&write.OperatorID),
				}
				if err := transaction.Create(&applicationBinding).Error; err != nil {
					return mapWriteError(err, "bind imported application role")
				}
				changedApplications[resolved.ApplicationID] = struct{}{}
			}
			users = append(users, toDomainUser(row))
		}

		for applicationID := range changedApplications {
			reason := "批量导入用户并绑定角色"
			if applicationID == platformApplication.ID {
				reason = "自动绑定新建用户的普通用户角色"
			}
			result = transaction.Model(&identityPolicyRevisionModel{}).
				Where("tenant_id = ? AND application_id = ?", tenantID, applicationID).
				Updates(map[string]any{"revision": gorm.Expr("revision + 1"), "changed_at": now, "change_reason": reason})
			if result.Error != nil {
				return fmt.Errorf("advance authorization policy revision: %w", result.Error)
			}
			if result.RowsAffected == 0 {
				return fmt.Errorf("advance authorization policy revision: %w", application.ErrNotFound)
			}
		}
		identityIDs := make([]string, 0, len(writes))
		for _, write := range writes {
			identityIDs = append(identityIDs, write.ID)
		}
		if err := enqueueKeycloakIdentityEvents(transaction, tenantID, identityIDs, now, "IDENTITY_CHANGED"); err != nil {
			return err
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return users, nil
}

type importedApplicationRole struct {
	ApplicationID string
	RoleID        string
}

// resolveImportedApplicationRoles validates all CSV roles before the first user is written. It
// accepts either stable codes (backwards compatibility) or exact human-readable names. Ambiguous
// names fail closed. Only ACTIVE application-owned catalog roles are assignable.
func resolveImportedApplicationRoles(transaction *gorm.DB, tenantID string, writes []application.UserWrite) (map[string]importedApplicationRole, error) {
	requested := make(map[string]application.ApplicationRoleBindingWrite)
	for _, write := range writes {
		for _, role := range write.ApplicationRoleBindings {
			key := importedApplicationRoleKey(role)
			requested[key] = role
		}
	}
	if len(requested) == 0 {
		return map[string]importedApplicationRole{}, nil
	}

	applicationCodes := make([]string, 0, len(requested))
	applicationNames := make([]string, 0, len(requested))
	seenApplicationCodes := make(map[string]struct{})
	seenApplicationNames := make(map[string]struct{})
	for _, request := range requested {
		if request.ApplicationCode != "" {
			if _, exists := seenApplicationCodes[request.ApplicationCode]; exists {
				continue
			}
			seenApplicationCodes[request.ApplicationCode] = struct{}{}
			applicationCodes = append(applicationCodes, request.ApplicationCode)
			continue
		}
		if _, exists := seenApplicationNames[request.ApplicationName]; !exists {
			seenApplicationNames[request.ApplicationName] = struct{}{}
			applicationNames = append(applicationNames, request.ApplicationName)
		}
	}
	query := transaction.Where("tenant_id = ? AND status = ?", tenantID, domain.StatusActive)
	if len(applicationCodes) > 0 && len(applicationNames) > 0 {
		query = query.Where("(code IN ? OR name IN ?)", applicationCodes, applicationNames)
	} else if len(applicationCodes) > 0 {
		query = query.Where("code IN ?", applicationCodes)
	} else {
		query = query.Where("name IN ?", applicationNames)
	}
	var applications []bootstrapApplicationModel
	if err := query.Find(&applications).Error; err != nil {
		return nil, fmt.Errorf("resolve imported role applications: %w", err)
	}
	applicationsByCode := make(map[string]bootstrapApplicationModel, len(applications))
	applicationsByName := make(map[string][]bootstrapApplicationModel, len(applications))
	for _, item := range applications {
		applicationsByCode[item.Code] = item
		applicationsByName[item.Name] = append(applicationsByName[item.Name], item)
	}

	resolved := make(map[string]importedApplicationRole, len(requested))
	for key, request := range requested {
		var app bootstrapApplicationModel
		if request.ApplicationCode != "" {
			var exists bool
			app, exists = applicationsByCode[request.ApplicationCode]
			if !exists {
				return nil, application.ErrValidation
			}
		} else {
			matches := applicationsByName[request.ApplicationName]
			if len(matches) != 1 {
				return nil, application.ErrValidation
			}
			app = matches[0]
		}

		roleQuery := transaction.Where(
			"tenant_id = ? AND application_id = ? AND role_type = ? AND status = ?",
			tenantID, app.ID, "APPLICATION", domain.StatusActive,
		)
		if request.RoleCode != "" {
			roleQuery = roleQuery.Where("code = ?", request.RoleCode)
		} else {
			roleQuery = roleQuery.Where("name = ?", request.RoleName)
		}
		var roles []bootstrapRoleModel
		if err := roleQuery.Limit(2).Find(&roles).Error; err != nil {
			return nil, fmt.Errorf("resolve imported application role: %w", err)
		}
		if len(roles) != 1 {
			return nil, application.ErrValidation
		}
		resolved[key] = importedApplicationRole{ApplicationID: app.ID, RoleID: roles[0].ID}
	}

	for _, write := range writes {
		rolesByApplication := make(map[string]map[string]struct{})
		for _, request := range write.ApplicationRoleBindings {
			role := resolved[importedApplicationRoleKey(request)]
			if rolesByApplication[role.ApplicationID] == nil {
				rolesByApplication[role.ApplicationID] = make(map[string]struct{})
			}
			if _, duplicate := rolesByApplication[role.ApplicationID][role.RoleID]; duplicate {
				return nil, application.ErrValidation
			}
			rolesByApplication[role.ApplicationID][role.RoleID] = struct{}{}
		}
		for applicationID, roleIDs := range rolesByApplication {
			var policy struct {
				MaxEffectiveRoles int `gorm:"column:max_effective_roles"`
			}
			err := transaction.Table("authz_application_authorization_policy").Select("max_effective_roles").Where("tenant_id = ? AND application_id = ?", tenantID, applicationID).Take(&policy).Error
			if err != nil && !errors.Is(err, gorm.ErrRecordNotFound) {
				return nil, fmt.Errorf("load imported role application policy: %w", err)
			}
			if policy.MaxEffectiveRoles > 0 && len(roleIDs) > policy.MaxEffectiveRoles {
				return nil, application.ErrValidation
			}
		}
	}
	return resolved, nil
}

func importedApplicationRoleKey(role application.ApplicationRoleBindingWrite) string {
	return role.ApplicationCode + "\x00" + role.ApplicationName + "\x00" + role.RoleCode + "\x00" + role.RoleName
}

// GetUser retrieves exactly one tenant-scoped user.
func (repository *GORMRepository) GetUser(ctx context.Context, tenantID, userID string) (domain.User, error) {
	var row userModel
	result := repository.database.WithContext(ctx).Where("tenant_id = ? AND id = ? AND deleted_at IS NULL", tenantID, userID).First(&row)
	if errors.Is(result.Error, gorm.ErrRecordNotFound) {
		return domain.User{}, application.ErrNotFound
	}
	if result.Error != nil {
		return domain.User{}, fmt.Errorf("get user: %w", result.Error)
	}
	return toDomainUser(row), nil
}

// UpdateUser applies an optimistic-lock update with an explicit persistence-field whitelist.
func (repository *GORMRepository) UpdateUser(ctx context.Context, input application.UserUpdateInput, mobileCiphertext, mobileHash []byte) (domain.User, error) {
	err := repository.database.WithContext(ctx).Transaction(func(transaction *gorm.DB) error {
		var existing userModel
		result := transaction.Clauses(clause.Locking{Strength: "UPDATE"}).
			Where("tenant_id = ? AND id = ? AND deleted_at IS NULL", input.TenantID, input.UserID).First(&existing)
		if errors.Is(result.Error, gorm.ErrRecordNotFound) {
			return application.ErrNotFound
		}
		if result.Error != nil {
			return fmt.Errorf("lock user for update: %w", result.Error)
		}
		if existing.Version != input.Version {
			return application.ErrVersionConflict
		}
		now := time.Now().UTC()
		updates := map[string]any{
			"display_name": input.DisplayName,
			"updated_at":   now,
			"updated_by":   input.OperatorID,
			"version":      gorm.Expr("version + 1"),
		}
		if input.EmployeeNo != nil {
			updates["employee_no"] = nullableString(input.EmployeeNo)
		}
		if input.PMSPersonID != nil {
			updates["pms_person_id"] = nullableString(input.PMSPersonID)
		}
		if input.Email != nil {
			updates["email"] = nullableString(input.Email)
		}
		if input.Status != nil {
			updates["status"] = *input.Status
		}
		if input.UpdateMobile {
			updates["mobile_ciphertext"] = nullableBytes(mobileCiphertext)
			updates["mobile_hash"] = nullableBytes(mobileHash)
		}
		result = transaction.Model(&userModel{}).Where("tenant_id = ? AND id = ? AND version = ?", input.TenantID, input.UserID, input.Version).Updates(updates)
		if result.Error != nil {
			return mapWriteError(result.Error, "update user")
		}
		if result.RowsAffected != 1 {
			return application.ErrVersionConflict
		}
		if input.Status != nil && *input.Status == domain.StatusDisabled && existing.Status != domain.StatusDisabled {
			accountIDs := transaction.Model(&accountModel{}).Select("id").Where("tenant_id = ? AND user_id = ?", input.TenantID, input.UserID)
			if err := transaction.Model(&accountModel{}).Where("tenant_id = ? AND user_id = ?", input.TenantID, input.UserID).
				Updates(map[string]any{"username": nil, "status": domain.StatusDisabled, "updated_at": now, "updated_by": input.OperatorID, "version": gorm.Expr("version + 1")}).Error; err != nil {
				return mapWriteError(err, "disable user accounts")
			}
			if err := transaction.Model(&passwordCredentialModel{}).Where("account_id IN (?) AND status <> ?", accountIDs, domain.StatusDisabled).
				Updates(map[string]any{"status": domain.StatusDisabled, "updated_at": now}).Error; err != nil {
				return fmt.Errorf("disable user password credentials: %w", err)
			}
			if err := transaction.Model(&membershipModel{}).Where("tenant_id = ? AND user_id = ? AND status <> ?", input.TenantID, input.UserID, domain.StatusDisabled).
				Updates(map[string]any{"status": domain.StatusDisabled, "is_primary": false, "updated_at": now, "updated_by": input.OperatorID, "version": gorm.Expr("version + 1")}).Error; err != nil {
				return mapWriteError(err, "disable user memberships")
			}
			if err := transaction.Model(&userModel{}).Where("tenant_id = ? AND id = ?", input.TenantID, input.UserID).
				Updates(map[string]any{"primary_org_id": nil, "updated_at": now, "updated_by": input.OperatorID, "version": gorm.Expr("version + 1")}).Error; err != nil {
				return mapWriteError(err, "clear user primary organization")
			}
			if err := transaction.Model(&sessionModel{}).Where("tenant_id = ? AND account_id IN (?) AND status = ? AND revoked_at IS NULL", input.TenantID, accountIDs, domain.StatusActive).
				Updates(map[string]any{"status": domain.StatusDisabled, "revoked_at": now, "revoke_reason": "USER_DISABLED"}).Error; err != nil {
				return fmt.Errorf("revoke disabled user sessions: %w", err)
			}
		}
		if err := enqueueKeycloakIdentityEvents(transaction, input.TenantID, []string{input.UserID}, now, "IDENTITY_CHANGED"); err != nil {
			return err
		}
		return nil
	})
	if err != nil {
		return domain.User{}, err
	}
	return repository.GetUser(ctx, input.TenantID, input.UserID)
}

// DeleteUser performs a tenant-scoped business deletion. The user, account and membership rows are
// retained as disabled records for audit and foreign-key integrity, but associated login accounts
// and appointments are removed from normal management views. Existing sessions are revoked in the
// same transaction so the deleted user loses access immediately.
func (repository *GORMRepository) DeleteUser(ctx context.Context, input application.UserDeleteInput) error {
	return repository.database.WithContext(ctx).Transaction(func(transaction *gorm.DB) error {
		var user userModel
		result := transaction.Clauses(clause.Locking{Strength: "UPDATE"}).
			Where("tenant_id = ? AND id = ?", input.TenantID, input.UserID).First(&user)
		if errors.Is(result.Error, gorm.ErrRecordNotFound) {
			return application.ErrNotFound
		}
		if result.Error != nil {
			return fmt.Errorf("lock user for deletion: %w", result.Error)
		}
		if user.Version != input.Version {
			return application.ErrVersionConflict
		}
		if user.Status != domain.StatusActive {
			return application.ErrConflict
		}

		now := time.Now().UTC()
		if err := transaction.Model(&userModel{}).Where("tenant_id = ? AND id = ? AND version = ?", input.TenantID, input.UserID, input.Version).
			Updates(map[string]any{
				"status": domain.StatusDisabled, "primary_org_id": nil, "deleted_at": now, "deleted_by": input.OperatorID, "updated_at": now, "updated_by": input.OperatorID, "version": gorm.Expr("version + 1"),
			}).Error; err != nil {
			return mapWriteError(err, "delete user")
		}

		accountIDs := transaction.Model(&accountModel{}).Select("id").
			Where("tenant_id = ? AND user_id = ?", input.TenantID, input.UserID)
		if err := transaction.Model(&accountModel{}).
			Where("tenant_id = ? AND user_id = ?", input.TenantID, input.UserID).
			Updates(map[string]any{"username": nil, "status": domain.StatusDisabled, "updated_at": now, "updated_by": input.OperatorID, "version": gorm.Expr("version + 1")}).Error; err != nil {
			return mapWriteError(err, "disable user accounts")
		}
		if err := transaction.Model(&passwordCredentialModel{}).
			Where("account_id IN (?) AND status <> ?", accountIDs, domain.StatusDisabled).
			Updates(map[string]any{"status": domain.StatusDisabled, "updated_at": now}).Error; err != nil {
			return fmt.Errorf("disable user password credentials: %w", err)
		}
		if err := transaction.Model(&membershipModel{}).
			Where("tenant_id = ? AND user_id = ? AND status <> ?", input.TenantID, input.UserID, domain.StatusDisabled).
			Updates(map[string]any{"status": domain.StatusDisabled, "is_primary": false, "updated_at": now, "updated_by": input.OperatorID, "version": gorm.Expr("version + 1")}).Error; err != nil {
			return mapWriteError(err, "disable user memberships")
		}
		reason := "USER_DELETED"
		if err := transaction.Model(&sessionModel{}).
			Where("tenant_id = ? AND account_id IN (?) AND status = ? AND revoked_at IS NULL", input.TenantID, accountIDs, domain.StatusActive).
			Updates(map[string]any{"status": domain.StatusDisabled, "revoked_at": now, "revoke_reason": reason}).Error; err != nil {
			return fmt.Errorf("revoke user sessions: %w", err)
		}
		if err := enqueueKeycloakIdentityEvents(transaction, input.TenantID, []string{input.UserID}, now, "IDENTITY_CHANGED"); err != nil {
			return err
		}
		return nil
	})
}

func (repository *GORMRepository) versionedUserError(ctx context.Context, tenantID, userID string) error {
	var total int64
	if err := repository.database.WithContext(ctx).Model(&userModel{}).Where("tenant_id = ? AND id = ? AND deleted_at IS NULL", tenantID, userID).Count(&total).Error; err != nil {
		return fmt.Errorf("check user after update: %w", err)
	}
	if total == 0 {
		return application.ErrNotFound
	}
	return application.ErrVersionConflict
}

func (repository *GORMRepository) ListAccounts(ctx context.Context, tenantID string, query application.PageRequest) (application.PageResult[domain.Account], error) {
	database := applyAccountFilter(repository.database.WithContext(ctx).Model(&accountModel{}), tenantID, query)
	var total int64
	if err := database.Count(&total).Error; err != nil {
		return application.PageResult[domain.Account]{}, fmt.Errorf("count accounts: %w", err)
	}

	var rows []accountModel
	if err := database.Select(`iam_account.*, EXISTS (
		SELECT 1 FROM iam_password_credential AS credential
		WHERE credential.account_id = iam_account.id AND credential.status = 'ACTIVE'
	) AS password_initialized`).Order("created_at DESC, id DESC").Limit(query.PageSize).Offset(pageOffset(query)).Find(&rows).Error; err != nil {
		return application.PageResult[domain.Account]{}, fmt.Errorf("list accounts: %w", err)
	}
	items := make([]domain.Account, 0, len(rows))
	linkedUserIDs := make([]string, 0, len(rows))
	for _, row := range rows {
		items = append(items, toAccountWithFallbackName(row))
		if row.UserID != nil && strings.TrimSpace(*row.UserID) != "" {
			linkedUserIDs = append(linkedUserIDs, strings.TrimSpace(*row.UserID))
		}
	}
	if len(linkedUserIDs) > 0 {
		var linkedUsers []userModel
		if err := repository.database.WithContext(ctx).
			Select("id", "display_name").
			Where("tenant_id = ? AND id IN ? AND deleted_at IS NULL", tenantID, linkedUserIDs).
			Find(&linkedUsers).Error; err != nil {
			return application.PageResult[domain.Account]{}, fmt.Errorf("list account users: %w", err)
		}
		userNames := make(map[string]string, len(linkedUsers))
		for _, user := range linkedUsers {
			userNames[user.ID] = user.DisplayName
		}
		for index := range items {
			if items[index].UserID == nil {
				continue
			}
			userID := strings.TrimSpace(*items[index].UserID)
			if displayName, ok := userNames[userID]; ok {
				items[index].User = &domain.ReferenceName{ID: userID, Name: displayName}
			}
		}
	}
	return application.PageResult[domain.Account]{Items: items, Page: query.Page, PageSize: query.PageSize, Total: total}, nil
}

// UpdateAccount changes an account status with its session invalidation in the same transaction.
//
// A disabled account must never regain access by simply being re-enabled while an old session is
// still inside its expiry window. The row lock makes the status transition and revocation atomic:
// if either write fails, neither becomes visible.
func (repository *GORMRepository) UpdateAccount(ctx context.Context, input application.AccountUpdateInput) (domain.Account, error) {
	ownerClause, ownerArgs := accountOwnerVisibilityFilter()
	err := repository.database.WithContext(ctx).Transaction(func(transaction *gorm.DB) error {
		var existing accountModel
		result := transaction.Clauses(clause.Locking{Strength: "UPDATE"}).
			Where("tenant_id = ? AND id = ? AND version = ?", input.TenantID, input.AccountID, input.Version).
			Where(ownerClause, ownerArgs...).
			First(&existing)
		if errors.Is(result.Error, gorm.ErrRecordNotFound) {
			return repository.versionedAccountError(ctx, input.TenantID, input.AccountID)
		}
		if result.Error != nil {
			return fmt.Errorf("lock account before update: %w", result.Error)
		}

		now := time.Now().UTC()
		result = transaction.Model(&accountModel{}).
			Where("tenant_id = ? AND id = ? AND version = ?", input.TenantID, input.AccountID, input.Version).
			Updates(map[string]any{
				"status":     input.Status,
				"updated_at": now,
				"updated_by": input.OperatorID,
				"version":    gorm.Expr("version + 1"),
			})
		if result.Error != nil {
			return mapWriteError(result.Error, "update account")
		}
		if result.RowsAffected != 1 {
			return application.ErrVersionConflict
		}

		// 重新启用账号不是恢复会话：从 ACTIVE 进入任一非活动状态时撤销旧会话，之后
		// 即使重新启用也必须重新认证。这里保留泛化的非 ACTIVE 判断，供后续锁定生命周期复用。
		if existing.Status == domain.StatusActive && input.Status != domain.StatusActive {
			result = transaction.Model(&sessionModel{}).
				Where("tenant_id = ? AND account_id = ? AND status = ? AND revoked_at IS NULL", input.TenantID, input.AccountID, domain.StatusActive).
				Updates(map[string]any{"status": "REVOKED", "revoked_at": now, "revoke_reason": "ACCOUNT_STATUS_CHANGED"})
			if result.Error != nil {
				return fmt.Errorf("revoke account sessions after status change: %w", result.Error)
			}
		}
		if existing.UserID != nil {
			if err := enqueueKeycloakIdentityEvents(transaction, input.TenantID, []string{*existing.UserID}, now, "IDENTITY_CHANGED"); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return domain.Account{}, err
	}
	return repository.getAccount(ctx, input.TenantID, input.AccountID)
}

func (repository *GORMRepository) versionedAccountError(ctx context.Context, tenantID, accountID string) error {
	var total int64
	ownerClause, ownerArgs := accountOwnerVisibilityFilter()
	if err := repository.database.WithContext(ctx).Model(&accountModel{}).
		Where("tenant_id = ? AND id = ?", tenantID, accountID).
		Where(ownerClause, ownerArgs...).
		Count(&total).Error; err != nil {
		return fmt.Errorf("check account after update: %w", err)
	}
	if total == 0 {
		return application.ErrNotFound
	}
	return application.ErrVersionConflict
}

func (repository *GORMRepository) getAccount(ctx context.Context, tenantID, accountID string) (domain.Account, error) {
	var row accountModel
	ownerClause, ownerArgs := accountOwnerVisibilityFilter()
	result := repository.database.WithContext(ctx).
		Select(`iam_account.*, EXISTS (
			SELECT 1 FROM iam_password_credential AS credential
			WHERE credential.account_id = iam_account.id AND credential.status = 'ACTIVE'
		) AS password_initialized`).
		Where("tenant_id = ? AND id = ?", tenantID, accountID).
		Where(ownerClause, ownerArgs...).
		First(&row)
	if errors.Is(result.Error, gorm.ErrRecordNotFound) {
		return domain.Account{}, application.ErrNotFound
	}
	if result.Error != nil {
		return domain.Account{}, fmt.Errorf("get account: %w", result.Error)
	}
	return toAccountWithFallbackName(row), nil
}

func (repository *GORMRepository) ListOrgUnits(ctx context.Context, tenantID, keyword, status string, query application.PageRequest) (application.PageResult[domain.OrgUnit], error) {
	database := applyOrganizationFilter(repository.database.WithContext(ctx).Model(&orgUnitModel{}), tenantID, keyword, status)
	database = applyManagementAuthorizationScope(database, query, "id", "id")
	var total int64
	if err := database.Count(&total).Error; err != nil {
		return application.PageResult[domain.OrgUnit]{}, fmt.Errorf("count organization units: %w", err)
	}

	var rows []orgUnitModel
	if err := database.Order("path, sort_order, code").Limit(query.PageSize).Offset(pageOffset(query)).Find(&rows).Error; err != nil {
		return application.PageResult[domain.OrgUnit]{}, fmt.Errorf("list organization units: %w", err)
	}
	items := make([]domain.OrgUnit, 0, len(rows))
	for _, row := range rows {
		items = append(items, toDomainOrgUnit(row))
	}
	return application.PageResult[domain.OrgUnit]{Items: items, Page: query.Page, PageSize: query.PageSize, Total: total}, nil
}

func (repository *GORMRepository) CreateOrgUnit(ctx context.Context, orgUnit domain.OrgUnit, operatorID string) (domain.OrgUnit, error) {
	err := repository.database.WithContext(ctx).Transaction(func(transaction *gorm.DB) error {
		// path 采用包含自身 ID 的物化路径，既支持稳定的子树前缀查询，也使移动节点时能
		// 一次批量替换所有后代路径；父节点必须来自同一租户且处于活动状态。
		path, depth := "/"+orgUnit.ID+"/", uint(1)
		if orgUnit.ParentID != nil {
			var parent orgUnitModel
			result := transaction.Where("tenant_id = ? AND id = ? AND status = ?", orgUnit.TenantID, *orgUnit.ParentID, domain.StatusActive).First(&parent)
			if errors.Is(result.Error, gorm.ErrRecordNotFound) {
				return application.ErrNotFound
			}
			if result.Error != nil {
				return fmt.Errorf("get organization parent: %w", result.Error)
			}
			path, depth = parent.Path+orgUnit.ID+"/", parent.Depth+1
		}
		orgUnit.Path, orgUnit.Depth = path, depth
		now := time.Now().UTC()
		if err := transaction.Create(&orgUnitModel{
			ID:        orgUnit.ID,
			TenantID:  orgUnit.TenantID,
			ParentID:  nullableString(orgUnit.ParentID),
			Code:      orgUnit.Code,
			Name:      orgUnit.Name,
			OrgType:   orgUnit.OrgType,
			Path:      orgUnit.Path,
			Depth:     orgUnit.Depth,
			SortOrder: orgUnit.SortOrder,
			Status:    orgUnit.Status,
			Version:   1,
			CreatedAt: now,
			CreatedBy: nullableString(&operatorID),
			UpdatedAt: now,
			UpdatedBy: nullableString(&operatorID),
		}).Error; err != nil {
			return mapWriteError(err, "create organization unit")
		}
		return nil
	})
	if err != nil {
		return domain.OrgUnit{}, err
	}
	return orgUnit, nil
}

// UpdateOrgUnit 移动组织节点时，在同一事务内重写全部后代的物化路径和深度；新父节点
// 不能是自身或自身后代，避免形成环并破坏后续组织范围授权的子树展开。
func (repository *GORMRepository) UpdateOrgUnit(ctx context.Context, input application.OrgUnitUpdateInput) (domain.OrgUnit, error) {
	var updated domain.OrgUnit
	err := repository.database.WithContext(ctx).Transaction(func(transaction *gorm.DB) error {
		var existing orgUnitModel
		result := transaction.Clauses(clause.Locking{Strength: "UPDATE"}).
			Where("tenant_id = ? AND id = ?", input.TenantID, input.OrgUnitID).
			First(&existing)
		if errors.Is(result.Error, gorm.ErrRecordNotFound) {
			return application.ErrNotFound
		}
		if result.Error != nil {
			return fmt.Errorf("lock organization unit for update: %w", result.Error)
		}
		if existing.Version != input.Version {
			return application.ErrVersionConflict
		}
		if existing.Status != domain.StatusActive {
			return application.ErrConflict
		}

		newPath, newDepth := "/"+existing.ID+"/", uint(1)
		if input.ParentID != nil {
			if *input.ParentID == existing.ID {
				return application.ErrConflict
			}
			var parent orgUnitModel
			result = transaction.Clauses(clause.Locking{Strength: "UPDATE"}).
				Where("tenant_id = ? AND id = ? AND status = ?", input.TenantID, *input.ParentID, domain.StatusActive).
				First(&parent)
			if errors.Is(result.Error, gorm.ErrRecordNotFound) {
				return application.ErrNotFound
			}
			if result.Error != nil {
				return fmt.Errorf("lock organization parent for update: %w", result.Error)
			}
			if strings.HasPrefix(parent.Path, existing.Path) {
				return application.ErrConflict
			}
			newPath, newDepth = parent.Path+existing.ID+"/", parent.Depth+1
		}

		now := time.Now().UTC()
		result = transaction.Model(&orgUnitModel{}).
			Where("tenant_id = ? AND id = ? AND version = ?", input.TenantID, input.OrgUnitID, input.Version).
			Updates(map[string]any{
				"parent_id":  nullableString(input.ParentID),
				"name":       input.Name,
				"path":       newPath,
				"depth":      newDepth,
				"sort_order": input.SortOrder,
				"updated_at": now,
				"updated_by": input.OperatorID,
				"version":    gorm.Expr("version + 1"),
			})
		if result.Error != nil {
			return mapWriteError(result.Error, "update organization unit")
		}
		if result.RowsAffected != 1 {
			return application.ErrVersionConflict
		}

		if existing.Path != newPath {
			depthOffset := int(newDepth) - int(existing.Depth)
			result = transaction.Model(&orgUnitModel{}).
				Where("tenant_id = ? AND id <> ? AND path LIKE ?", input.TenantID, existing.ID, existing.Path+"%").
				Updates(map[string]any{
					"path":       gorm.Expr("REPLACE(path, ?, ?)", existing.Path, newPath),
					"depth":      gorm.Expr("depth + ?", depthOffset),
					"updated_at": now,
					"updated_by": input.OperatorID,
					"version":    gorm.Expr("version + 1"),
				})
			if result.Error != nil {
				return mapWriteError(result.Error, "update organization subtree path")
			}
		}
		identityIDs, err := membershipIdentityIDs(transaction, input.TenantID, transaction.Model(&membershipModel{}).Where("org_unit_id IN (?)", transaction.Model(&orgUnitModel{}).Select("id").Where("tenant_id = ? AND path LIKE ?", input.TenantID, newPath+"%")))
		if err != nil {
			return err
		}
		if err := enqueueKeycloakIdentityEvents(transaction, input.TenantID, identityIDs, now, "ORGANIZATION_CHANGED"); err != nil {
			return err
		}

		var row orgUnitModel
		if err := transaction.Where("tenant_id = ? AND id = ?", input.TenantID, input.OrgUnitID).First(&row).Error; err != nil {
			return fmt.Errorf("read updated organization unit: %w", err)
		}
		updated = toDomainOrgUnit(row)
		return nil
	})
	if err != nil {
		return domain.OrgUnit{}, err
	}
	return updated, nil
}

// DeleteOrgUnit 逻辑停用整个组织子树，并同步停用其岗位、任职及组织/岗位授权产物；
// 数据仍按状态保留用于审计，但不能再作为身份归属或授权继承来源。
func (repository *GORMRepository) DeleteOrgUnit(ctx context.Context, input application.OrgUnitDeleteInput) error {
	return repository.database.WithContext(ctx).Transaction(func(transaction *gorm.DB) error {
		var existing orgUnitModel
		result := transaction.Clauses(clause.Locking{Strength: "UPDATE"}).
			Where("tenant_id = ? AND id = ?", input.TenantID, input.OrgUnitID).
			First(&existing)
		if errors.Is(result.Error, gorm.ErrRecordNotFound) {
			return application.ErrNotFound
		}
		if result.Error != nil {
			return fmt.Errorf("lock organization unit for deletion: %w", result.Error)
		}
		if existing.Version != input.Version {
			return application.ErrVersionConflict
		}
		if existing.Status != domain.StatusActive {
			return application.ErrConflict
		}

		now := time.Now().UTC()
		organizationIDs := transaction.Model(&orgUnitModel{}).Select("id").
			Where("tenant_id = ? AND path LIKE ?", input.TenantID, existing.Path+"%")
		identityIDs, err := membershipIdentityIDs(transaction, input.TenantID, transaction.Model(&membershipModel{}).Where("org_unit_id IN (?)", organizationIDs))
		if err != nil {
			return err
		}
		positionIDs := transaction.Model(&positionModel{}).Select("id").
			Where("tenant_id = ? AND org_unit_id IN (?)", input.TenantID, organizationIDs)
		if err := disableOrganizationRoleBindings(transaction, input.TenantID, organizationIDs, input.OperatorID, now); err != nil {
			return err
		}
		if err := disablePositionAuthorizationArtifacts(transaction, input.TenantID, positionIDs, input.OperatorID, now); err != nil {
			return err
		}
		if result = transaction.Model(&orgUnitModel{}).
			Where("tenant_id = ? AND path LIKE ? AND status <> ?", input.TenantID, existing.Path+"%", domain.StatusDisabled).
			Updates(map[string]any{
				"status":     domain.StatusDisabled,
				"updated_at": now,
				"updated_by": input.OperatorID,
				"version":    gorm.Expr("version + 1"),
			}); result.Error != nil {
			return mapWriteError(result.Error, "disable organization subtree")
		}
		if result = transaction.Model(&positionModel{}).
			Where("tenant_id = ? AND org_unit_id IN (?) AND status <> ?", input.TenantID, organizationIDs, domain.StatusDisabled).
			Updates(map[string]any{
				"status":     domain.StatusDisabled,
				"updated_at": now,
				"updated_by": input.OperatorID,
				"version":    gorm.Expr("version + 1"),
			}); result.Error != nil {
			return mapWriteError(result.Error, "disable organization positions")
		}
		if result = transaction.Model(&membershipModel{}).
			Where("tenant_id = ? AND org_unit_id IN (?) AND status <> ?", input.TenantID, organizationIDs, domain.StatusDisabled).
			Updates(map[string]any{
				"status":     domain.StatusDisabled,
				"is_primary": false,
				"updated_at": now,
				"updated_by": input.OperatorID,
				"version":    gorm.Expr("version + 1"),
			}); result.Error != nil {
			return mapWriteError(result.Error, "disable organization memberships")
		}
		if result = transaction.Model(&userModel{}).
			Where("tenant_id = ? AND primary_org_id IN (?)", input.TenantID, organizationIDs).
			Updates(map[string]any{
				"primary_org_id": nil,
				"updated_at":     now,
				"updated_by":     input.OperatorID,
				"version":        gorm.Expr("version + 1"),
			}); result.Error != nil {
			return mapWriteError(result.Error, "clear users primary organization")
		}
		if err := advanceMembershipAuthorizationRevisions(transaction, input.TenantID, now, "组织删除导致组织/岗位继承授权变化"); err != nil {
			return err
		}
		if err := enqueueKeycloakIdentityEvents(transaction, input.TenantID, identityIDs, now, "ORGANIZATION_CHANGED"); err != nil {
			return err
		}
		return nil
	})
}

// disableOrganizationRoleBindings closes every role binding owned by an organization in the
// deleted subtree. The rows remain available for audit, while the subject type predicate keeps
// user and position bindings outside this subject-lifecycle cleanup operation. This cleanup is
// intentionally broader than the management UI, which permits revoking MANUAL bindings only.
func disableOrganizationRoleBindings(transaction *gorm.DB, tenantID string, organizationIDs *gorm.DB, operatorID string, now time.Time) error {
	result := buildDisableOrganizationRoleBindings(transaction, tenantID, organizationIDs, operatorID, now)
	if result.Error != nil {
		return mapWriteError(result.Error, "disable organization role bindings")
	}
	return nil
}

func buildDisableOrganizationRoleBindings(transaction *gorm.DB, tenantID string, organizationIDs *gorm.DB, operatorID string, now time.Time) *gorm.DB {
	return transaction.Table("authz_role_binding").
		Where("tenant_id = ? AND subject_type = ? AND subject_id IN (?) AND status = ?", tenantID, "ORG_UNIT", organizationIDs, domain.StatusActive).
		Updates(map[string]any{
			"status":     domain.StatusDisabled,
			"updated_at": now,
			"updated_by": operatorID,
			"version":    gorm.Expr("version + 1"),
		})
}

func (repository *GORMRepository) ListPositions(ctx context.Context, tenantID, keyword, status string, query application.PageRequest) (application.PageResult[domain.Position], error) {
	database := applyPositionFilter(repository.database.WithContext(ctx).Model(&positionModel{}), tenantID, keyword, status)
	database = applyManagementAuthorizationScope(database, query, "id", "org_unit_id")
	var total int64
	if err := database.Count(&total).Error; err != nil {
		return application.PageResult[domain.Position]{}, fmt.Errorf("count positions: %w", err)
	}

	var rows []positionModel
	if err := database.Order("org_unit_id, code").Limit(query.PageSize).Offset(pageOffset(query)).Find(&rows).Error; err != nil {
		return application.PageResult[domain.Position]{}, fmt.Errorf("list positions: %w", err)
	}
	items := make([]domain.Position, 0, len(rows))
	for _, row := range rows {
		items = append(items, toDomainPosition(row))
	}
	return application.PageResult[domain.Position]{Items: items, Page: query.Page, PageSize: query.PageSize, Total: total}, nil
}

func (repository *GORMRepository) CreatePosition(ctx context.Context, position domain.Position, operatorID string) (domain.Position, error) {
	err := repository.database.WithContext(ctx).Transaction(func(transaction *gorm.DB) error {
		var total int64
		if err := transaction.Model(&orgUnitModel{}).Where("id = ? AND tenant_id = ? AND status = ?", position.OrgUnitID, position.TenantID, domain.StatusActive).Count(&total).Error; err != nil {
			return fmt.Errorf("check position organization unit: %w", err)
		}
		if total != 1 {
			return application.ErrNotFound
		}
		now := time.Now().UTC()
		if err := transaction.Create(&positionModel{
			ID:        position.ID,
			TenantID:  position.TenantID,
			OrgUnitID: position.OrgUnitID,
			Code:      position.Code,
			Name:      position.Name,
			Status:    position.Status,
			Version:   1,
			CreatedAt: now,
			CreatedBy: nullableString(&operatorID),
			UpdatedAt: now,
			UpdatedBy: nullableString(&operatorID),
		}).Error; err != nil {
			return mapWriteError(err, "create position")
		}
		return nil
	})
	if err != nil {
		return domain.Position{}, err
	}
	return position, nil
}

// DeletePosition logically deletes one position and disables every active membership that points
// to it. The transaction also clears affected users' primary organization shortcut and advances
// inherited-authorization revisions so refreshed subsystem claims stop carrying the position role.
func (repository *GORMRepository) DeletePosition(ctx context.Context, input application.PositionDeleteInput) error {
	return repository.database.WithContext(ctx).Transaction(func(transaction *gorm.DB) error {
		var existing positionModel
		result := transaction.Clauses(clause.Locking{Strength: "UPDATE"}).
			Where("tenant_id = ? AND id = ?", input.TenantID, input.PositionID).
			First(&existing)
		if errors.Is(result.Error, gorm.ErrRecordNotFound) {
			return application.ErrNotFound
		}
		if result.Error != nil {
			return fmt.Errorf("lock position for deletion: %w", result.Error)
		}
		if existing.Version != input.Version {
			return application.ErrVersionConflict
		}
		if existing.Status != domain.StatusActive {
			return application.ErrConflict
		}

		now := time.Now().UTC()
		positionIDs := transaction.Model(&positionModel{}).Select("id").
			Where("tenant_id = ? AND id = ?", input.TenantID, input.PositionID)
		identityIDs, err := membershipIdentityIDs(transaction, input.TenantID, transaction.Model(&membershipModel{}).Where("position_id IN (?)", positionIDs))
		if err != nil {
			return err
		}
		if err := disablePositionAuthorizationArtifacts(transaction, input.TenantID, positionIDs, input.OperatorID, now); err != nil {
			return err
		}
		primaryUserIDs := transaction.Model(&membershipModel{}).Select("user_id").
			Where("tenant_id = ? AND position_id = ? AND status = ? AND is_primary = ?", input.TenantID, input.PositionID, domain.StatusActive, true)
		if result = transaction.Model(&userModel{}).
			Where("tenant_id = ? AND id IN (?)", input.TenantID, primaryUserIDs).
			Updates(map[string]any{
				"primary_org_id": nil,
				"updated_at":     now,
				"updated_by":     input.OperatorID,
				"version":        gorm.Expr("version + 1"),
			}); result.Error != nil {
			return mapWriteError(result.Error, "clear deleted position primary organizations")
		}
		if result = transaction.Model(&membershipModel{}).
			Where("tenant_id = ? AND position_id = ? AND status <> ?", input.TenantID, input.PositionID, domain.StatusDisabled).
			Updates(map[string]any{
				"status":     domain.StatusDisabled,
				"is_primary": false,
				"updated_at": now,
				"updated_by": input.OperatorID,
				"version":    gorm.Expr("version + 1"),
			}); result.Error != nil {
			return mapWriteError(result.Error, "disable deleted position memberships")
		}
		if result = transaction.Model(&positionModel{}).
			Where("tenant_id = ? AND id = ? AND version = ? AND status = ?", input.TenantID, input.PositionID, input.Version, domain.StatusActive).
			Updates(map[string]any{
				"status":     domain.StatusDisabled,
				"updated_at": now,
				"updated_by": input.OperatorID,
				"version":    gorm.Expr("version + 1"),
			}); result.Error != nil {
			return mapWriteError(result.Error, "disable position")
		}
		if result.RowsAffected != 1 {
			return application.ErrVersionConflict
		}
		if err := advanceMembershipAuthorizationRevisions(transaction, input.TenantID, now, "岗位删除导致岗位继承授权变化"); err != nil {
			return err
		}
		if err := enqueueKeycloakIdentityEvents(transaction, input.TenantID, identityIDs, now, "ORGANIZATION_CHANGED"); err != nil {
			return err
		}
		return nil
	})
}

// disablePositionAuthorizationArtifacts closes every active authorization edge owned by a
// position before that position is disabled. Keeping the rows as DISABLED preserves audit and
// optimistic-lock history while preventing stale template assignments from appearing active.
func disablePositionAuthorizationArtifacts(transaction *gorm.DB, tenantID string, positionIDs *gorm.DB, operatorID string, now time.Time) error {
	assignmentResult := buildDisablePositionAuthorizationTemplateAssignments(transaction, tenantID, positionIDs, operatorID, now)
	if assignmentResult.Error != nil {
		return mapWriteError(assignmentResult.Error, "disable position authorization template assignments")
	}

	bindingResult := buildDisablePositionRoleBindings(transaction, tenantID, positionIDs, operatorID, now)
	if bindingResult.Error != nil {
		return mapWriteError(bindingResult.Error, "disable position role bindings")
	}
	return nil
}

func buildDisablePositionAuthorizationTemplateAssignments(transaction *gorm.DB, tenantID string, positionIDs *gorm.DB, operatorID string, now time.Time) *gorm.DB {
	return transaction.Table("authz_position_grant_template_assignment").
		Where("tenant_id = ? AND position_id IN (?) AND status <> ?", tenantID, positionIDs, domain.StatusDisabled).
		Updates(map[string]any{
			"status":     domain.StatusDisabled,
			"updated_at": now,
			"updated_by": operatorID,
			"version":    gorm.Expr("version + 1"),
		})
}

func buildDisablePositionRoleBindings(transaction *gorm.DB, tenantID string, positionIDs *gorm.DB, operatorID string, now time.Time) *gorm.DB {
	return transaction.Table("authz_role_binding").
		Where("tenant_id = ? AND subject_type = ? AND subject_id IN (?) AND status <> ?", tenantID, "POSITION", positionIDs, domain.StatusDisabled).
		Updates(map[string]any{
			"status":     domain.StatusDisabled,
			"updated_at": now,
			"updated_by": operatorID,
			"version":    gorm.Expr("version + 1"),
		})
}

func (repository *GORMRepository) ListMemberships(ctx context.Context, tenantID string, query application.PageRequest) (application.PageResult[domain.Membership], error) {
	database := applyMembershipFilter(repository.membershipQuery(ctx), tenantID, query)
	database = applyManagementAuthorizationScope(database, query, "m.id", "m.org_unit_id")
	var total int64
	if err := database.Count(&total).Error; err != nil {
		return application.PageResult[domain.Membership]{}, fmt.Errorf("count memberships: %w", err)
	}

	var rows []membershipProjection
	if err := database.Select(membershipSelectColumns).Order("m.created_at DESC, m.id DESC").Limit(query.PageSize).Offset(pageOffset(query)).Find(&rows).Error; err != nil {
		return application.PageResult[domain.Membership]{}, fmt.Errorf("list memberships: %w", err)
	}
	items := make([]domain.Membership, 0, len(rows))
	for _, row := range rows {
		items = append(items, toDomainMembership(row))
	}
	return application.PageResult[domain.Membership]{Items: items, Page: query.Page, PageSize: query.PageSize, Total: total}, nil
}

func (repository *GORMRepository) CreateMembership(ctx context.Context, input application.MembershipCreateInput, id string) (domain.Membership, error) {
	err := repository.database.WithContext(ctx).Transaction(func(transaction *gorm.DB) error {
		// 服务端以联表校验用户、组织、岗位同租户且岗位确属该组织，不能信任前端提交的
		// org_unit_id/position_id 组合；主任职唯一性当前仅做事务内应用层检查，并发完整性仍需
		// 数据库约束或用户级锁兜底。用户快捷主组织与任职写入则在同一事务内提交。
		if err := ensureMembershipReferences(ctx, transaction, input); err != nil {
			return err
		}
		if err := lockMembershipUsers(ctx, transaction, input.TenantID, input.UserID); err != nil {
			return err
		}
		isPrimary := input.MembershipType == domain.MembershipPrimary
		if isPrimary {
			if err := ensureNoOtherPrimary(ctx, transaction, input.TenantID, input.UserID, ""); err != nil {
				return err
			}
		}
		now := time.Now().UTC()
		if err := transaction.Create(&membershipModel{
			ID:                   id,
			TenantID:             input.TenantID,
			UserID:               input.UserID,
			OrgUnitID:            input.OrgUnitID,
			PositionID:           input.PositionID,
			MembershipType:       input.MembershipType,
			IsPrimary:            isPrimary,
			InheritAuthorization: input.InheritAuthorization == nil || *input.InheritAuthorization,
			ValidFrom:            nullableTime(input.EffectiveFrom),
			ValidUntil:           nullableTime(input.EffectiveTo),
			Status:               domain.StatusActive,
			Version:              1,
			CreatedAt:            now,
			CreatedBy:            nullableString(&input.OperatorID),
			UpdatedAt:            now,
			UpdatedBy:            nullableString(&input.OperatorID),
		}).Error; err != nil {
			return mapWriteError(err, "create membership")
		}
		if isPrimary {
			result := transaction.Model(&userModel{}).Where("id = ? AND tenant_id = ?", input.UserID, input.TenantID).Updates(map[string]any{
				"primary_org_id": input.OrgUnitID,
				"updated_at":     now,
				"updated_by":     input.OperatorID,
				"version":        gorm.Expr("version + 1"),
			})
			if result.Error != nil {
				return fmt.Errorf("set user primary organization: %w", result.Error)
			}
		}
		if err := advanceMembershipAuthorizationRevisions(transaction, input.TenantID, now, "任职关系创建导致组织/岗位继承授权变化"); err != nil {
			return err
		}
		if err := enqueueKeycloakIdentityEvents(transaction, input.TenantID, []string{input.UserID}, now, "ORGANIZATION_CHANGED"); err != nil {
			return err
		}
		return nil
	})
	if err != nil {
		return domain.Membership{}, err
	}
	return repository.getMembership(ctx, input.TenantID, id)
}

func (repository *GORMRepository) UpdateMembership(ctx context.Context, input application.MembershipUpdateInput) (domain.Membership, error) {
	resultID := input.MembershipID
	err := repository.database.WithContext(ctx).Transaction(func(transaction *gorm.DB) error {
		// 任职更新可能同时换用户、组织、岗位、主次和继承开关；先验证新引用，再用版本号
		// 更新，并在同一事务清理旧主组织、设置新主组织和推进授权修订号。
		var existing membershipModel
		result := transaction.Where("tenant_id = ? AND id = ?", input.TenantID, input.MembershipID).First(&existing)
		if errors.Is(result.Error, gorm.ErrRecordNotFound) {
			return application.ErrNotFound
		}
		if result.Error != nil {
			return fmt.Errorf("get membership before update: %w", result.Error)
		}
		if err := ensureMembershipReferences(ctx, transaction, input.MembershipCreateInput); err != nil {
			return err
		}
		if err := lockMembershipUsers(ctx, transaction, input.TenantID, existing.UserID, input.UserID); err != nil {
			return err
		}

		status := existing.Status
		if input.Status != nil {
			status = *input.Status
		}
		isPrimary := input.MembershipType == domain.MembershipPrimary && status == domain.StatusActive
		if isPrimary {
			if err := ensureNoOtherPrimary(ctx, transaction, input.TenantID, input.UserID, input.MembershipID); err != nil {
				return err
			}
		}

		now := time.Now().UTC()
		// 岗位、组织或主次任职发生变化属于人员异动，绝不能覆盖历史任职。
		// 结束旧任职并创建新任职，使晋升、降职和调岗的授权来源可审计。
		if existing.OrgUnitID != input.OrgUnitID || existing.PositionID != input.PositionID || existing.MembershipType != input.MembershipType {
			newID, idErr := ulid.Generator{}.New(now)
			if idErr != nil {
				return fmt.Errorf("generate personnel change membership id: %w", idErr)
			}
			if err := transaction.Model(&membershipModel{}).Where("tenant_id = ? AND id = ? AND version = ?", input.TenantID, existing.ID, existing.Version).Updates(map[string]any{"status": domain.StatusDisabled, "is_primary": false, "valid_until": nullableTime(input.EffectiveFrom), "updated_at": now, "updated_by": input.OperatorID, "version": gorm.Expr("version + 1")}).Error; err != nil {
				return mapWriteError(err, "close prior membership")
			}
			primary := input.MembershipType == domain.MembershipPrimary && *input.Status == domain.StatusActive
			if primary {
				if err := ensureNoOtherPrimary(ctx, transaction, input.TenantID, input.UserID, ""); err != nil {
					return err
				}
			}
			if err := transaction.Create(&membershipModel{ID: newID, TenantID: input.TenantID, UserID: input.UserID, OrgUnitID: input.OrgUnitID, PositionID: input.PositionID, MembershipType: input.MembershipType, IsPrimary: primary, InheritAuthorization: input.InheritAuthorization == nil || *input.InheritAuthorization, ValidFrom: nullableTime(input.EffectiveFrom), ValidUntil: nullableTime(input.EffectiveTo), Status: *input.Status, Version: 1, CreatedAt: now, CreatedBy: nullableString(&input.OperatorID), UpdatedAt: now, UpdatedBy: nullableString(&input.OperatorID)}).Error; err != nil {
				return mapWriteError(err, "create changed membership")
			}
			if primary {
				if err := transaction.Model(&userModel{}).Where("tenant_id = ? AND id = ?", input.TenantID, input.UserID).Updates(map[string]any{"primary_org_id": input.OrgUnitID, "updated_at": now, "updated_by": input.OperatorID, "version": gorm.Expr("version + 1")}).Error; err != nil {
					return err
				}
			}
			if err := advanceMembershipAuthorizationRevisions(transaction, input.TenantID, now, "人员异动导致岗位继承授权变化"); err != nil {
				return err
			}
			if err := enqueueKeycloakIdentityEvents(transaction, input.TenantID, []string{existing.UserID, input.UserID}, now, "ORGANIZATION_CHANGED"); err != nil {
				return err
			}
			resultID = newID
			return nil
		}
		updates := map[string]any{
			"user_id":         input.UserID,
			"org_unit_id":     input.OrgUnitID,
			"position_id":     input.PositionID,
			"membership_type": input.MembershipType,
			"is_primary":      isPrimary,
			"valid_from":      nullableTime(input.EffectiveFrom),
			"valid_until":     nullableTime(input.EffectiveTo),
			"status":          status,
			"updated_at":      now,
			"updated_by":      input.OperatorID,
			"version":         gorm.Expr("version + 1"),
		}
		if input.InheritAuthorization != nil {
			updates["inherit_authorization"] = *input.InheritAuthorization
		}
		result = transaction.Model(&membershipModel{}).
			Where("tenant_id = ? AND id = ? AND version = ?", input.TenantID, input.MembershipID, input.Version).
			Updates(updates)
		if result.Error != nil {
			return mapWriteError(result.Error, "update membership")
		}
		if result.RowsAffected == 0 {
			return application.ErrVersionConflict
		}
		existingPrimary := existing.IsPrimary && existing.Status == domain.StatusActive
		if existingPrimary {
			result = transaction.Model(&userModel{}).
				Where("tenant_id = ? AND id = ? AND primary_org_id = ?", input.TenantID, existing.UserID, existing.OrgUnitID).
				Updates(map[string]any{"primary_org_id": nil, "updated_at": now, "updated_by": input.OperatorID, "version": gorm.Expr("version + 1")})
			if result.Error != nil {
				return fmt.Errorf("clear prior primary organization: %w", result.Error)
			}
		}
		if isPrimary {
			result = transaction.Model(&userModel{}).
				Where("tenant_id = ? AND id = ?", input.TenantID, input.UserID).
				Updates(map[string]any{"primary_org_id": input.OrgUnitID, "updated_at": now, "updated_by": input.OperatorID, "version": gorm.Expr("version + 1")})
			if result.Error != nil {
				return fmt.Errorf("set updated primary organization: %w", result.Error)
			}
		}
		if err := advanceMembershipAuthorizationRevisions(transaction, input.TenantID, now, "任职关系更新导致组织/岗位继承授权变化"); err != nil {
			return err
		}
		if err := enqueueKeycloakIdentityEvents(transaction, input.TenantID, []string{existing.UserID, input.UserID}, now, "ORGANIZATION_CHANGED"); err != nil {
			return err
		}
		return nil
	})
	if err != nil {
		return domain.Membership{}, err
	}
	return repository.getMembership(ctx, input.TenantID, resultID)
}

// advanceMembershipAuthorizationRevisions 使所有可能通过组织或岗位继承角色的应用授权
// 快照失效。这里保守更新租户内全部相关应用，而不猜测新旧任职具体命中了哪些绑定，
// 从而覆盖有效期切换和后续绑定解析，并保持事务内一致。
func advanceMembershipAuthorizationRevisions(transaction *gorm.DB, tenantID string, now time.Time, reason string) error {
	result := buildMembershipAuthorizationRevisionUpdate(transaction, tenantID, now, reason)
	if result.Error != nil {
		return fmt.Errorf("advance membership authorization policy revisions: %w", result.Error)
	}
	return nil
}

func buildMembershipAuthorizationRevisionUpdate(transaction *gorm.DB, tenantID string, now time.Time, reason string) *gorm.DB {
	return transaction.Model(&identityPolicyRevisionModel{}).
		Where("tenant_id = ?", tenantID).
		Where(`EXISTS (
			SELECT 1
			FROM authz_role_binding AS inherited_binding
			WHERE inherited_binding.tenant_id = authz_policy_revision.tenant_id
				AND inherited_binding.application_id = authz_policy_revision.application_id
				AND inherited_binding.subject_type IN ('ORG_UNIT', 'POSITION')
		)`).
		Updates(map[string]any{
			"revision":      gorm.Expr("revision + 1"),
			"changed_at":    now,
			"change_reason": reason,
		})
}

const membershipSelectColumns = `m.id, m.tenant_id, u.id AS user_id, u.display_name AS user_name,
	o.id AS org_unit_id, o.name AS org_unit_name, p.id AS position_id, p.name AS position_name,
	m.membership_type, m.valid_from, m.valid_until, m.status, m.version, m.is_primary, m.inherit_authorization`

func (repository *GORMRepository) membershipQuery(ctx context.Context) *gorm.DB {
	return repository.database.WithContext(ctx).
		Table("iam_membership AS m").
		Joins("JOIN iam_user AS u ON u.id = m.user_id AND u.tenant_id = m.tenant_id").
		Joins("JOIN iam_org_unit AS o ON o.id = m.org_unit_id AND o.tenant_id = m.tenant_id").
		Joins("JOIN iam_position AS p ON p.id = m.position_id AND p.tenant_id = m.tenant_id")
}

func (repository *GORMRepository) getMembership(ctx context.Context, tenantID, membershipID string) (domain.Membership, error) {
	var row membershipProjection
	result := repository.membershipQuery(ctx).Select(membershipSelectColumns).Where("m.tenant_id = ? AND m.id = ?", tenantID, membershipID).Limit(1).Find(&row)
	if result.Error != nil {
		return domain.Membership{}, fmt.Errorf("get membership: %w", result.Error)
	}
	if result.RowsAffected == 0 {
		return domain.Membership{}, application.ErrNotFound
	}
	return toDomainMembership(row), nil
}

func ensureMembershipReferences(ctx context.Context, database *gorm.DB, input application.MembershipCreateInput) error {
	// 一条联表查询同时证明：用户活动、组织活动、岗位活动、三者同租户且岗位属于组织。
	// 这道应用层约束是跨租户与组织岗位错配写入的主要防线。
	var total int64
	result := database.WithContext(ctx).
		Model(&userModel{}).
		Joins("JOIN iam_org_unit AS o ON o.id = ? AND o.tenant_id = iam_user.tenant_id AND o.status = ?", input.OrgUnitID, domain.StatusActive).
		Joins("JOIN iam_position AS p ON p.id = ? AND p.tenant_id = iam_user.tenant_id AND p.org_unit_id = o.id AND p.status = ?", input.PositionID, domain.StatusActive).
		Where("iam_user.id = ? AND iam_user.tenant_id = ? AND iam_user.status = ?", input.UserID, input.TenantID, domain.StatusActive).
		Count(&total)
	if result.Error != nil {
		return fmt.Errorf("validate membership references: %w", result.Error)
	}
	if total != 1 {
		return application.ErrNotFound
	}
	return nil
}

func ensureNoOtherPrimary(ctx context.Context, database *gorm.DB, tenantID, userID, excludingID string) error {
	query := database.WithContext(ctx).Model(&membershipModel{}).
		Where("tenant_id = ? AND user_id = ? AND is_primary = ? AND status = ?", tenantID, userID, true, domain.StatusActive)
	if excludingID != "" {
		query = query.Where("id <> ?", excludingID)
	}
	var total int64
	if err := query.Count(&total).Error; err != nil {
		return fmt.Errorf("check primary membership: %w", err)
	}
	if total > 0 {
		return application.ErrConflict
	}
	return nil
}

// lockMembershipUsers serializes all primary-membership changes for the affected users.
// IDs are locked in deterministic order so moving an appointment between users cannot
// deadlock with another concurrent move in the opposite direction.
func lockMembershipUsers(ctx context.Context, database *gorm.DB, tenantID string, userIDs ...string) error {
	ids := make([]string, 0, len(userIDs))
	seen := make(map[string]struct{}, len(userIDs))
	for _, id := range userIDs {
		id = strings.TrimSpace(id)
		if id == "" {
			continue
		}
		if _, ok := seen[id]; ok {
			continue
		}
		seen[id] = struct{}{}
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		var user userModel
		result := database.WithContext(ctx).Clauses(clause.Locking{Strength: "UPDATE"}).
			Where("tenant_id = ? AND id = ? AND status = ?", tenantID, id, domain.StatusActive).First(&user)
		if errors.Is(result.Error, gorm.ErrRecordNotFound) {
			return application.ErrNotFound
		}
		if result.Error != nil {
			return fmt.Errorf("lock membership user: %w", result.Error)
		}
	}
	return nil
}

func applyUserFilter(database *gorm.DB, tenantID string, query application.PageRequest) *gorm.DB {
	database = database.Where("tenant_id = ? AND deleted_at IS NULL", tenantID)
	if query.Keyword != "" {
		like := "%" + query.Keyword + "%"
		database = database.Where("display_name LIKE ? OR employee_no LIKE ? OR email LIKE ?", like, like, like)
	}
	if query.Status != "" {
		database = database.Where("status = ?", query.Status)
	}
	return database
}

func applyAccountFilter(database *gorm.DB, tenantID string, query application.PageRequest) *gorm.DB {
	ownerClause, ownerArgs := accountOwnerVisibilityFilter()
	database = database.Where("tenant_id = ?", tenantID).Where(ownerClause, ownerArgs...)
	if query.Keyword != "" {
		database = database.Where("username LIKE ?", "%"+query.Keyword+"%")
	}
	if query.Status != "" {
		database = database.Where("status = ?", query.Status)
	}
	return database
}

// accountOwnerVisibilityFilter hides login accounts whose owning user has been business-deleted.
// Disabled but non-deleted users remain manageable. Service accounts without a user_id remain
// visible. Account rows stay in the database so security and audit references keep integrity.
func accountOwnerVisibilityFilter() (string, []any) {
	return `(iam_account.user_id IS NULL OR EXISTS (
		SELECT 1 FROM iam_user AS linked_user
		WHERE linked_user.id = iam_account.user_id
		  AND linked_user.tenant_id = iam_account.tenant_id
		  AND linked_user.deleted_at IS NULL
	))`, nil
}

func applyOrganizationFilter(database *gorm.DB, tenantID, keyword, status string) *gorm.DB {
	database = database.Where("tenant_id = ?", tenantID)
	if keyword != "" {
		like := "%" + keyword + "%"
		database = database.Where("code LIKE ? OR name LIKE ?", like, like)
	}
	if status != "" {
		database = database.Where("status = ?", status)
	}
	return database
}

func applyPositionFilter(database *gorm.DB, tenantID, keyword, status string) *gorm.DB {
	database = database.Where("tenant_id = ?", tenantID)
	if keyword != "" {
		like := "%" + keyword + "%"
		database = database.Where("code LIKE ? OR name LIKE ?", like, like)
	}
	if status != "" {
		database = database.Where("status = ?", status)
	}
	return database
}

func applyMembershipFilter(database *gorm.DB, tenantID string, query application.PageRequest) *gorm.DB {
	database = database.Where("m.tenant_id = ?", tenantID)
	if query.Keyword != "" {
		like := "%" + query.Keyword + "%"
		database = database.Where("u.display_name LIKE ? OR o.name LIKE ?", like, like)
	}
	if query.Status != "" {
		database = database.Where("m.status = ?", query.Status)
	}
	return database
}

func applyManagementAuthorizationScope(database *gorm.DB, query application.PageRequest, resourceColumn, organizationColumn string) *gorm.DB {
	if !query.ScopeRestricted {
		return database
	}
	clauses := make([]string, 0, 2)
	arguments := make([]any, 0, 2)
	if len(query.AllowedOrgUnitIDs) > 0 {
		clauses = append(clauses, organizationColumn+" IN ?")
		arguments = append(arguments, query.AllowedOrgUnitIDs)
	}
	if len(query.AllowedResourceIDs) > 0 {
		clauses = append(clauses, resourceColumn+" IN ?")
		arguments = append(arguments, query.AllowedResourceIDs)
	}
	if len(clauses) == 0 {
		return database.Where("1 = 0")
	}
	return database.Where("("+strings.Join(clauses, " OR ")+")", arguments...)
}

func pageOffset(query application.PageRequest) int {
	return (query.Page - 1) * query.PageSize
}

func toAccountWithFallbackName(model accountModel) domain.Account {
	account := toDomainAccount(model)
	if account.AccountName == "" {
		account.AccountName = account.ID
	}
	return account
}

func mapWriteError(err error, operation string) error {
	if errors.Is(err, gorm.ErrDuplicatedKey) {
		return application.ErrConflict
	}
	return fmt.Errorf("%s: %w", operation, err)
}

// keep strings imported through this compile-time check while application filters stay explicit.
