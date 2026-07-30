package infrastructure

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/J-S-Te/Basic-Platform/backend/internal/platform/identity/application"
	"github.com/J-S-Te/Basic-Platform/backend/internal/platform/identity/domain"
	"gorm.io/gorm"
)

// CreateEmployee persists the complete employee onboarding aggregate in one database transaction.
// It intentionally does not call the public CreateUsers/CreateLocalAccount/CreateMembership methods:
// each owns its own transaction and would reintroduce partial-success states.
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
				HashAlgorithm: "argon2id", AlgorithmParams: append([]byte(nil), write.Account.AlgorithmParams...), MustChange: false,
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
			membershipID = write.MembershipID
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
