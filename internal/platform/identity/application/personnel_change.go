package application

import (
	"context"
	"errors"
	"fmt"
	"github.com/J-S-Te/Basic-Platform/internal/platform/identity/domain"
	notificationapp "github.com/J-S-Te/Basic-Platform/internal/platform/notification/application"
	notificationdomain "github.com/J-S-Te/Basic-Platform/internal/platform/notification/domain"
	"strings"
	"time"
)

type PersonnelChangeRequest struct {
	// 请求同时承载业务变更、审批凭据和执行时间；临时密码只允许出现在即时返聘响应中。
	ID, TenantID, UserID, SourceMembershipID, TargetOrgUnitID, TargetPositionID, ChangeType, Status, Reason, ApprovalReference, SubmittedBy, ApprovedBy string
	EffectiveAt, ApprovedAt, ExecutedAt                                                                                                                 *time.Time
	Version                                                                                                                                             uint64
	// TemporaryPassword 仅在即时返聘执行响应中填充，不落库、不记录日志，也不由列表或详情接口返回。
	TemporaryPassword    string `json:"temporary_password,omitempty"`
	CreatedAt, UpdatedAt time.Time
}
type PersonnelChangeCreateInput struct {
	TenantID, OperatorID, UserID, SourceMembershipID, TargetOrgUnitID, TargetPositionID, ChangeType, Reason, ApprovalReference string
	EffectiveAt                                                                                                                time.Time
}
type PersonnelChangeTransitionInput struct{ TenantID, OperatorID, ID, ToStatus, ApprovalReference string }
type PermissionRole struct {
	ApplicationID   string `json:"application_id"`
	ApplicationCode string `json:"application_code"`
	ApplicationName string `json:"application_name"`
	RoleID          string `json:"role_id"`
	RoleCode        string `json:"role_code"`
	RoleName        string `json:"role_name"`
	ScopeType       string `json:"scope_type"`
	ScopeID         string `json:"scope_id"`
}
type PersonnelChangePermissionPreview struct {
	Added    []PermissionRole `json:"added_roles"`
	Removed  []PermissionRole `json:"removed_roles"`
	Retained []PermissionRole `json:"retained_roles"`
}
type PersonnelChangeRepository interface {
	Create(context.Context, PersonnelChangeRequest) (PersonnelChangeRequest, error)
	List(context.Context, string, string, string, string) ([]PersonnelChangeRequest, error)
	Get(context.Context, string, string) (PersonnelChangeRequest, error)
	UpdateStatus(context.Context, string, string, string, string, time.Time) (PersonnelChangeRequest, error)
	Execute(context.Context, PersonnelChangeRequest, string, time.Time) (PersonnelChangeRequest, error)
	PreviewPermissions(context.Context, PersonnelChangeRequest) (PersonnelChangePermissionPreview, error)
}
type PersonnelChangeService struct {
	repo     PersonnelChangeRepository
	ids      IDGenerator
	clock    Clock
	handover HandoverChecker
	notifier interface {
		Create(context.Context, notificationapp.CreateInput) (notificationapp.CreateResult, error)
	}
}

func NewPersonnelChangeService(repo PersonnelChangeRepository, ids IDGenerator, clock Clock, checkers ...HandoverChecker) (*PersonnelChangeService, error) {
	// 服务启动时即拒绝缺失依赖，避免运行到审批或执行路径才出现不可诊断的空指针。
	if repo == nil || ids == nil || clock == nil {
		return nil, errors.New("personnel change dependencies must not be nil")
	}
	var handover HandoverChecker
	if len(checkers) > 1 {
		return nil, errors.New("personnel change accepts at most one handover checker")
	}
	if len(checkers) == 1 {
		handover = checkers[0]
	}
	return &PersonnelChangeService{repo: repo, ids: ids, clock: clock, handover: handover}, nil
}
func (s *PersonnelChangeService) Create(ctx context.Context, in PersonnelChangeCreateInput) (PersonnelChangeRequest, error) {
	// 人员异动由具备管理权限的管理员直接配置，不再创建待审批草稿；
	// 进入待生效后由 worker 按 effective_at 执行，避免把管理员配置误导成审批流程。
	in.ChangeType = strings.ToUpper(strings.TrimSpace(in.ChangeType))
	in.Reason = strings.TrimSpace(in.Reason)
	if in.TenantID == "" || in.OperatorID == "" || in.UserID == "" || in.Reason == "" || in.EffectiveAt.IsZero() {
		return PersonnelChangeRequest{}, ErrValidation
	}
	switch in.ChangeType {
	case domain.PersonnelChangePromotion, domain.PersonnelChangeDemotion, domain.PersonnelChangeTransfer, domain.PersonnelChangeTermination, domain.PersonnelChangeRehire:
	default:
		return PersonnelChangeRequest{}, ErrValidation
	}
	now := s.clock.Now().UTC()
	id, err := s.ids.New(now)
	if err != nil {
		return PersonnelChangeRequest{}, fmt.Errorf("generate personnel change id: %w", err)
	}
	status := domain.PersonnelChangeScheduled
	return s.repo.Create(ctx, PersonnelChangeRequest{ID: id, TenantID: in.TenantID, UserID: in.UserID, SourceMembershipID: in.SourceMembershipID, TargetOrgUnitID: in.TargetOrgUnitID, TargetPositionID: in.TargetPositionID, ChangeType: in.ChangeType, Status: status, Reason: in.Reason, ApprovalReference: in.ApprovalReference, SubmittedBy: in.OperatorID, EffectiveAt: &in.EffectiveAt, Version: 1, CreatedAt: now, UpdatedAt: now})
}
func (s *PersonnelChangeService) List(ctx context.Context, tenant, status, changeType, keyword string) ([]PersonnelChangeRequest, error) {
	return s.repo.List(ctx, tenant, status, changeType, keyword)
}
func (s *PersonnelChangeService) Get(ctx context.Context, tenant, id string) (PersonnelChangeRequest, error) {
	return s.repo.Get(ctx, tenant, id)
}
func (s *PersonnelChangeService) Transition(ctx context.Context, in PersonnelChangeTransitionInput) (PersonnelChangeRequest, error) {
	if in.TenantID == "" || in.OperatorID == "" || in.ID == "" || in.ToStatus == "" {
		return PersonnelChangeRequest{}, ErrValidation
	}
	cur, err := s.repo.Get(ctx, in.TenantID, in.ID)
	if err != nil {
		return PersonnelChangeRequest{}, err
	}
	if !domain.CanTransitionPersonnelChange(cur.Status, in.ToStatus) {
		return PersonnelChangeRequest{}, ErrConflict
	}
	// 审批凭据与离职交接是显式安全闸门；交接系统未接入时，凭据仍是责任已转移并检查过的持久证据。
	if in.ToStatus == domain.PersonnelChangeScheduled {
		if strings.TrimSpace(in.ApprovalReference) == "" {
			return PersonnelChangeRequest{}, fmt.Errorf("approval reference is required: %w", ErrValidation)
		}
		if cur.ChangeType == domain.PersonnelChangeTermination && !strings.HasPrefix(strings.ToUpper(strings.TrimSpace(in.ApprovalReference)), "HANDOVER-") {
			return PersonnelChangeRequest{}, fmt.Errorf("termination requires a HANDOVER-* reference: %w", ErrConflict)
		}
		if cur.ChangeType == domain.PersonnelChangeTermination {
			if s.handover == nil {
				return PersonnelChangeRequest{}, fmt.Errorf("handover checker is unavailable: %w", ErrConflict)
			}
			report, checkErr := s.handover.Check(ctx, cur)
			if checkErr != nil {
				return PersonnelChangeRequest{}, fmt.Errorf("handover check unavailable: %w", checkErr)
			}
			if !report.Ready {
				return PersonnelChangeRequest{}, fmt.Errorf("responsibility handover is incomplete: %w", ErrConflict)
			}
		}
	}
	if in.ToStatus == domain.PersonnelChangeExecuted {
		// 执行只接受已到生效时间的请求，防止提前变更身份与权限。
		if cur.EffectiveAt == nil || cur.EffectiveAt.After(s.clock.Now().UTC()) {
			return PersonnelChangeRequest{}, fmt.Errorf("personnel change is not yet effective: %w", ErrConflict)
		}
		result, execErr := s.repo.Execute(ctx, cur, in.OperatorID, s.clock.Now().UTC())
		if execErr == nil && s.notifier != nil {
			s.notify(ctx, cur, in.OperatorID)
		}
		return result, execErr
	}
	return s.repo.UpdateStatus(ctx, in.TenantID, in.ID, in.ToStatus, in.ApprovalReference, s.clock.Now().UTC())
}
func (s *PersonnelChangeService) Preview(ctx context.Context, tenant, id string) (map[string]any, error) {
	// 已落库请求的权限影响由仓储计算，确保预览结果与实际授权来源一致。
	r, e := s.Get(ctx, tenant, id)
	if e != nil {
		return nil, e
	}
	roles, e := s.repo.PreviewPermissions(ctx, r)
	if e != nil {
		return nil, e
	}
	return map[string]any{"request": r, "added_roles": roles.Added, "removed_roles": roles.Removed, "retained_roles": roles.Retained}, nil
}

// PreviewDraft 在请求落库前计算权限差异；复用持久化预览查询，避免 UI 自行推断授权结果。
func (s *PersonnelChangeService) PreviewDraft(ctx context.Context, in PersonnelChangeCreateInput) (map[string]any, error) {
	if strings.TrimSpace(in.TenantID) == "" || strings.TrimSpace(in.UserID) == "" {
		return nil, ErrValidation
	}
	r := PersonnelChangeRequest{TenantID: in.TenantID, UserID: in.UserID, SourceMembershipID: in.SourceMembershipID, TargetPositionID: in.TargetPositionID}
	roles, err := s.repo.PreviewPermissions(ctx, r)
	if err != nil {
		return nil, err
	}
	return map[string]any{"added_roles": roles.Added, "removed_roles": roles.Removed, "retained_roles": roles.Retained}, nil
}

// SetNotifier 在模块组装完成后注入共享通知服务。
func (s *PersonnelChangeService) SetNotifier(n interface {
	Create(context.Context, notificationapp.CreateInput) (notificationapp.CreateResult, error)
}) {
	s.notifier = n
}
func (s *PersonnelChangeService) notify(ctx context.Context, req PersonnelChangeRequest, operator string) {
	recipients := []notificationdomain.RecipientTarget{{Type: notificationdomain.RecipientTypeUser, ID: req.UserID}, {Type: notificationdomain.RecipientTypeUser, ID: operator}}
	// 目标组织作为受众覆盖新负责人和管理员，避免身份模块依赖具体的管理者模型。
	if strings.TrimSpace(req.TargetOrgUnitID) != "" {
		recipients = append(recipients, notificationdomain.RecipientTarget{Type: notificationdomain.RecipientTypeOrganization, ID: req.TargetOrgUnitID})
	}
	_, _ = s.notifier.Create(ctx, notificationapp.CreateInput{TenantID: req.TenantID, OperatorID: operator, TemplateCode: "personnel_change_executed", Category: "PERSONNEL_CHANGE", Variables: map[string]string{"change_type": req.ChangeType, "reason": req.Reason, "request_id": req.ID}, Recipients: recipients, ReferenceType: "PERSONNEL_CHANGE", ReferenceID: req.ID, IdempotencyKey: "personnel-change:" + req.ID})
}
