package infrastructure

import (
	"context"
	"errors"
	"fmt"

	"github.com/J-S-Te/Basic-Platform/internal/platform/identity/application"
	"github.com/J-S-Te/Basic-Platform/internal/platform/identity/domain"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// BootstrapFirstSuperAdmin performs the migration-owned, one-time IAM initialization in a single
// database transaction. The default tenant row is locked so concurrent bootstrap attempts cannot
// create multiple administrators before the state marker is committed.
func (repository *GORMRepository) BootstrapFirstSuperAdmin(ctx context.Context, write application.BootstrapWrite) (application.BootstrapResult, error) {
	var output application.BootstrapResult

	err := repository.database.WithContext(ctx).Transaction(func(transaction *gorm.DB) error {
		var tenant tenantModel
		result := transaction.Clauses(clause.Locking{Strength: "UPDATE"}).
			Where("code = ? AND status = ?", application.BootstrapTenantCode, domain.StatusActive).
			First(&tenant)
		if errors.Is(result.Error, gorm.ErrRecordNotFound) {
			return application.ErrBootstrapUnavailable
		}
		if result.Error != nil {
			return fmt.Errorf("lock bootstrap tenant: %w", result.Error)
		}

		var state bootstrapStateModel
		result = transaction.Where("tenant_id = ?", tenant.ID).First(&state)
		if result.Error == nil {
			return application.ErrBootstrapAlreadyInitialized
		}
		if !errors.Is(result.Error, gorm.ErrRecordNotFound) {
			return fmt.Errorf("check bootstrap state: %w", result.Error)
		}

		var platformApplication bootstrapApplicationModel
		result = transaction.Where("tenant_id = ? AND code = ? AND status = ?", tenant.ID, application.BootstrapApplicationCode, domain.StatusActive).
			First(&platformApplication)
		if errors.Is(result.Error, gorm.ErrRecordNotFound) {
			return application.ErrBootstrapUnavailable
		}
		if result.Error != nil {
			return fmt.Errorf("get bootstrap application: %w", result.Error)
		}

		var superAdminRole bootstrapRoleModel
		result = transaction.Where(
			"tenant_id = ? AND application_id = ? AND code = ? AND status = ?",
			tenant.ID,
			platformApplication.ID,
			application.BootstrapSuperAdminRoleCode,
			domain.StatusActive,
		).First(&superAdminRole)
		if errors.Is(result.Error, gorm.ErrRecordNotFound) {
			return application.ErrBootstrapUnavailable
		}
		if result.Error != nil {
			return fmt.Errorf("get bootstrap super-admin role: %w", result.Error)
		}

		initializedAt := write.InitializedAt.UTC()
		userID := write.UserID
		accountName := write.AccountName
		if err := transaction.Create(&userModel{
			ID:               write.UserID,
			TenantID:         tenant.ID,
			DisplayName:      write.DisplayName,
			EmploymentStatus: domain.StatusActive,
			Status:           domain.StatusActive,
			Version:          1,
			CreatedAt:        initializedAt,
			UpdatedAt:        initializedAt,
		}).Error; err != nil {
			return mapWriteError(err, "create bootstrap user")
		}
		if err := transaction.Create(&accountModel{
			ID:          write.AccountID,
			TenantID:    tenant.ID,
			UserID:      &userID,
			Username:    &accountName,
			AccountType: "HUMAN",
			AuthSource:  "LOCAL",
			Status:      domain.StatusActive,
			Version:     1,
			CreatedAt:   initializedAt,
			UpdatedAt:   initializedAt,
		}).Error; err != nil {
			return mapWriteError(err, "create bootstrap account")
		}
		if err := transaction.Create(&passwordCredentialModel{
			ID:                write.CredentialID,
			AccountID:         write.AccountID,
			PasswordHash:      append([]byte(nil), write.PasswordDigest...),
			HashAlgorithm:     "argon2id",
			AlgorithmParams:   append([]byte(nil), write.AlgorithmParams...),
			MustChange:        false,
			FailedAttempts:    0,
			Status:            domain.StatusActive,
			PasswordChangedAt: initializedAt,
			CreatedAt:         initializedAt,
			UpdatedAt:         initializedAt,
		}).Error; err != nil {
			return mapWriteError(err, "create bootstrap password credential")
		}
		if err := transaction.Create(&bootstrapRoleBindingModel{
			ID:            write.RoleBindingID,
			TenantID:      tenant.ID,
			ApplicationID: platformApplication.ID,
			RoleID:        superAdminRole.ID,
			SubjectType:   "USER",
			SubjectID:     write.UserID,
			ScopeType:     "TENANT",
			ScopeID:       "",
			Status:        domain.StatusActive,
			Version:       1,
			CreatedAt:     initializedAt,
			UpdatedAt:     initializedAt,
		}).Error; err != nil {
			return mapWriteError(err, "create bootstrap role binding")
		}
		if err := transaction.Create(&bootstrapStateModel{
			ID:                       write.BootstrapID,
			TenantID:                 tenant.ID,
			FirstSuperAdminUserID:    write.UserID,
			FirstSuperAdminAccountID: write.AccountID,
			InitializedAt:            initializedAt,
		}).Error; err != nil {
			return mapWriteError(err, "create bootstrap state")
		}
		if err := enqueueKeycloakIdentityEvents(transaction, tenant.ID, []string{write.UserID}, initializedAt, "IDENTITY_CHANGED"); err != nil {
			return err
		}

		output = application.BootstrapResult{
			BootstrapID:   write.BootstrapID,
			TenantID:      tenant.ID,
			TenantCode:    tenant.Code,
			UserID:        write.UserID,
			DisplayName:   write.DisplayName,
			AccountID:     write.AccountID,
			AccountName:   write.AccountName,
			RoleID:        superAdminRole.ID,
			RoleCode:      superAdminRole.Code,
			RoleName:      superAdminRole.Name,
			InitializedAt: initializedAt,
		}
		return nil
	})
	if errors.Is(err, gorm.ErrDuplicatedKey) {
		return application.BootstrapResult{}, application.ErrConflict
	}
	return output, err
}

// FirstSuperAdminInitialized reports whether the one-time bootstrap state has already been
// committed for the migration-seeded default tenant. It does not expose any account data.
func (repository *GORMRepository) FirstSuperAdminInitialized(ctx context.Context) (bool, error) {
	var tenant tenantModel
	result := repository.database.WithContext(ctx).
		Select("id").
		Where("code = ?", application.BootstrapTenantCode).
		Limit(1).
		Find(&tenant)
	if result.Error != nil {
		return false, fmt.Errorf("find bootstrap tenant: %w", result.Error)
	}
	if result.RowsAffected == 0 {
		return false, application.ErrBootstrapUnavailable
	}

	var state bootstrapStateModel
	result = repository.database.WithContext(ctx).
		Select("id").
		Where("tenant_id = ?", tenant.ID).
		Limit(1).
		Find(&state)
	if result.Error != nil {
		return false, fmt.Errorf("check bootstrap state: %w", result.Error)
	}
	return result.RowsAffected > 0, nil
}
