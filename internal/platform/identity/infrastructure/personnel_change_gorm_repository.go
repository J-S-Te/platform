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
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

type personnelChangeModel struct {
	ID                   string     `gorm:"column:id;primaryKey"`
	TenantID             string     `gorm:"column:tenant_id"`
	UserID               string     `gorm:"column:user_id"`
	SourceMembershipID   *string    `gorm:"column:source_membership_id"`
	TargetOrgUnitID      *string    `gorm:"column:target_org_unit_id"`
	TargetPositionID     *string    `gorm:"column:target_position_id"`
	ChangeType           string     `gorm:"column:change_type"`
	Status               string     `gorm:"column:status"`
	Reason               string     `gorm:"column:reason"`
	ApprovalReference    *string    `gorm:"column:approval_reference"`
	HandoverReference    *string    `gorm:"column:handover_reference"`
	RejectionReason      *string    `gorm:"column:rejection_reason"`
	EffectiveAt          time.Time  `gorm:"column:effective_at"`
	SubmittedBy          string     `gorm:"column:submitted_by"`
	ApprovedBy           *string    `gorm:"column:approved_by"`
	ApprovedAt           *time.Time `gorm:"column:approved_at"`
	ExecutedAt           *time.Time `gorm:"column:executed_at"`
	CancelledAt          *time.Time `gorm:"column:cancelled_at"`
	UserDisplayName      string     `gorm:"column:user_display_name;->"`
	SourceOrganization   string     `gorm:"column:source_organization_name;->"`
	SourcePosition       string     `gorm:"column:source_position_name;->"`
	TargetOrganization   string     `gorm:"column:target_organization_name;->"`
	TargetPosition       string     `gorm:"column:target_position_name;->"`
	Version              uint64     `gorm:"column:version"`
	CreatedAt, UpdatedAt time.Time
}

type personnelChangeTransitionModel struct {
	ID             uint64    `gorm:"column:id;primaryKey;autoIncrement"`
	TenantID       string    `gorm:"column:tenant_id"`
	RequestID      string    `gorm:"column:request_id"`
	FromStatus     string    `gorm:"column:from_status"`
	ToStatus       string    `gorm:"column:to_status"`
	OperatorID     string    `gorm:"column:operator_id"`
	ReferenceValue *string   `gorm:"column:reference_value"`
	CreatedAt      time.Time `gorm:"column:created_at"`
}

func (personnelChangeTransitionModel) TableName() string { return "iam_personnel_change_transition" }

type personnelHandoverItemModel struct {
	ID, TenantID, RequestID, SystemCode, ResourceType, ResourceID, CurrentOwnerID, Status string
	CreatedAt, UpdatedAt                                                                  time.Time
}

func (personnelHandoverItemModel) TableName() string { return "iam_personnel_handover_item" }

// Execute applies an approved change atomically. Position changes close the old
// appointment and create a new one; termination disables all user access.
func (r *PersonnelChangeGORMRepository) Execute(c context.Context, req application.PersonnelChangeRequest, operator string, now time.Time) (application.PersonnelChangeRequest, error) {
	err := r.db.WithContext(c).Transaction(func(tx *gorm.DB) error {
		// Serialize execution for the request. Multiple worker replicas may observe the
		// same due row; locking and re-checking the status makes the second execution a
		// no-op before it can mutate memberships or accounts.
		var locked personnelChangeModel
		lockResult := tx.Clauses(clause.Locking{Strength: "UPDATE"}).
			Where("tenant_id = ? AND id = ? AND status = ? AND version = ?", req.TenantID, req.ID, domain.PersonnelChangeScheduled, req.Version).
			First(&locked)
		if errors.Is(lockResult.Error, gorm.ErrRecordNotFound) {
			return application.ErrConflict
		}
		if lockResult.Error != nil {
			return fmt.Errorf("lock personnel change for execution: %w", lockResult.Error)
		}
		// 后续业务变更只使用锁内重新读取的快照，防止调用方携带的陈旧字段
		// 与数据库当前请求内容分叉。
		req = toPersonnel(locked)
		if req.ChangeType == domain.PersonnelChangeTermination {
			var accountIDs []string
			if err := tx.Model(&accountModel{}).Where("tenant_id = ? AND user_id = ?", req.TenantID, req.UserID).Pluck("id", &accountIDs).Error; err != nil {
				return fmt.Errorf("list accounts before personnel termination: %w", err)
			}
			identityRepository := &GORMRepository{database: tx}
			for _, accountID := range accountIDs {
				// Revoke platform sessions, authorization codes, refresh families and
				// enqueue RP back-channel logout before disabling the account. Accounts
				// without an active session are a valid termination case.
				if err := identityRepository.RevokeAccountSessions(c, req.TenantID, accountID, now, "PERSONNEL_TERMINATION"); err != nil && !errors.Is(err, application.ErrUnauthenticated) {
					return fmt.Errorf("revoke account sessions during personnel termination: %w", err)
				}
			}
			if err := tx.Model(&membershipModel{}).Where("tenant_id = ? AND user_id = ? AND status <> ?", req.TenantID, req.UserID, domain.StatusDisabled).Updates(map[string]any{"status": domain.StatusDisabled, "is_primary": false, "valid_until": now, "updated_at": now, "updated_by": operator, "version": gorm.Expr("version + 1")}).Error; err != nil {
				return err
			}
			if err := tx.Model(&accountModel{}).Where("tenant_id = ? AND user_id = ?", req.TenantID, req.UserID).Updates(map[string]any{"status": domain.StatusDisabled, "updated_at": now, "updated_by": operator, "version": gorm.Expr("version + 1")}).Error; err != nil {
				return err
			}
			if err := tx.Model(&userModel{}).Where("tenant_id = ? AND id = ?", req.TenantID, req.UserID).Updates(map[string]any{"status": domain.StatusDisabled, "employment_status": "TERMINATED", "primary_org_id": nil, "updated_at": now, "updated_by": operator, "version": gorm.Expr("version + 1")}).Error; err != nil {
				return err
			}
		} else {
			if req.TargetOrgUnitID == "" || req.TargetPositionID == "" {
				return application.ErrValidation
			}
			// 目标组织和岗位必须是同一租户下的有效组合，防止客户端把 A 组织与 B 组织岗位拼接后写入任职。
			var targetPosition positionModel
			if err := tx.Where("tenant_id = ? AND id = ? AND org_unit_id = ? AND status = ?", req.TenantID, req.TargetPositionID, req.TargetOrgUnitID, domain.StatusActive).First(&targetPosition).Error; err != nil {
				return application.ErrValidation
			}
			var targetOrganization orgUnitModel
			if err := tx.Where("tenant_id = ? AND id = ? AND status = ?", req.TenantID, req.TargetOrgUnitID, domain.StatusActive).First(&targetOrganization).Error; err != nil {
				return application.ErrValidation
			}
			var source membershipModel
			if req.ChangeType == domain.PersonnelChangeRehire {
				source.MembershipType = "PRIMARY"
				source.IsPrimary = true
				source.InheritAuthorization = true
			} else {
				q := tx.Where("tenant_id = ? AND user_id = ? AND status = ?", req.TenantID, req.UserID, domain.StatusActive)
				if req.SourceMembershipID != "" {
					q = tx.Where("tenant_id = ? AND id = ? AND user_id = ? AND status = ?", req.TenantID, req.SourceMembershipID, req.UserID, domain.StatusActive)
				}
				if err := q.First(&source).Error; err != nil {
					return err
				}
			}
			if req.ChangeType != domain.PersonnelChangeRehire {
				sourceUpdate := tx.Model(&membershipModel{}).
					Where("tenant_id = ? AND id = ? AND version = ?", req.TenantID, source.ID, source.Version).
					Updates(map[string]any{"status": domain.StatusDisabled, "is_primary": false, "valid_until": now, "updated_at": now, "updated_by": operator, "version": gorm.Expr("version + 1")})
				if sourceUpdate.Error != nil {
					return sourceUpdate.Error
				}
				if sourceUpdate.RowsAffected != 1 {
					return application.ErrConflict
				}
			}
			if err := tx.Create(&membershipModel{ID: req.ID, TenantID: req.TenantID, UserID: req.UserID, OrgUnitID: req.TargetOrgUnitID, PositionID: req.TargetPositionID, MembershipType: source.MembershipType, IsPrimary: source.IsPrimary, InheritAuthorization: source.InheritAuthorization, ValidFrom: &now, Status: domain.StatusActive, Version: 1, CreatedAt: now, CreatedBy: &operator, UpdatedAt: now, UpdatedBy: &operator}).Error; err != nil {
				return err
			}
			// 主任职跨组织变更时同步用户主组织；否则新的任职记录与 OIDC primary_org_id
			// 会分叉，下一次登录仍可能携带旧组织并导致授权目录校验失败。
			if source.IsPrimary {
				if err := tx.Model(&userModel{}).Where("tenant_id = ? AND id = ?", req.TenantID, req.UserID).Updates(map[string]any{"primary_org_id": req.TargetOrgUnitID, "updated_at": now, "updated_by": operator, "version": gorm.Expr("version + 1")}).Error; err != nil {
					return err
				}
			}
			if req.ChangeType == domain.PersonnelChangeRehire {
				var account accountModel
				accountResult := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where("tenant_id = ? AND user_id = ? AND account_type = ? AND auth_source = ?", req.TenantID, req.UserID, "HUMAN", "LOCAL").Order("created_at DESC").First(&account)
				if errors.Is(accountResult.Error, gorm.ErrRecordNotFound) {
					return fmt.Errorf("rehire requires an existing local account: %w", application.ErrValidation)
				}
				if accountResult.Error != nil {
					return accountResult.Error
				}
				if err := tx.Model(&accountModel{}).Where("tenant_id = ? AND id = ?", req.TenantID, account.ID).Updates(map[string]any{"status": domain.StatusActive, "locked_until": nil, "valid_until": nil, "expiry_processed_at": nil, "updated_at": now, "updated_by": operator, "version": gorm.Expr("version + 1")}).Error; err != nil {
					return err
				}
				var credential passwordCredentialModel
				credentialResult := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where("account_id = ?", account.ID).First(&credential)
				if errors.Is(credentialResult.Error, gorm.ErrRecordNotFound) {
					return fmt.Errorf("rehire requires an initialized local credential: %w", application.ErrValidation)
				} else if credentialResult.Error != nil {
					return credentialResult.Error
				} else if err := tx.Model(&passwordCredentialModel{}).Where("id = ?", credential.ID).Updates(map[string]any{"must_change": true, "failed_attempts": 0, "last_failed_at": nil, "status": domain.StatusActive, "updated_at": now}).Error; err != nil {
					return err
				}
				if err := tx.Model(&sessionModel{}).Where("tenant_id = ? AND account_id = ? AND status = ? AND revoked_at IS NULL", req.TenantID, account.ID, domain.StatusActive).Updates(map[string]any{"revoked_at": now, "revoke_reason": "PERSONNEL_REHIRE", "status": "REVOKED"}).Error; err != nil {
					return err
				}
				if err := tx.Model(&userModel{}).Where("tenant_id = ? AND id = ?", req.TenantID, req.UserID).Updates(map[string]any{"status": domain.StatusActive, "employment_status": "EMPLOYED", "valid_until": nil, "expiry_processed_at": nil, "updated_at": now, "updated_by": operator, "version": gorm.Expr("version + 1")}).Error; err != nil {
					return err
				}
			}
		}
		if err := enqueueKeycloakIdentityEvents(tx, req.TenantID, []string{req.UserID}, now, "PERSONNEL_CHANGE_EXECUTED"); err != nil {
			return err
		}
		executionUpdate := tx.Model(&personnelChangeModel{}).
			Where("tenant_id = ? AND id = ? AND status = ? AND version = ?", req.TenantID, req.ID, domain.PersonnelChangeScheduled, req.Version).
			Updates(map[string]any{"status": domain.PersonnelChangeExecuted, "executed_at": now, "updated_at": now, "version": gorm.Expr("version + 1")})
		if executionUpdate.Error != nil {
			return executionUpdate.Error
		}
		if executionUpdate.RowsAffected != 1 {
			return application.ErrConflict
		}
		if err := tx.Create(&personnelChangeTransitionModel{TenantID: req.TenantID, RequestID: req.ID, FromStatus: domain.PersonnelChangeScheduled, ToStatus: domain.PersonnelChangeExecuted, OperatorID: operator, CreatedAt: now}).Error; err != nil {
			return fmt.Errorf("record personnel change execution: %w", err)
		}
		return nil
	})
	if err != nil {
		return application.PersonnelChangeRequest{}, err
	}
	result, err := r.Get(c, req.TenantID, req.ID)
	if err != nil {
		return application.PersonnelChangeRequest{}, err
	}
	return result, nil
}

func (personnelChangeModel) TableName() string { return "iam_personnel_change_request" }

type PersonnelChangeGORMRepository struct{ db *gorm.DB }

func NewPersonnelChangeGORMRepository(db *gorm.DB) *PersonnelChangeGORMRepository {
	return &PersonnelChangeGORMRepository{db: db}
}

// ValidateCreate moves all identity and assignment checks ahead of persistence.
// Execute repeats the critical checks as a defense against changes between scheduling and execution.
func (r *PersonnelChangeGORMRepository) ValidateCreate(c context.Context, in application.PersonnelChangeCreateInput) error {
	var user userModel
	if err := r.db.WithContext(c).Where("tenant_id = ? AND id = ? AND deleted_at IS NULL", in.TenantID, in.UserID).First(&user).Error; err != nil {
		return application.ErrValidation
	}
	if in.ChangeType == domain.PersonnelChangeRehire {
		if user.Status != domain.StatusDisabled {
			return fmt.Errorf("rehire requires a disabled user: %w", application.ErrValidation)
		}
		var initializedAccounts int64
		if err := r.db.WithContext(c).Table("iam_account AS account").
			Joins("JOIN iam_password_credential AS credential ON credential.account_id = account.id AND credential.status = ?", domain.StatusActive).
			Where("account.tenant_id = ? AND account.user_id = ? AND account.account_type = ? AND account.auth_source = ?", in.TenantID, in.UserID, "HUMAN", "LOCAL").
			Count(&initializedAccounts).Error; err != nil {
			return fmt.Errorf("check rehire account: %w", err)
		}
		if initializedAccounts == 0 {
			return fmt.Errorf("rehire requires an initialized local account: %w", application.ErrValidation)
		}
	} else if user.Status != domain.StatusActive {
		return fmt.Errorf("personnel change requires an active user: %w", application.ErrValidation)
	}
	var sourceMembership membershipModel
	if in.SourceMembershipID != "" {
		if err := r.db.WithContext(c).Where("tenant_id = ? AND id = ? AND user_id = ? AND status = ?", in.TenantID, in.SourceMembershipID, in.UserID, domain.StatusActive).First(&sourceMembership).Error; err != nil {
			return fmt.Errorf("source membership is not active for the selected user: %w", application.ErrValidation)
		}
	}
	if requiresCreateTarget(in.ChangeType) {
		var position positionModel
		if err := r.db.WithContext(c).Where("tenant_id = ? AND id = ? AND org_unit_id = ? AND status = ?", in.TenantID, in.TargetPositionID, in.TargetOrgUnitID, domain.StatusActive).First(&position).Error; err != nil {
			return fmt.Errorf("target position is not active in the selected organization: %w", application.ErrValidation)
		}
		var organization orgUnitModel
		if err := r.db.WithContext(c).Where("tenant_id = ? AND id = ? AND status = ?", in.TenantID, in.TargetOrgUnitID, domain.StatusActive).First(&organization).Error; err != nil {
			return fmt.Errorf("target organization is not active: %w", application.ErrValidation)
		}
		if in.ChangeType != domain.PersonnelChangeRehire && sourceMembership.ID != "" && sourceMembership.OrgUnitID == in.TargetOrgUnitID && sourceMembership.PositionID == in.TargetPositionID {
			return fmt.Errorf("target assignment must differ from the source assignment: %w", application.ErrValidation)
		}
	}
	var activeRequests int64
	if err := r.db.WithContext(c).Model(&personnelChangeModel{}).
		Where("tenant_id = ? AND user_id = ? AND status IN ?", in.TenantID, in.UserID, []string{domain.PersonnelChangeDraft, domain.PersonnelChangePendingApproval, domain.PersonnelChangePendingHandover, domain.PersonnelChangeScheduled}).
		Count(&activeRequests).Error; err != nil {
		return fmt.Errorf("check active personnel changes: %w", err)
	}
	if activeRequests > 0 {
		return fmt.Errorf("the selected user already has an active personnel change: %w", application.ErrConflict)
	}
	return nil
}

func requiresCreateTarget(changeType string) bool {
	return changeType != domain.PersonnelChangeTermination
}
func toPersonnel(m personnelChangeModel) application.PersonnelChangeRequest {
	return application.PersonnelChangeRequest{ID: m.ID, TenantID: m.TenantID, UserID: m.UserID, UserDisplayName: m.UserDisplayName, SourceMembershipID: deref(m.SourceMembershipID), SourceOrganization: m.SourceOrganization, SourcePosition: m.SourcePosition, TargetOrgUnitID: deref(m.TargetOrgUnitID), TargetOrganization: m.TargetOrganization, TargetPositionID: deref(m.TargetPositionID), TargetPosition: m.TargetPosition, ChangeType: m.ChangeType, Status: m.Status, Reason: m.Reason, ApprovalReference: deref(m.ApprovalReference), HandoverReference: deref(m.HandoverReference), RejectionReason: deref(m.RejectionReason), SubmittedBy: m.SubmittedBy, ApprovedBy: deref(m.ApprovedBy), EffectiveAt: &m.EffectiveAt, ApprovedAt: m.ApprovedAt, ExecutedAt: m.ExecutedAt, CancelledAt: m.CancelledAt, Version: m.Version, CreatedAt: m.CreatedAt, UpdatedAt: m.UpdatedAt}
}
func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}
func (r *PersonnelChangeGORMRepository) Create(c context.Context, v application.PersonnelChangeRequest) (application.PersonnelChangeRequest, error) {
	m := personnelChangeModel{ID: v.ID, TenantID: v.TenantID, UserID: v.UserID, ChangeType: v.ChangeType, Status: v.Status, Reason: v.Reason, EffectiveAt: *v.EffectiveAt, SubmittedBy: v.SubmittedBy, Version: v.Version, CreatedAt: v.CreatedAt, UpdatedAt: v.UpdatedAt}
	if v.SourceMembershipID != "" {
		x := v.SourceMembershipID
		m.SourceMembershipID = &x
	}
	if v.TargetOrgUnitID != "" {
		x := v.TargetOrgUnitID
		m.TargetOrgUnitID = &x
	}
	if v.TargetPositionID != "" {
		x := v.TargetPositionID
		m.TargetPositionID = &x
	}
	if v.ApprovalReference != "" {
		x := v.ApprovalReference
		m.ApprovalReference = &x
	}

	if err := r.db.WithContext(c).Transaction(func(tx *gorm.DB) error {
		if err := tx.Create(&m).Error; err != nil {
			return err
		}
		return tx.Create(&personnelChangeTransitionModel{TenantID: v.TenantID, RequestID: v.ID, FromStatus: "", ToStatus: v.Status, OperatorID: v.SubmittedBy, CreatedAt: v.CreatedAt}).Error
	}); err != nil {
		return application.PersonnelChangeRequest{}, err
	}
	return toPersonnel(m), nil
}
func (r *PersonnelChangeGORMRepository) List(c context.Context, t, status, changeType, keyword string) (out []application.PersonnelChangeRequest, err error) {
	q := r.db.WithContext(c).Table("iam_personnel_change_request AS pcr").
		Select(`pcr.*, user.display_name AS user_display_name,
			COALESCE(source_org.name, '') AS source_organization_name,
			COALESCE(source_position.name, '') AS source_position_name,
			COALESCE(target_org.name, '') AS target_organization_name,
			COALESCE(target_position.name, '') AS target_position_name`).
		Joins("JOIN iam_user AS user ON user.tenant_id = pcr.tenant_id AND user.id = pcr.user_id").
		Joins("LEFT JOIN iam_membership AS source_membership ON source_membership.tenant_id = pcr.tenant_id AND source_membership.id = pcr.source_membership_id").
		Joins("LEFT JOIN iam_org_unit AS source_org ON source_org.tenant_id = pcr.tenant_id AND source_org.id = source_membership.org_unit_id").
		Joins("LEFT JOIN iam_position AS source_position ON source_position.tenant_id = pcr.tenant_id AND source_position.id = source_membership.position_id").
		Joins("LEFT JOIN iam_org_unit AS target_org ON target_org.tenant_id = pcr.tenant_id AND target_org.id = pcr.target_org_unit_id").
		Joins("LEFT JOIN iam_position AS target_position ON target_position.tenant_id = pcr.tenant_id AND target_position.id = pcr.target_position_id")
	if t != "" {
		q = q.Where("pcr.tenant_id = ?", t)
	}
	if status != "" {
		q = q.Where("pcr.status = ?", status)
	}
	if changeType != "" {
		q = q.Where("pcr.change_type = ?", changeType)
	}
	if keyword != "" {
		like := "%" + keyword + "%"
		q = q.Where("pcr.user_id LIKE ? OR pcr.id LIKE ? OR pcr.approval_reference LIKE ? OR user.display_name LIKE ?", like, like, like, like)
	}
	var ms []personnelChangeModel
	err = q.Order("pcr.created_at DESC").Scan(&ms).Error
	for _, m := range ms {
		out = append(out, toPersonnel(m))
	}
	return
}
func (r *PersonnelChangeGORMRepository) Get(c context.Context, t, id string) (application.PersonnelChangeRequest, error) {
	var m personnelChangeModel
	err := r.db.WithContext(c).Where("tenant_id = ? AND id = ?", t, id).First(&m).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return application.PersonnelChangeRequest{}, application.ErrNotFound
	}
	return toPersonnel(m), err
}

// UpdateStatus 按旧状态和版本原子推进人员异动；expected 提供租户、请求 ID、旧状态和
// 版本，status/ref 是目标状态与审批凭据。记录已被执行、取消或由其他请求推进时返回
// application.ErrConflict，成功时返回本次事务内读取的新快照。
func (r *PersonnelChangeGORMRepository) UpdateStatus(c context.Context, expected application.PersonnelChangeRequest, status, ref, operator string, now time.Time) (application.PersonnelChangeRequest, error) {
	u := map[string]any{"status": status, "updated_at": now, "version": gorm.Expr("version + 1")}
	if status == "EXECUTED" {
		u["executed_at"] = now
	}
	if expected.Status == domain.PersonnelChangePendingApproval && (status == domain.PersonnelChangePendingHandover || status == domain.PersonnelChangeScheduled) {
		u["approval_reference"] = ref
		u["approved_by"] = operator
		u["approved_at"] = now
	}
	if expected.Status == domain.PersonnelChangePendingApproval && status == domain.PersonnelChangeRejected {
		u["rejection_reason"] = ref
	}
	if expected.Status == domain.PersonnelChangePendingHandover && status == domain.PersonnelChangeScheduled {
		u["handover_reference"] = ref
	}
	if status == domain.PersonnelChangeCancelled {
		u["cancelled_at"] = now
	}
	var updated personnelChangeModel
	err := r.db.WithContext(c).Transaction(func(tx *gorm.DB) error {
		result := tx.Model(&personnelChangeModel{}).
			Where("tenant_id = ? AND id = ? AND status = ? AND version = ?", expected.TenantID, expected.ID, expected.Status, expected.Version).
			Updates(u)
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected != 1 {
			return application.ErrConflict
		}
		if expected.ChangeType == domain.PersonnelChangeTermination && expected.Status == domain.PersonnelChangePendingApproval && status == domain.PersonnelChangePendingHandover {
			// Every termination gets a mandatory durable responsibility item. Subsystem
			// adapters may add more items under the existing unique business key.
			item := personnelHandoverItemModel{ID: expected.ID, TenantID: expected.TenantID, RequestID: expected.ID, SystemCode: "platform", ResourceType: "PERSONNEL_RESPONSIBILITY", ResourceID: expected.UserID, CurrentOwnerID: expected.UserID, Status: "PENDING", CreatedAt: now, UpdatedAt: now}
			if err := tx.Clauses(clause.OnConflict{DoNothing: true}).Create(&item).Error; err != nil {
				return fmt.Errorf("create mandatory personnel handover item: %w", err)
			}
		}
		var reference *string
		if trimmed := strings.TrimSpace(ref); trimmed != "" {
			reference = &trimmed
		}
		if err := tx.Create(&personnelChangeTransitionModel{TenantID: expected.TenantID, RequestID: expected.ID, FromStatus: expected.Status, ToStatus: status, OperatorID: operator, ReferenceValue: reference, CreatedAt: now}).Error; err != nil {
			return fmt.Errorf("record personnel change transition: %w", err)
		}
		if err := tx.Where("tenant_id = ? AND id = ?", expected.TenantID, expected.ID).First(&updated).Error; err != nil {
			return err
		}
		return nil
	})
	if err != nil {
		return application.PersonnelChangeRequest{}, err
	}
	return toPersonnel(updated), nil
}

type personnelPermissionRow struct {
	ApplicationID   string `gorm:"column:application_id"`
	ApplicationCode string `gorm:"column:application_code"`
	ApplicationName string `gorm:"column:application_name"`
	RoleID          string `gorm:"column:role_id"`
	RoleCode        string `gorm:"column:role_code"`
	RoleName        string `gorm:"column:role_name"`
	ScopeType       string `gorm:"column:scope_type"`
	ScopeID         string `gorm:"column:scope_id"`
}

// PreviewPermissions compares the role bindings inherited by the source and target
// positions. Direct user bindings are included in both sets, since a position change
// must not be presented as removing a manually granted role.
func (r *PersonnelChangeGORMRepository) PreviewPermissions(c context.Context, request application.PersonnelChangeRequest) (application.PersonnelChangePermissionPreview, error) {
	now := time.Now().UTC()
	sourceMembership := request.SourceMembershipID
	if sourceMembership == "" {
		sourceMembership = "__ALL__"
	}
	current, err := r.queryPermissionRoles(c, request.TenantID, request.UserID, sourceMembership, "", now)
	if err != nil {
		return application.PersonnelChangePermissionPreview{}, err
	}
	target, err := r.queryPermissionRoles(c, request.TenantID, request.UserID, "", request.TargetPositionID, now)
	if err != nil {
		return application.PersonnelChangePermissionPreview{}, err
	}
	currentByKey := make(map[string]application.PermissionRole, len(current))
	targetByKey := make(map[string]application.PermissionRole, len(target))
	for _, role := range current {
		currentByKey[permissionRoleKey(role)] = role
	}
	for _, role := range target {
		targetByKey[permissionRoleKey(role)] = role
	}
	result := application.PersonnelChangePermissionPreview{Added: []application.PermissionRole{}, Removed: []application.PermissionRole{}, Retained: []application.PermissionRole{}}
	for key, role := range targetByKey {
		if _, ok := currentByKey[key]; ok {
			result.Retained = append(result.Retained, role)
		} else {
			result.Added = append(result.Added, role)
		}
	}
	for key, role := range currentByKey {
		if _, ok := targetByKey[key]; !ok {
			result.Removed = append(result.Removed, role)
		}
	}
	sort.Slice(result.Added, func(i, j int) bool { return permissionRoleKey(result.Added[i]) < permissionRoleKey(result.Added[j]) })
	sort.Slice(result.Removed, func(i, j int) bool {
		return permissionRoleKey(result.Removed[i]) < permissionRoleKey(result.Removed[j])
	})
	sort.Slice(result.Retained, func(i, j int) bool {
		return permissionRoleKey(result.Retained[i]) < permissionRoleKey(result.Retained[j])
	})
	return result, nil
}

func permissionRoleKey(role application.PermissionRole) string {
	return role.ApplicationID + "|" + role.RoleID + "|" + role.ScopeType + "|" + role.ScopeID
}

func (r *PersonnelChangeGORMRepository) queryPermissionRoles(c context.Context, tenant, userID, sourceMembershipID, targetPositionID string, now time.Time) ([]application.PermissionRole, error) {
	// POSITION bindings are materialized by the position-grant service. Restricting
	// them to active assignments/validity keeps the preview identical to runtime auth.
	query := `
SELECT DISTINCT app.id AS application_id, app.code AS application_code, app.name AS application_name,
       role.id AS role_id, role.code AS role_code, role.name AS role_name,
       binding.scope_type, binding.scope_id
FROM authz_role_binding binding
JOIN platform_application app ON app.id = binding.application_id AND app.tenant_id = binding.tenant_id AND app.status = 'ACTIVE'
JOIN authz_role role ON role.id = binding.role_id AND role.tenant_id = binding.tenant_id AND role.application_id = binding.application_id AND role.status = 'ACTIVE'
WHERE binding.tenant_id = ? AND binding.status = 'ACTIVE'
  AND (binding.valid_from IS NULL OR binding.valid_from <= ?) AND (binding.valid_until IS NULL OR binding.valid_until > ?)
  AND ((binding.subject_type = 'USER' AND binding.subject_id = ?)
       OR (binding.subject_type = 'POSITION' AND binding.subject_id IN (
          SELECT m.position_id FROM iam_membership m
          WHERE m.tenant_id = ? AND m.user_id = ? AND m.status = 'ACTIVE' AND m.inherit_authorization = 1
			AND (? = '__ALL__' OR (? <> '' AND (? = '' OR m.id = ?)))
            AND (m.valid_from IS NULL OR m.valid_from <= ?) AND (m.valid_until IS NULL OR m.valid_until > ?)
       ))
       OR (binding.subject_type = 'POSITION' AND ? <> '' AND binding.subject_id = ?))`
	args := []any{tenant, now, now, userID, tenant, userID, sourceMembershipID, sourceMembershipID, sourceMembershipID, sourceMembershipID, now, now, targetPositionID, targetPositionID}
	var rows []personnelPermissionRow
	if err := r.db.WithContext(c).Raw(query, args...).Scan(&rows).Error; err != nil {
		return nil, err
	}
	roles := make([]application.PermissionRole, 0, len(rows))
	for _, row := range rows {
		roles = append(roles, application.PermissionRole{ApplicationID: row.ApplicationID, ApplicationCode: row.ApplicationCode, ApplicationName: row.ApplicationName, RoleID: row.RoleID, RoleCode: row.RoleCode, RoleName: row.RoleName, ScopeType: row.ScopeType, ScopeID: row.ScopeID})
	}
	return roles, nil
}
