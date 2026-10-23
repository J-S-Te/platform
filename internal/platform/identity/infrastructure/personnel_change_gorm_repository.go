package infrastructure

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/J-S-Te/Basic-Platform/internal/platform/bootstrapadmin"
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

// personnelApprovalExceptionOutboxModel 是超管例外审批的本地审计 outbox。它和审批状态在
// 同一事务写入；投递状态与重试进度独立更新，不反向影响已提交的审批结果。
type personnelApprovalExceptionOutboxModel struct {
	ID                string     `gorm:"column:id;primaryKey"`
	TenantID          string     `gorm:"column:tenant_id"`
	RequestID         string     `gorm:"column:request_id"`
	OperatorID        string     `gorm:"column:operator_id"`
	SubmittedBy       string     `gorm:"column:submitted_by"`
	SubjectUserID     string     `gorm:"column:subject_user_id"`
	ToStatus          string     `gorm:"column:to_status"`
	RuleHit           string     `gorm:"column:rule_hit"`
	ExceptionReason   string     `gorm:"column:exception_reason"`
	ApprovalReference string     `gorm:"column:approval_reference"`
	OccurredAt        time.Time  `gorm:"column:occurred_at"`
	Status            string     `gorm:"column:status"`
	Attempts          uint       `gorm:"column:attempts"`
	NextAttemptAt     time.Time  `gorm:"column:next_attempt_at"`
	LastError         *string    `gorm:"column:last_error"`
	DeliveredAt       *time.Time `gorm:"column:delivered_at"`
	CreatedAt         time.Time
	UpdatedAt         time.Time
}

// TableName 指定 GORM 行映射的超管例外审计 outbox 表名。
func (personnelApprovalExceptionOutboxModel) TableName() string {
	return "iam_personnel_approval_exception_outbox"
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

// TableName 指定 GORM 行映射的异动轨迹表名。
func (personnelChangeTransitionModel) TableName() string { return "iam_personnel_change_transition" }

type personnelHandoverItemModel struct {
	ID, TenantID, RequestID, SystemCode, ResourceType, ResourceID, CurrentOwnerID, Status string
	CreatedAt, UpdatedAt                                                                  time.Time
}

// TableName 指定 GORM 行映射的交接责任项表名。
func (personnelHandoverItemModel) TableName() string { return "iam_personnel_handover_item" }

// Execute 原子应用一个已批准的异动。调岗关闭旧任职并创建新任职；离职停用用户全部访问。
func (r *PersonnelChangeGORMRepository) Execute(c context.Context, req application.PersonnelChangeRequest, operator string, now time.Time) (application.PersonnelChangeRequest, error) {
	err := r.db.WithContext(c).Transaction(func(tx *gorm.DB) error {
		// 串行化该请求的执行。多个 worker 副本可能观察到同一到期行；先加锁再复核状态，
		// 让第二个执行在改动任职或账号之前变成 no-op。
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
		identity, initialized, err := bootstrapadmin.Resolve(c, tx, req.TenantID)
		if err != nil {
			return err
		}
		if initialized && identity.ProtectsUser(req.UserID) {
			return bootstrapadmin.ErrProtected
		}
		if req.ChangeType == domain.PersonnelChangeTermination {
			var accountIDs []string
			if err := tx.Model(&accountModel{}).Where("tenant_id = ? AND user_id = ?", req.TenantID, req.UserID).Pluck("id", &accountIDs).Error; err != nil {
				return fmt.Errorf("list accounts before personnel termination: %w", err)
			}
			identityRepository := &GORMRepository{database: tx}
			for _, accountID := range accountIDs {
				// 在停用账号之前，吊销平台会话、授权码、刷新令牌族并入队 RP Back-Channel
				// 注销。没有活跃会话的账号是合法的离职场景。
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
			authority := req.ApprovedBy
			if authority == "" {
				authority = req.SubmittedBy
			}
			desired := membershipModel{UserID: req.UserID, OrgUnitID: req.TargetOrgUnitID, PositionID: req.TargetPositionID, Status: domain.StatusActive, InheritAuthorization: source.InheritAuthorization, ValidFrom: &now}
			if err := validateMembershipDelegation(c, tx, req.TenantID, authority, desired, &source, now); err != nil {
				return err
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
		// 事件类型必须落在 keycloak_authorization_outbox 的检查约束内；异动执行
		// 变更的是就业状态、主组织与账号状态，按身份变化事件驱动下游投影刷新。
		// 晋升/降职/调岗立即改变有效权限与主组织：撤销被调整人的活跃平台会话并经
		// RP back-channel logout 通知下游销毁 SSO 会话，防止旧身份 token 在过期前
		// 继续访问已变更的授权。无活跃会话（未登录）时撤销返回 ErrUnauthenticated，
		// 属正常情况；重新登录由会话实时投影与 outbox 同步保证拿到新任职。
		if req.ChangeType == domain.PersonnelChangePromotion || req.ChangeType == domain.PersonnelChangeDemotion || req.ChangeType == domain.PersonnelChangeTransfer {
			var reassignmentAccountIDs []string
			if err := tx.Model(&accountModel{}).Where("tenant_id = ? AND user_id = ?", req.TenantID, req.UserID).Pluck("id", &reassignmentAccountIDs).Error; err != nil {
				return fmt.Errorf("list accounts before personnel reassignment: %w", err)
			}
			identityRepository := &GORMRepository{database: tx}
			for _, accountID := range reassignmentAccountIDs {
				if err := identityRepository.RevokeAccountSessions(c, req.TenantID, accountID, now, "PERSONNEL_TRANSFER"); err != nil && !errors.Is(err, application.ErrUnauthenticated) {
					return fmt.Errorf("revoke account sessions during personnel reassignment: %w", err)
				}
			}
		}
		if err := enqueueKeycloakIdentityEvents(tx, req.TenantID, []string{req.UserID}, now, "IDENTITY_CHANGED"); err != nil {
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
	// 事务成功后直接返回锁内快照。提交后的查询可能因断连失败，不能把已提交的
	// EXECUTED 伪报为失败，也不能因此阻止远端冻结的异步完成补偿。
	req.Status = domain.PersonnelChangeExecuted
	req.ExecutedAt = &now
	req.UpdatedAt = now
	req.Version++
	return req, nil
}

// TableName 指定 GORM 行映射的异动请求表名。
func (personnelChangeModel) TableName() string { return "iam_personnel_change_request" }

// PersonnelChangeGORMRepository 以 GORM 持久化人员异动请求、状态轨迹与交接责任项，
// 全部写入都带租户条件并配合版本乐观锁。
type PersonnelChangeGORMRepository struct{ db *gorm.DB }

// NewPersonnelChangeGORMRepository 构造异动仓储。
func NewPersonnelChangeGORMRepository(db *gorm.DB) *PersonnelChangeGORMRepository {
	return &PersonnelChangeGORMRepository{db: db}
}

// ValidateCreate 把全部身份与目标任职检查提前到持久化之前。Execute 会重复关键检查，
// 防御排期到执行之间发生的变化。
func (r *PersonnelChangeGORMRepository) ValidateCreate(c context.Context, in application.PersonnelChangeCreateInput) error {
	identity, initialized, err := bootstrapadmin.Resolve(c, r.db, in.TenantID)
	if err != nil {
		return err
	}
	if initialized && identity.ProtectsUser(in.UserID) {
		return bootstrapadmin.ErrProtected
	}
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

// Create 在同一事务写入异动请求和首条状态轨迹（FromStatus 为空），保证任何可见请求都有
// 完整的轨迹起点，审计上不存在无来源的状态。
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

// List 联表补齐人员与来源/目标组织岗位展示名，过滤条件全部参数化并保持租户条件；
// 结果按创建时间倒序。
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

// Get 按租户与请求 ID 读取异动请求；未命中统一映射为 application.ErrNotFound。
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
	return r.updateStatus(c, expected, status, ref, operator, now, nil)
}

// UpdateStatusWithApprovalException 在 UpdateStatus 的同一事务内写入超管例外审计 outbox。
// 状态更新、状态轨迹与 outbox 要么全部提交，要么全部回滚；事务外再访问审计端，
// 避免出现"审批已提交但没有例外原因/审计事实"的缺口。
func (r *PersonnelChangeGORMRepository) UpdateStatusWithApprovalException(c context.Context, expected application.PersonnelChangeRequest, status, ref, operator string, now time.Time, exception application.PersonnelChangeAuditEvent) (application.PersonnelChangeRequest, error) {
	if err := validateApprovalException(expected, exception); err != nil {
		return application.PersonnelChangeRequest{}, err
	}
	return r.updateStatus(c, expected, status, ref, operator, now, &exception)
}

func validateApprovalException(expected application.PersonnelChangeRequest, exception application.PersonnelChangeAuditEvent) error {
	if strings.TrimSpace(exception.EventID) == "" || exception.RequestID != expected.ID || exception.TenantID != expected.TenantID ||
		strings.TrimSpace(exception.Reason) == "" || strings.TrimSpace(exception.RuleHit) == "" || exception.OccurredAt.IsZero() {
		return fmt.Errorf("approval exception audit event is incomplete: %w", application.ErrValidation)
	}
	return nil
}

func (r *PersonnelChangeGORMRepository) updateStatus(c context.Context, expected application.PersonnelChangeRequest, status, ref, operator string, now time.Time, exception *application.PersonnelChangeAuditEvent) (application.PersonnelChangeRequest, error) {
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
			// 每个离职都会得到一条强制的持久责任项；子系统适配器可在既有唯一业务键下追加
			// 更多项。
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
		if exception != nil {
			// outbox 写入放在状态更新和轨迹之后：任何一步失败都会回滚整笔事务，
			// 不会留下"状态已提交、例外原因或审计事实缺失"的部分结果。
			if err := tx.Create(&personnelApprovalExceptionOutboxModel{
				ID: exception.EventID, TenantID: expected.TenantID, RequestID: expected.ID,
				OperatorID: exception.OperatorID, SubmittedBy: exception.SubmittedBy, SubjectUserID: exception.SubjectUserID,
				ToStatus: exception.ToStatus, RuleHit: exception.RuleHit, ExceptionReason: exception.Reason,
				ApprovalReference: exception.ApprovalReference, OccurredAt: exception.OccurredAt,
				Status: application.ApprovalExceptionOutboxPending, Attempts: 0, NextAttemptAt: now,
				CreatedAt: now, UpdatedAt: now,
			}).Error; err != nil {
				return fmt.Errorf("persist approval exception audit outbox: %w", err)
			}
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

// 编译期确认仓储满足服务所需的"状态 + outbox 同事务"端口。
var _ application.PersonnelChangeApprovalExceptionStore = (*PersonnelChangeGORMRepository)(nil)

// PendingApprovalExceptions 返回到达重试时间的待投递例外审计事件；读取顺序稳定，
// 让多轮投递可预期地推进，不做内存扫描全表。
func (r *PersonnelChangeGORMRepository) PendingApprovalExceptions(c context.Context, now time.Time, limit int) ([]application.ApprovalExceptionOutboxEntry, error) {
	if limit <= 0 {
		limit = 50
	}
	if limit > 200 {
		limit = 200
	}
	var rows []personnelApprovalExceptionOutboxModel
	if err := r.db.WithContext(c).
		Where("status = ? AND next_attempt_at <= ?", application.ApprovalExceptionOutboxPending, now.UTC()).
		Order("next_attempt_at, occurred_at").Limit(limit).Find(&rows).Error; err != nil {
		return nil, err
	}
	entries := make([]application.ApprovalExceptionOutboxEntry, 0, len(rows))
	for _, row := range rows {
		entries = append(entries, application.ApprovalExceptionOutboxEntry{Event: row.toAuditEvent(), Attempts: int(row.Attempts)})
	}
	return entries, nil
}

// MarkApprovalExceptionDelivered 把已成功投递的事件标记为完成。重复标记是幂等的：
// 事件已送达时返回成功，避免 dispatcher 因并发或重放把正常结果当成错误。
func (r *PersonnelChangeGORMRepository) MarkApprovalExceptionDelivered(c context.Context, tenantID, eventID string, now time.Time) error {
	result := r.db.WithContext(c).Model(&personnelApprovalExceptionOutboxModel{}).
		Where("tenant_id = ? AND id = ? AND status = ?", tenantID, eventID, application.ApprovalExceptionOutboxPending).
		Updates(map[string]any{"status": application.ApprovalExceptionOutboxDelivered, "delivered_at": now, "last_error": nil, "updated_at": now})
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected == 0 {
		return requireApprovalExceptionOutboxRow(r.db.WithContext(c), tenantID, eventID)
	}
	return nil
}

// MarkApprovalExceptionRetry 记录一次投递失败和下次可尝试时间，事件保持待投递。
// 不删除 outbox 行，也不改变审批状态，失败必须能被下一轮 worker 重试。
func (r *PersonnelChangeGORMRepository) MarkApprovalExceptionRetry(c context.Context, tenantID, eventID string, attempts int, nextAttemptAt time.Time, lastError string, now time.Time) error {
	if attempts < 1 {
		return fmt.Errorf("approval exception attempts must be positive: %w", application.ErrValidation)
	}
	lastError = strings.TrimSpace(lastError)
	if runes := []rune(lastError); len(runes) > 500 {
		lastError = string(runes[:500])
	}
	result := r.db.WithContext(c).Model(&personnelApprovalExceptionOutboxModel{}).
		Where("tenant_id = ? AND id = ? AND status = ?", tenantID, eventID, application.ApprovalExceptionOutboxPending).
		Updates(map[string]any{"attempts": attempts, "next_attempt_at": nextAttemptAt.UTC(), "last_error": lastError, "updated_at": now})
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected == 0 {
		return requireApprovalExceptionOutboxRow(r.db.WithContext(c), tenantID, eventID)
	}
	return nil
}

// requireApprovalExceptionOutboxRow 把"行不存在"统一映射为 application.ErrNotFound；
// 行存在（可能已被并发标记完成）视为幂等成功。
func requireApprovalExceptionOutboxRow(database *gorm.DB, tenantID, eventID string) error {
	var count int64
	if err := database.Model(&personnelApprovalExceptionOutboxModel{}).Where("tenant_id = ? AND id = ?", tenantID, eventID).Count(&count).Error; err != nil {
		return err
	}
	if count == 0 {
		return application.ErrNotFound
	}
	return nil
}

func (row personnelApprovalExceptionOutboxModel) toAuditEvent() application.PersonnelChangeAuditEvent {
	return application.PersonnelChangeAuditEvent{
		EventID: row.ID, TenantID: row.TenantID, OperatorID: row.OperatorID, RequestID: row.RequestID,
		ToStatus: row.ToStatus, SubmittedBy: row.SubmittedBy, SubjectUserID: row.SubjectUserID,
		Reason: row.ExceptionReason, RuleHit: row.RuleHit, ApprovalReference: row.ApprovalReference,
		OccurredAt: row.OccurredAt,
	}
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

// PreviewPermissions 比较源岗位与目标岗位继承的角色绑定。直接用户绑定同时出现在两个
// 集合中：调岗绝不能被展示为移除了手工授予的角色。
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
	// POSITION 绑定由岗位授予服务物化。把它们限制在活跃任职/有效期内，保证预览结果与
	// 运行时授权一致。
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
