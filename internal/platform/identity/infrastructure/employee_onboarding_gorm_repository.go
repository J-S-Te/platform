package infrastructure

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/J-S-Te/Basic-Platform/internal/platform/identity/application"
	"github.com/J-S-Te/Basic-Platform/internal/platform/identity/domain"
	"gorm.io/gorm"
)

// CreateEmployee 用一个数据库事务持久化完整入职聚合。这里不串联各自拥有事务边界的
// CreateUsers/CreateLocalAccount/CreateMembership，否则后一步失败时无法回滚前面已提交的数据。
func (repository *GORMRepository) CreateEmployee(ctx context.Context, write application.EmployeeOnboardingWrite) (domain.User, *domain.Account, *domain.Membership, error) {
	if write.User.ID == "" || write.User.RoleBindingID == "" || write.User.TenantID == "" || write.User.OperatorID == "" {
		return domain.User{}, nil, nil, application.ErrValidation
	}
	if write.Account != nil && (write.Account.UserID != write.User.ID || write.Account.TenantID != write.User.TenantID) {
		return domain.User{}, nil, nil, application.ErrValidation
	}
	if write.Membership != nil && (write.MembershipID == "" || write.Membership.UserID != write.User.ID || write.Membership.TenantID != write.User.TenantID) {
		return domain.User{}, nil, nil, application.ErrValidation
	}

	var user domain.User
	var account *domain.Account
	var membershipID string
	err := repository.database.WithContext(ctx).Transaction(func(transaction *gorm.DB) error {
		var platformApplication bootstrapApplicationModel
		result := transaction.Where("tenant_id = ? AND code = ? AND status = ?", write.User.TenantID, application.DefaultPlatformApplicationCode, domain.StatusActive).First(&platformApplication)
		if errors.Is(result.Error, gorm.ErrRecordNotFound) {
			return fmt.Errorf("resolve platform application: %w", application.ErrNotFound)
		}
		if result.Error != nil {
			return fmt.Errorf("resolve platform application: %w", result.Error)
		}
		var ordinaryUserRole bootstrapRoleModel
		result = transaction.Where("tenant_id = ? AND application_id = ? AND code = ? AND status = ?", write.User.TenantID, platformApplication.ID, application.DefaultUserRoleCode, domain.StatusActive).First(&ordinaryUserRole)
		if errors.Is(result.Error, gorm.ErrRecordNotFound) {
			return fmt.Errorf("resolve ordinary-user role: %w", application.ErrNotFound)
		}
		if result.Error != nil {
			return fmt.Errorf("resolve ordinary-user role: %w", result.Error)
		}
		// 平台普通用户角色是所有员工身份的基线授权；种子应用或角色缺失属于部署损坏，
		// 必须让整次入职失败，不能创建一个无法进入平台的孤立用户。

		now := write.OccurredAt.UTC()
		if now.IsZero() {
			now = time.Now().UTC()
		}
		row := userModel{
			ID: write.User.ID, TenantID: write.User.TenantID, EmployeeNo: nullableString(write.User.EmployeeNo),
			DisplayName: write.User.DisplayName, Email: nullableString(write.User.Email),
			MobileCiphertext: nullableBytes(write.User.MobileCiphertext), MobileHash: nullableBytes(write.User.MobileHash),
			EmploymentStatus: "EMPLOYED", Status: write.User.Status, Version: 1,
			CreatedAt: now, CreatedBy: nullableString(&write.User.OperatorID), UpdatedAt: now, UpdatedBy: nullableString(&write.User.OperatorID),
		}
		if err := transaction.Create(&row).Error; err != nil {
			return mapWriteError(err, "create employee user")
		}
		binding := bootstrapRoleBindingModel{
			ID: write.User.RoleBindingID, TenantID: write.User.TenantID, ApplicationID: platformApplication.ID,
			RoleID: ordinaryUserRole.ID, SubjectType: "USER", SubjectID: write.User.ID,
			ScopeType: "TENANT", ScopeID: "", Status: domain.StatusActive, Version: 1,
			CreatedAt: now, CreatedBy: nullableString(&write.User.OperatorID), UpdatedAt: now, UpdatedBy: nullableString(&write.User.OperatorID),
		}
		if err := transaction.Create(&binding).Error; err != nil {
			return mapWriteError(err, "bind employee ordinary-user role")
		}
		result = transaction.Model(&identityPolicyRevisionModel{}).Where("tenant_id = ? AND application_id = ?", write.User.TenantID, platformApplication.ID).Updates(map[string]any{
			"revision": gorm.Expr("revision + 1"), "changed_at": now, "change_reason": "新增员工自动绑定普通用户角色",
		})
		if result.Error != nil {
			return fmt.Errorf("advance employee authorization policy revision: %w", result.Error)
		}
		if result.RowsAffected == 0 {
			return fmt.Errorf("advance employee authorization policy revision: %w", application.ErrNotFound)
		}
		user = toDomainUser(row)

		if write.Account != nil {
			accountName, operatorID := write.Account.AccountName, write.Account.OperatorID
			accountRow := accountModel{
				ID: write.Account.AccountID, TenantID: write.Account.TenantID, UserID: &write.Account.UserID,
				Username: &accountName, AccountType: "HUMAN", AuthSource: "LOCAL", Status: domain.StatusActive,
				ValidUntil: copyTime(write.Account.ValidUntil), Version: 1, CreatedAt: write.Account.OccurredAt,
				CreatedBy: &operatorID, UpdatedAt: write.Account.OccurredAt, UpdatedBy: &operatorID,
			}
			if err := transaction.Create(&accountRow).Error; err != nil {
				return mapWriteError(err, "create employee local account")
			}
			if err := transaction.Create(&passwordCredentialModel{
				ID: write.Account.CredentialID, AccountID: write.Account.AccountID, PasswordHash: append([]byte(nil), write.Account.PasswordDigest...),
				HashAlgorithm: "argon2id", AlgorithmParams: append([]byte(nil), write.Account.AlgorithmParams...), MustChange: true,
				FailedAttempts: 0, Status: domain.StatusActive, PasswordChangedAt: write.Account.OccurredAt,
				CreatedAt: write.Account.OccurredAt, UpdatedAt: write.Account.OccurredAt,
			}).Error; err != nil {
				return mapWriteError(err, "create employee local account credential")
			}
			created := domain.Account{ID: write.Account.AccountID, TenantID: write.Account.TenantID, UserID: &write.Account.UserID, AccountName: write.Account.AccountName, Status: domain.StatusActive, ValidUntil: copyTime(write.Account.ValidUntil), Version: 1, CreatedAt: write.Account.OccurredAt, UpdatedAt: write.Account.OccurredAt}
			account = &created
		}

		if write.Membership != nil {
			if err := ensureMembershipReferences(ctx, transaction, *write.Membership); err != nil {
				return err
			}
			isPrimary := write.Membership.MembershipType == domain.MembershipPrimary
			if isPrimary {
				if err := ensureNoOtherPrimary(ctx, transaction, write.Membership.TenantID, write.Membership.UserID, ""); err != nil {
					return err
				}
			}
			if err := transaction.Create(&membershipModel{
				ID: write.MembershipID, TenantID: write.Membership.TenantID, UserID: write.Membership.UserID,
				OrgUnitID: write.Membership.OrgUnitID, PositionID: write.Membership.PositionID,
				MembershipType: write.Membership.MembershipType, IsPrimary: isPrimary,
				InheritAuthorization: write.Membership.InheritAuthorization == nil || *write.Membership.InheritAuthorization,
				ValidFrom:            nullableTime(write.Membership.EffectiveFrom), ValidUntil: nullableTime(write.Membership.EffectiveTo),
				Status: domain.StatusActive, Version: 1, CreatedAt: now, CreatedBy: nullableString(&write.Membership.OperatorID), UpdatedAt: now, UpdatedBy: nullableString(&write.Membership.OperatorID),
			}).Error; err != nil {
				return mapWriteError(err, "create employee membership")
			}
			if isPrimary {
				result = transaction.Model(&userModel{}).Where("id = ? AND tenant_id = ?", write.Membership.UserID, write.Membership.TenantID).Updates(map[string]any{
					"primary_org_id": write.Membership.OrgUnitID, "updated_at": now, "updated_by": write.Membership.OperatorID, "version": gorm.Expr("version + 1"),
				})
				if result.Error != nil {
					return fmt.Errorf("set employee primary organization: %w", result.Error)
				}
			}
			if err := advanceMembershipAuthorizationRevisions(transaction, write.Membership.TenantID, now, "新增员工任职关系导致组织/岗位继承授权变化"); err != nil {
				return err
			}
			// 任职与授权 revision 同事务提交，子系统不会观察到“任职已存在但仍沿用旧授权
			// 快照”的中间状态。
			membershipID = write.MembershipID
		}
		eventType := "IDENTITY_CHANGED"
		if write.Membership != nil {
			eventType = "ORGANIZATION_CHANGED"
		}
		if err := enqueueKeycloakIdentityEvents(transaction, write.User.TenantID, []string{write.User.ID}, now, eventType); err != nil {
			return err
		}
		return nil
	})
	if err != nil {
		return domain.User{}, nil, nil, err
	}
	if membershipID == "" {
		return user, account, nil, nil
	}
	membership, err := repository.getMembership(ctx, write.User.TenantID, membershipID)
	if err != nil {
		return domain.User{}, nil, nil, err
	}
	return user, account, &membership, nil
}

// CreateEmployees persists a complete employee batch in one transaction, including local
// accounts and credentials generated by the application layer.
func (repository *GORMRepository) CreateEmployees(ctx context.Context, writes []application.EmployeeOnboardingWrite) ([]domain.User, error) {
	if len(writes) == 0 {
		return nil, application.ErrValidation
	}
	tenantID := writes[0].User.TenantID
	for _, write := range writes {
		if write.User.TenantID != tenantID || write.User.ID == "" || write.User.RoleBindingID == "" || write.Account == nil || write.Membership == nil || write.MembershipID == "" || write.Account.UserID != write.User.ID || write.Account.TenantID != tenantID {
			return nil, application.ErrValidation
		}
	}
	users := make([]domain.User, 0, len(writes))
	err := repository.database.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var platform bootstrapApplicationModel
		if result := tx.Where("tenant_id = ? AND code = ? AND status = ?", tenantID, application.DefaultPlatformApplicationCode, domain.StatusActive).First(&platform); result.Error != nil {
			if errors.Is(result.Error, gorm.ErrRecordNotFound) {
				return fmt.Errorf("resolve platform application: %w", application.ErrNotFound)
			}
			return result.Error
		}
		var baseline bootstrapRoleModel
		if result := tx.Where("tenant_id = ? AND application_id = ? AND code = ? AND status = ?", tenantID, platform.ID, application.DefaultUserRoleCode, domain.StatusActive).First(&baseline); result.Error != nil {
			if errors.Is(result.Error, gorm.ErrRecordNotFound) {
				return fmt.Errorf("resolve ordinary-user role: %w", application.ErrNotFound)
			}
			return result.Error
		}
		userWrites := make([]application.UserWrite, 0, len(writes))
		for _, write := range writes {
			userWrites = append(userWrites, write.User)
		}
		resolved, err := resolveImportedApplicationRoles(tx, tenantID, userWrites)
		if err != nil {
			return err
		}
		// Check the tenant-wide unique username constraint before inserting any row. This
		// produces a deterministic conflict for both duplicate names inside the import and
		// names already used by existing accounts, instead of relying on a late SQL error
		// after earlier rows have done work in the transaction.
		accountNames := make([]string, 0, len(writes))
		seenAccountNames := make(map[string]struct{}, len(writes))
		for _, write := range writes {
			name := write.Account.AccountName
			if _, exists := seenAccountNames[name]; exists {
				return application.ErrConflict
			}
			seenAccountNames[name] = struct{}{}
			accountNames = append(accountNames, name)
		}
		var existingAccounts int64
		if result := tx.Model(&accountModel{}).Where("tenant_id = ? AND username IN ?", tenantID, accountNames).Count(&existingAccounts); result.Error != nil {
			return fmt.Errorf("check employee batch account names: %w", result.Error)
		} else if existingAccounts > 0 {
			return application.ErrConflict
		}
		now := time.Now().UTC()
		changed := map[string]struct{}{platform.ID: {}}
		for _, write := range writes {
			row := userModel{ID: write.User.ID, TenantID: tenantID, EmployeeNo: nullableString(write.User.EmployeeNo), DisplayName: write.User.DisplayName, Email: nullableString(write.User.Email), MobileCiphertext: nullableBytes(write.User.MobileCiphertext), MobileHash: nullableBytes(write.User.MobileHash), EmploymentStatus: "EMPLOYED", Status: write.User.Status, Version: 1, CreatedAt: now, CreatedBy: nullableString(&write.User.OperatorID), UpdatedAt: now, UpdatedBy: nullableString(&write.User.OperatorID)}
			if err := tx.Create(&row).Error; err != nil {
				return mapWriteError(err, "create employee batch user")
			}
			// The membership reference check joins iam_user. Insert the new user first;
			// the surrounding transaction still rolls back the row if the organization,
			// position or tenant relationship is invalid.
			if err := ensureMembershipReferences(ctx, tx, *write.Membership); err != nil {
				return fmt.Errorf("validate employee batch membership references: %w", err)
			}
			accountName, operatorID := write.Account.AccountName, write.Account.OperatorID
			accountRow := accountModel{ID: write.Account.AccountID, TenantID: tenantID, UserID: &write.Account.UserID, Username: &accountName, AccountType: "HUMAN", AuthSource: "LOCAL", Status: domain.StatusActive, ValidUntil: copyTime(write.Account.ValidUntil), Version: 1, CreatedAt: now, CreatedBy: &operatorID, UpdatedAt: now, UpdatedBy: &operatorID}
			if err := tx.Create(&accountRow).Error; err != nil {
				return mapWriteError(err, "create employee batch local account")
			}
			if err := tx.Create(&passwordCredentialModel{ID: write.Account.CredentialID, AccountID: write.Account.AccountID, PasswordHash: append([]byte(nil), write.Account.PasswordDigest...), HashAlgorithm: "argon2id", AlgorithmParams: append([]byte(nil), write.Account.AlgorithmParams...), MustChange: true, FailedAttempts: 0, Status: domain.StatusActive, PasswordChangedAt: now, CreatedAt: now, UpdatedAt: now}).Error; err != nil {
				return mapWriteError(err, "create employee batch password credential")
			}
			if err := tx.Create(&bootstrapRoleBindingModel{ID: write.User.RoleBindingID, TenantID: tenantID, ApplicationID: platform.ID, RoleID: baseline.ID, SubjectType: "USER", SubjectID: write.User.ID, ScopeType: "TENANT", Status: domain.StatusActive, GrantOrigin: "SYSTEM", Version: 1, CreatedAt: now, CreatedBy: nullableString(&write.User.OperatorID), UpdatedAt: now, UpdatedBy: nullableString(&write.User.OperatorID)}).Error; err != nil {
				return mapWriteError(err, "bind employee batch ordinary-user role")
			}
			for _, imported := range write.User.ApplicationRoleBindings {
				target, ok := resolved[importedApplicationRoleKey(imported)]
				if !ok {
					return application.ErrValidation
				}
				if err := tx.Create(&bootstrapRoleBindingModel{ID: imported.ID, TenantID: tenantID, ApplicationID: target.ApplicationID, RoleID: target.RoleID, SubjectType: "USER", SubjectID: write.User.ID, ScopeType: "TENANT", Status: domain.StatusActive, GrantOrigin: "MANUAL", Version: 1, CreatedAt: now, CreatedBy: nullableString(&write.User.OperatorID), UpdatedAt: now, UpdatedBy: nullableString(&write.User.OperatorID)}).Error; err != nil {
					return mapWriteError(err, "bind employee batch application role")
				}
				changed[target.ApplicationID] = struct{}{}
			}
			isPrimary := write.Membership.MembershipType == domain.MembershipPrimary
			if isPrimary {
				if err := ensureNoOtherPrimary(ctx, tx, tenantID, write.User.ID, ""); err != nil {
					return err
				}
			}
			if err := tx.Create(&membershipModel{ID: write.MembershipID, TenantID: tenantID, UserID: write.User.ID, OrgUnitID: write.Membership.OrgUnitID, PositionID: write.Membership.PositionID, MembershipType: write.Membership.MembershipType, IsPrimary: isPrimary, InheritAuthorization: write.Membership.InheritAuthorization == nil || *write.Membership.InheritAuthorization, ValidFrom: nullableTime(write.Membership.EffectiveFrom), ValidUntil: nullableTime(write.Membership.EffectiveTo), Status: domain.StatusActive, Version: 1, CreatedAt: now, CreatedBy: nullableString(&write.User.OperatorID), UpdatedAt: now, UpdatedBy: nullableString(&write.User.OperatorID)}).Error; err != nil {
				return mapWriteError(err, "create employee batch membership")
			}
			if isPrimary {
				if err := tx.Model(&userModel{}).Where("id = ? AND tenant_id = ?", write.User.ID, tenantID).Updates(map[string]any{"primary_org_id": write.Membership.OrgUnitID, "updated_at": now, "updated_by": write.User.OperatorID, "version": gorm.Expr("version + 1")}).Error; err != nil {
					return err
				}
			}
			users = append(users, toDomainUser(row))
		}
		for appID := range changed {
			result := tx.Model(&identityPolicyRevisionModel{}).Where("tenant_id = ? AND application_id = ?", tenantID, appID).Updates(map[string]any{"revision": gorm.Expr("revision + 1"), "changed_at": now, "change_reason": "批量导入员工并绑定角色"})
			if result.Error != nil || result.RowsAffected == 0 {
				if result.Error != nil {
					return result.Error
				}
				return application.ErrNotFound
			}
		}
		if err := advanceMembershipAuthorizationRevisions(tx, tenantID, now, "批量导入员工任职关系导致组织/岗位继承授权变化"); err != nil {
			return err
		}
		ids := make([]string, 0, len(writes))
		for _, write := range writes {
			ids = append(ids, write.User.ID)
		}
		return enqueueKeycloakIdentityEvents(tx, tenantID, ids, now, "ORGANIZATION_CHANGED")
	})
	if err != nil {
		return nil, err
	}
	return users, nil
}
