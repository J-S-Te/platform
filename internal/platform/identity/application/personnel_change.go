package application

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/J-S-Te/Basic-Platform/internal/platform/identity/domain"
	notificationapp "github.com/J-S-Te/Basic-Platform/internal/platform/notification/application"
	notificationdomain "github.com/J-S-Te/Basic-Platform/internal/platform/notification/domain"
)

type PersonnelChangeRequest struct {
	// 请求同时承载业务变更、审批凭据和执行时间；复职沿用既有凭据并强制下次改密。
	ID                 string     `json:"id"`
	TenantID           string     `json:"tenant_id"`
	UserID             string     `json:"user_id"`
	UserDisplayName    string     `json:"user_display_name,omitempty"`
	SourceMembershipID string     `json:"source_membership_id,omitempty"`
	SourceOrganization string     `json:"source_organization_name,omitempty"`
	SourcePosition     string     `json:"source_position_name,omitempty"`
	TargetOrgUnitID    string     `json:"target_org_unit_id,omitempty"`
	TargetOrganization string     `json:"target_organization_name,omitempty"`
	TargetPositionID   string     `json:"target_position_id,omitempty"`
	TargetPosition     string     `json:"target_position_name,omitempty"`
	ChangeType         string     `json:"change_type"`
	Status             string     `json:"status"`
	Reason             string     `json:"reason"`
	ApprovalReference  string     `json:"approval_reference,omitempty"`
	HandoverReference  string     `json:"handover_reference,omitempty"`
	RejectionReason    string     `json:"rejection_reason,omitempty"`
	SubmittedBy        string     `json:"submitted_by"`
	ApprovedBy         string     `json:"approved_by,omitempty"`
	EffectiveAt        *time.Time `json:"effective_at"`
	ApprovedAt         *time.Time `json:"approved_at,omitempty"`
	ExecutedAt         *time.Time `json:"executed_at,omitempty"`
	CancelledAt        *time.Time `json:"cancelled_at,omitempty"`
	Version            uint64     `json:"version"`
	CreatedAt          time.Time  `json:"created_at"`
	UpdatedAt          time.Time  `json:"updated_at"`
}
type PersonnelChangeCreateInput struct {
	TenantID, OperatorID, UserID, SourceMembershipID, TargetOrgUnitID, TargetPositionID, ChangeType, Reason, ApprovalReference string
	EffectiveAt                                                                                                                time.Time
	// DirectScheduleAuthorized is derived exclusively from the authenticated server-side
	// principal. Browser payloads must never decide whether approval can be bypassed.
	DirectScheduleAuthorized bool
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
	UpdateStatus(context.Context, PersonnelChangeRequest, string, string, string, time.Time) (PersonnelChangeRequest, error)
	Execute(context.Context, PersonnelChangeRequest, string, time.Time) (PersonnelChangeRequest, error)
	PreviewPermissions(context.Context, PersonnelChangeRequest) (PersonnelChangePermissionPreview, error)
	ValidateCreate(context.Context, PersonnelChangeCreateInput) error
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
	in.ChangeType = strings.ToUpper(strings.TrimSpace(in.ChangeType))
	in.UserID = strings.TrimSpace(in.UserID)
	in.SourceMembershipID = strings.TrimSpace(in.SourceMembershipID)
	in.TargetOrgUnitID = strings.TrimSpace(in.TargetOrgUnitID)
	in.TargetPositionID = strings.TrimSpace(in.TargetPositionID)
	in.Reason = strings.TrimSpace(in.Reason)
	if in.TenantID == "" || in.OperatorID == "" || in.UserID == "" || in.Reason == "" || in.EffectiveAt.IsZero() {
		return PersonnelChangeRequest{}, ErrValidation
	}
	switch in.ChangeType {
	case domain.PersonnelChangePromotion, domain.PersonnelChangeDemotion, domain.PersonnelChangeTransfer, domain.PersonnelChangeTermination, domain.PersonnelChangeRehire:
	default:
		return PersonnelChangeRequest{}, ErrValidation
	}
	if requiresSourceMembership(in.ChangeType) && in.SourceMembershipID == "" {
		return PersonnelChangeRequest{}, fmt.Errorf("source membership is required: %w", ErrValidation)
	}
	if requiresTargetAssignment(in.ChangeType) && (in.TargetOrgUnitID == "" || in.TargetPositionID == "") {
		return PersonnelChangeRequest{}, fmt.Errorf("target organization and position are required: %w", ErrValidation)
	}
	if err := s.repo.ValidateCreate(ctx, in); err != nil {
		return PersonnelChangeRequest{}, fmt.Errorf("validate personnel change: %w", err)
	}
	now := s.clock.Now().UTC()
	id, err := s.ids.New(now)
	if err != nil {
		return PersonnelChangeRequest{}, fmt.Errorf("generate personnel change id: %w", err)
	}
	status := domain.PersonnelChangeDraft
	// Only a server-authorized super administrator may directly schedule a
	// non-termination change. Termination always traverses approval and handover.
	if in.DirectScheduleAuthorized && in.ChangeType != domain.PersonnelChangeTermination {
		status = domain.PersonnelChangeScheduled
	}
	return s.repo.Create(ctx, PersonnelChangeRequest{ID: id, TenantID: in.TenantID, UserID: in.UserID, SourceMembershipID: in.SourceMembershipID, TargetOrgUnitID: in.TargetOrgUnitID, TargetPositionID: in.TargetPositionID, ChangeType: in.ChangeType, Status: status, Reason: in.Reason, ApprovalReference: in.ApprovalReference, SubmittedBy: in.OperatorID, EffectiveAt: &in.EffectiveAt, Version: 1, CreatedAt: now, UpdatedAt: now})
}

func requiresSourceMembership(changeType string) bool {
	switch changeType {
	case domain.PersonnelChangePromotion, domain.PersonnelChangeDemotion, domain.PersonnelChangeTransfer, domain.PersonnelChangeTermination:
		return true
	default:
		return false
	}
}

func requiresTargetAssignment(changeType string) bool {
	switch changeType {
	case domain.PersonnelChangePromotion, domain.PersonnelChangeDemotion, domain.PersonnelChangeTransfer, domain.PersonnelChangeRehire:
		return true
	default:
		return false
	}
}
func (s *PersonnelChangeService) List(ctx context.Context, tenant, status, changeType, keyword string) ([]PersonnelChangeRequest, error) {
	return s.repo.List(ctx, tenant, status, changeType, keyword)
}
func (s *PersonnelChangeService) Get(ctx context.Context, tenant, id string) (PersonnelChangeRequest, error) {
	return s.repo.Get(ctx, tenant, id)
}

func (s *PersonnelChangeService) ListHandoverItems(ctx context.Context, tenant, requestID string) ([]HandoverItem, error) {
	manager, ok := s.handover.(HandoverManager)
	if !ok || strings.TrimSpace(tenant) == "" || strings.TrimSpace(requestID) == "" {
		return nil, ErrValidation
	}
	return manager.List(ctx, tenant, requestID)
}

func (s *PersonnelChangeService) CompleteHandoverItem(ctx context.Context, tenant, requestID, itemID, targetUserID, operator string) (HandoverItem, error) {
	manager, ok := s.handover.(HandoverManager)
	if !ok || strings.TrimSpace(tenant) == "" || strings.TrimSpace(requestID) == "" || strings.TrimSpace(itemID) == "" || strings.TrimSpace(targetUserID) == "" || strings.TrimSpace(operator) == "" {
		return HandoverItem{}, ErrValidation
	}
	request, err := s.repo.Get(ctx, tenant, requestID)
	if err != nil {
		return HandoverItem{}, err
	}
	if request.ChangeType != domain.PersonnelChangeTermination || request.Status != domain.PersonnelChangePendingHandover {
		return HandoverItem{}, ErrConflict
	}
	return manager.Complete(ctx, tenant, requestID, itemID, targetUserID, operator)
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
	if cur.ChangeType == domain.PersonnelChangeTermination && cur.Status == domain.PersonnelChangePendingApproval && in.ToStatus == domain.PersonnelChangeScheduled {
		return PersonnelChangeRequest{}, fmt.Errorf("termination must enter handover before scheduling: %w", ErrConflict)
	}
	if cur.Status == domain.PersonnelChangePendingApproval && (in.ToStatus == domain.PersonnelChangePendingHandover || in.ToStatus == domain.PersonnelChangeScheduled) && strings.TrimSpace(in.ApprovalReference) == "" {
		return PersonnelChangeRequest{}, fmt.Errorf("approval reference is required: %w", ErrValidation)
	}
	if cur.Status == domain.PersonnelChangePendingApproval && in.ToStatus == domain.PersonnelChangeRejected && strings.TrimSpace(in.ApprovalReference) == "" {
		return PersonnelChangeRequest{}, fmt.Errorf("rejection reason is required: %w", ErrValidation)
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
			// Notify only after the repository has durably transitioned the request;
			// retries or failed executions must not announce a false completion.
			s.notify(ctx, result, in.OperatorID)
		}
		return result, execErr
	}
	// 仓储同时校验读取到的旧状态和版本，避免取消/审批与 worker 执行并发时
	// 后提交的一方覆盖已经完成的终态。
	return s.repo.UpdateStatus(ctx, cur, in.ToStatus, in.ApprovalReference, in.OperatorID, s.clock.Now().UTC())
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
