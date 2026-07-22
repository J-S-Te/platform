package infrastructure

import (
	"time"

	"github.com/J-S-Te/Basic-Platform/backend/internal/platform/identity/domain"
)

func toDomainUser(model userModel) domain.User {
	return domain.User{
		ID:               model.ID,
		TenantID:         model.TenantID,
		EmployeeNo:       copyString(model.EmployeeNo),
		DisplayName:      model.DisplayName,
		Email:            copyString(model.Email),
		MobileCiphertext: append([]byte(nil), model.MobileCiphertext...),
		Status:           model.Status,
		Version:          model.Version,
		CreatedAt:        model.CreatedAt.UTC(),
		UpdatedAt:        model.UpdatedAt.UTC(),
	}
}

func toDomainAccount(model accountModel) domain.Account {
	return domain.Account{
		ID:          model.ID,
		TenantID:    model.TenantID,
		UserID:      copyString(model.UserID),
		AccountName: valueOrEmpty(model.Username),
		Status:      model.Status,
		LastLoginAt: copyTime(model.LastLoginAt),
		Version:     model.Version,
		CreatedAt:   model.CreatedAt.UTC(),
		UpdatedAt:   model.UpdatedAt.UTC(),
	}
}

func toDomainOrgUnit(model orgUnitModel) domain.OrgUnit {
	return domain.OrgUnit{
		ID:        model.ID,
		TenantID:  model.TenantID,
		ParentID:  copyString(model.ParentID),
		Code:      model.Code,
		Name:      model.Name,
		OrgType:   model.OrgType,
		Path:      model.Path,
		Depth:     model.Depth,
		SortOrder: model.SortOrder,
		Status:    model.Status,
		Version:   model.Version,
	}
}

func toDomainPosition(model positionModel) domain.Position {
	return domain.Position{
		ID:        model.ID,
		TenantID:  model.TenantID,
		OrgUnitID: model.OrgUnitID,
		Code:      model.Code,
		Name:      model.Name,
		Status:    model.Status,
		Version:   model.Version,
	}
}

func toDomainMembership(model membershipProjection) domain.Membership {
	return domain.Membership{
		ID:             model.ID,
		TenantID:       model.TenantID,
		User:           domain.ReferenceName{ID: model.UserID, Name: model.UserName},
		OrgUnit:        domain.ReferenceName{ID: model.OrgUnitID, Name: model.OrgUnitName},
		Position:       domain.ReferenceName{ID: model.PositionID, Name: model.PositionName},
		MembershipType: model.MembershipType,
		EffectiveFrom:  copyTime(model.ValidFrom),
		EffectiveTo:    copyTime(model.ValidUntil),
		Status:         model.Status,
		Version:        model.Version,
		IsPrimary:      model.IsPrimary,
	}
}

func toDomainLoginAccount(model loginAccountProjection) domain.LoginAccount {
	return domain.LoginAccount{
		TenantID:         model.TenantID,
		TenantName:       model.TenantName,
		TenantCode:       model.TenantCode,
		TenantStatus:     model.TenantStatus,
		UserID:           model.UserID,
		UserName:         model.UserName,
		UserStatus:       model.UserStatus,
		AccountID:        model.AccountID,
		AccountName:      model.AccountName,
		AccountStatus:    model.AccountStatus,
		LockedUntil:      copyTime(model.LockedUntil),
		PasswordHash:     append([]byte(nil), model.PasswordHash...),
		HashAlgorithm:    model.HashAlgorithm,
		AlgorithmParams:  append([]byte(nil), model.AlgorithmParams...),
		CredentialStatus: model.CredentialStatus,
		CredentialExpiry: copyTime(model.CredentialExpiry),
	}
}

func copyString(value *string) *string {
	if value == nil {
		return nil
	}
	copied := *value
	return &copied
}

func copyTime(value *time.Time) *time.Time {
	if value == nil {
		return nil
	}
	copied := value.UTC()
	return &copied
}

func valueOrEmpty(value *string) string {
	if value == nil {
		return ""
	}
	return *value
}

func nullableString(value *string) *string {
	if value == nil || *value == "" {
		return nil
	}
	return value
}

func nullableBytes(value []byte) []byte {
	if len(value) == 0 {
		return nil
	}
	return value
}

func nullableTime(value *time.Time) *time.Time {
	if value == nil {
		return nil
	}
	utc := value.UTC()
	return &utc
}
