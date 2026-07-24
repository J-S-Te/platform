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

// ListUsers reads tenant-scoped users and leaves mobile masking to the application layer.
func (repository *GORMRepository) ListUsers(ctx context.Context, tenantID string, query application.PageRequest) (application.PageResult[domain.User], error) {
	database := applyUserFilter(repository.database.WithContext(ctx).Model(&userModel{}), tenantID, query)
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

// CreateUser persists a natural person but intentionally does not create a login account.
func (repository *GORMRepository) CreateUser(ctx context.Context, write application.UserWrite) (domain.User, error) {
	now := time.Now().UTC()
	if err := repository.database.WithContext(ctx).Create(&userModel{
		ID:               write.ID,
		TenantID:         write.TenantID,
		EmployeeNo:       nullableString(write.EmployeeNo),
		DisplayName:      write.DisplayName,
		Email:            nullableString(write.Email),
		MobileCiphertext: nullableBytes(write.MobileCiphertext),
		MobileHash:       nullableBytes(write.MobileHash),
		EmploymentStatus: "EMPLOYED",
		Status:           write.Status,
		Version:          1,
		CreatedAt:        now,
		CreatedBy:        nullableString(&write.OperatorID),
		UpdatedAt:        now,
		UpdatedBy:        nullableString(&write.OperatorID),
	}).Error; err != nil {
		return domain.User{}, mapWriteError(err, "create user")
	}
	return repository.GetUser(ctx, write.TenantID, write.ID)
}

// GetUser retrieves exactly one tenant-scoped user.
func (repository *GORMRepository) GetUser(ctx context.Context, tenantID, userID string) (domain.User, error) {
	var row userModel
	result := repository.database.WithContext(ctx).Where("tenant_id = ? AND id = ?", tenantID, userID).First(&row)
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
	updates := map[string]any{
		"display_name": input.DisplayName,
		"updated_at":   time.Now().UTC(),
		"updated_by":   input.OperatorID,
		"version":      gorm.Expr("version + 1"),
	}
	if input.EmployeeNo != nil {
		updates["employee_no"] = nullableString(input.EmployeeNo)
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

	result := repository.database.WithContext(ctx).Model(&userModel{}).
		Where("tenant_id = ? AND id = ? AND version = ?", input.TenantID, input.UserID, input.Version).
		Updates(updates)
	if result.Error != nil {
		return domain.User{}, mapWriteError(result.Error, "update user")
	}
	if result.RowsAffected == 0 {
		return domain.User{}, repository.versionedUserError(ctx, input.TenantID, input.UserID)
	}
	return repository.GetUser(ctx, input.TenantID, input.UserID)
}

func (repository *GORMRepository) versionedUserError(ctx context.Context, tenantID, userID string) error {
	var total int64
	if err := repository.database.WithContext(ctx).Model(&userModel{}).Where("tenant_id = ? AND id = ?", tenantID, userID).Count(&total).Error; err != nil {
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
	if err := database.Order("created_at DESC, id DESC").Limit(query.PageSize).Offset(pageOffset(query)).Find(&rows).Error; err != nil {
		return application.PageResult[domain.Account]{}, fmt.Errorf("list accounts: %w", err)
	}
	items := make([]domain.Account, 0, len(rows))
	for _, row := range rows {
		items = append(items, toAccountWithFallbackName(row))
	}
	return application.PageResult[domain.Account]{Items: items, Page: query.Page, PageSize: query.PageSize, Total: total}, nil
}

func (repository *GORMRepository) UpdateAccount(ctx context.Context, input application.AccountUpdateInput) (domain.Account, error) {
	result := repository.database.WithContext(ctx).Model(&accountModel{}).
		Where("tenant_id = ? AND id = ? AND version = ?", input.TenantID, input.AccountID, input.Version).
		Updates(map[string]any{
			"status":     input.Status,
			"updated_at": time.Now().UTC(),
			"updated_by": input.OperatorID,
			"version":    gorm.Expr("version + 1"),
		})
	if result.Error != nil {
		return domain.Account{}, mapWriteError(result.Error, "update account")
	}
	if result.RowsAffected == 0 {
		return domain.Account{}, repository.versionedAccountError(ctx, input.TenantID, input.AccountID)
	}
	return repository.getAccount(ctx, input.TenantID, input.AccountID)
}

func (repository *GORMRepository) versionedAccountError(ctx context.Context, tenantID, accountID string) error {
	var total int64
	if err := repository.database.WithContext(ctx).Model(&accountModel{}).Where("tenant_id = ? AND id = ?", tenantID, accountID).Count(&total).Error; err != nil {
		return fmt.Errorf("check account after update: %w", err)
	}
	if total == 0 {
		return application.ErrNotFound
	}
	return application.ErrVersionConflict
}

func (repository *GORMRepository) getAccount(ctx context.Context, tenantID, accountID string) (domain.Account, error) {
	var row accountModel
	result := repository.database.WithContext(ctx).Where("tenant_id = ? AND id = ?", tenantID, accountID).First(&row)
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

func (repository *GORMRepository) ListPositions(ctx context.Context, tenantID, keyword, status string, query application.PageRequest) (application.PageResult[domain.Position], error) {
	database := applyPositionFilter(repository.database.WithContext(ctx).Model(&positionModel{}), tenantID, keyword, status)
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

func (repository *GORMRepository) ListMemberships(ctx context.Context, tenantID string, query application.PageRequest) (application.PageResult[domain.Membership], error) {
	database := applyMembershipFilter(repository.membershipQuery(ctx), tenantID, query)
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
		if err := ensureMembershipReferences(ctx, transaction, input); err != nil {
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
			ID:             id,
			TenantID:       input.TenantID,
			UserID:         input.UserID,
			OrgUnitID:      input.OrgUnitID,
			PositionID:     input.PositionID,
			MembershipType: input.MembershipType,
			IsPrimary:      isPrimary,
			ValidFrom:      nullableTime(input.EffectiveFrom),
			ValidUntil:     nullableTime(input.EffectiveTo),
			Status:         domain.StatusActive,
			Version:        1,
			CreatedAt:      now,
			CreatedBy:      nullableString(&input.OperatorID),
			UpdatedAt:      now,
			UpdatedBy:      nullableString(&input.OperatorID),
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
		return nil
	})
	if err != nil {
		return domain.Membership{}, err
	}
	return repository.getMembership(ctx, input.TenantID, id)
}

func (repository *GORMRepository) UpdateMembership(ctx context.Context, input application.MembershipUpdateInput) (domain.Membership, error) {
	err := repository.database.WithContext(ctx).Transaction(func(transaction *gorm.DB) error {
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
		result = transaction.Model(&membershipModel{}).
			Where("tenant_id = ? AND id = ? AND version = ?", input.TenantID, input.MembershipID, input.Version).
			Updates(map[string]any{
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
			})
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
		return nil
	})
	if err != nil {
		return domain.Membership{}, err
	}
	return repository.getMembership(ctx, input.TenantID, input.MembershipID)
}

const membershipSelectColumns = `m.id, m.tenant_id, u.id AS user_id, u.display_name AS user_name,
	o.id AS org_unit_id, o.name AS org_unit_name, p.id AS position_id, p.name AS position_name,
	m.membership_type, m.valid_from, m.valid_until, m.status, m.version, m.is_primary`

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

func applyUserFilter(database *gorm.DB, tenantID string, query application.PageRequest) *gorm.DB {
	database = database.Where("tenant_id = ?", tenantID)
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
	database = database.Where("tenant_id = ?", tenantID)
	if query.Keyword != "" {
		database = database.Where("username LIKE ?", "%"+query.Keyword+"%")
	}
	if query.Status != "" {
		database = database.Where("status = ?", query.Status)
	}
	return database
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
