package application

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/J-S-Te/Basic-Platform/internal/platform/identity/domain"
	notificationapp "github.com/J-S-Te/Basic-Platform/internal/platform/notification/application"
	notificationdomain "github.com/J-S-Te/Basic-Platform/internal/platform/notification/domain"
)

// PersonnelChangeRequest 是一次人员异动请求的完整聚合视图，贯穿创建、审批、交接、执行
// 与取消的不可变状态轨迹。
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

// PersonnelChangeCreateInput 是异动创建用例的输入；目标引用合法性由仓储在 ValidateCreate
// 中跨租户校验。
type PersonnelChangeCreateInput struct {
	TenantID, OperatorID, UserID, SourceMembershipID, TargetOrgUnitID, TargetPositionID, ChangeType, Reason, ApprovalReference string
	EffectiveAt                                                                                                                time.Time
	// DirectScheduleAuthorized 只能由服务端已认证主体推导。浏览器载荷绝不能决定审批
	// 是否可以被绕过。
	DirectScheduleAuthorized bool
}

// PersonnelChangeAuditEvent 是身份模块对外的最小审计事件，不携带审计模块内部类型。
// EventID 在写入本地 outbox 时生成并保持稳定，异步投递重试必须复用它，接收端据此幂等去重。
type PersonnelChangeAuditEvent struct {
	EventID                                                               string
	TenantID, OperatorID, RequestID, ToStatus, SubmittedBy, SubjectUserID string
	Reason, RuleHit, ApprovalReference                                    string
	OccurredAt                                                            time.Time
}

// PersonnelChangeAuditSink 由组合根适配到平台审计服务；超管例外审批必须留痕。
type PersonnelChangeAuditSink interface {
	RecordApprovalException(context.Context, PersonnelChangeAuditEvent) error
}

// HandoverCoordinator 是离职交接编排的最小端口（由 HandoverOrchestrator 实现）。
type HandoverCoordinator interface {
	Start(context.Context, string, string, string) (HandoverBatch, error)
	CancelByRequest(context.Context, string, string) error
}

// HandoverCompletionVerifier 是跨系统交接的完成核验能力。协调器只在跨系统交接启用时注入，
// 因此排期前必须通过该端口核对持久化回执与阻断状态；本地人工勾选的 COMPLETED 不能替代
// 远端责任已经转移的事实。
type HandoverCompletionVerifier interface {
	VerifyCrossSystemCompletion(context.Context, string, string) (HandoverCompletionVerification, error)
}

// ErrSelfApprovalForbidden 表示普通审批人试图审批自己发起或涉及本人的异动；
// 超管例外只有在服务端推导的 ApproverIsPrivileged 为真且给出原因时才被接受。
var ErrSelfApprovalForbidden = errors.New("personnel change self-approval is forbidden")

// ErrHandoverFreezeReleased 标识旧版提前完成批次的排期，需要受控恢复冻结与权威责任核对。
// 同时保留 ErrConflict 分类，供现有 HTTP 契约兼容处理。
var ErrHandoverFreezeReleased = fmt.Errorf("handover freeze was already released; controlled recovery is required before termination: %w", ErrConflict)

// PersonnelChangeTransitionInput 描述一次异动状态转移请求及其安全闸门输入。
type PersonnelChangeTransitionInput struct {
	TenantID, OperatorID, ID, ToStatus, ApprovalReference string
	// ApproverIsPrivileged 由 HTTP 层依据认证主体的权限码推导；浏览器载荷不得传入。
	// 仅用于"超管例外"审批（普通审批人必须不同于申请人与异动本人）。
	ApproverIsPrivileged bool
	// ExceptionReason 使用超管例外时必须填写，并随审批留痕（操作者、原因、命中规则）。
	ExceptionReason string
	// Immediate 允许对已排期请求跳过生效时间闸门立即执行；仅对 EXECUTED 转移有意义，
	// 审批/交接闸门与仓储幂等锁定不受影响。
	Immediate bool
}

// PermissionRole 描述异动权限预览中的一条应用角色及其授权范围。
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

// PersonnelChangePermissionPreview 是异动前后角色授权的差集/保留集预览结果。
type PersonnelChangePermissionPreview struct {
	Added    []PermissionRole `json:"added_roles"`
	Removed  []PermissionRole `json:"removed_roles"`
	Retained []PermissionRole `json:"retained_roles"`
}

// PersonnelChangeRepository 定义异动请求的持久化与权限预览行为；UpdateStatus 与 Execute
// 必须在同一事务内校验读取状态与版本，防止并发覆盖终态。
type PersonnelChangeRepository interface {
	Create(context.Context, PersonnelChangeRequest) (PersonnelChangeRequest, error)
	List(context.Context, string, string, string, string) ([]PersonnelChangeRequest, error)
	Get(context.Context, string, string) (PersonnelChangeRequest, error)
	UpdateStatus(context.Context, PersonnelChangeRequest, string, string, string, time.Time) (PersonnelChangeRequest, error)
	Execute(context.Context, PersonnelChangeRequest, string, time.Time) (PersonnelChangeRequest, error)
	PreviewPermissions(context.Context, PersonnelChangeRequest) (PersonnelChangePermissionPreview, error)
	ValidateCreate(context.Context, PersonnelChangeCreateInput) error
}

// PersonnelChangeService 编排人员异动的创建、审批、交接与执行，集中维护审批人约束、
// 超管例外留痕与离职交接闸门。
type PersonnelChangeService struct {
	repo     PersonnelChangeRepository
	ids      IDGenerator
	clock    Clock
	handover HandoverChecker
	notifier interface {
		Create(context.Context, notificationapp.CreateInput) (notificationapp.CreateResult, error)
	}
	audit       PersonnelChangeAuditSink
	coordinator HandoverCoordinator
}

// SetHandoverCoordinator 注入离职交接编排；未注入时保持既有 HandoverChecker 闸门行为。
func (s *PersonnelChangeService) SetHandoverCoordinator(coordinator HandoverCoordinator) {
	s.coordinator = coordinator
}

// SetAuditSink 在模块组装完成后注入审计写入端。例外审批的审计事实先与状态同事务写入
// 本地 outbox，再由 ApprovalExceptionDispatcher 异步投递；未注入投递端时 outbox 无法送达，
// 因此超管例外审批被拒绝（fail-closed），避免出现"用了例外却没有留痕"的路径。
func (s *PersonnelChangeService) SetAuditSink(sink PersonnelChangeAuditSink) { s.audit = sink }

// NewPersonnelChangeService 构造异动服务；任一必需依赖为 nil 或交接检查器多于一个时
// 返回错误。
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

// Create 创建异动请求并做服务端权威校验：异动类型、租约必填项与长度上限不满足时拒绝；
// 仅服务端授权的超级管理员可直排非离职异动，离职一律从 DRAFT 起步走审批与交接。
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
	if len([]rune(in.Reason)) > 500 || len(strings.TrimSpace(in.ApprovalReference)) > 128 {
		return PersonnelChangeRequest{}, fmt.Errorf("reason or approval reference exceeds the length limit: %w", ErrValidation)
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
	// 只有服务端授权的超级管理员可以直接排期非离职异动；离职必须经过审批与交接。
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

// GenerateHandoverReference 生成服务端权威的交接凭据编号（HANDOVER-日期-请求尾号）。
// 编号只承担审计索引职责；责任转移的实质证据是交接项的接收人/操作者/时间记录与
// 不可变状态轨迹，因此编号由系统生成而不是让操作者手工编造。
func GenerateHandoverReference(now time.Time, requestID string) string {
	suffix := strings.TrimSpace(requestID)
	if len(suffix) > 6 {
		suffix = suffix[len(suffix)-6:]
	}
	return fmt.Sprintf("HANDOVER-%s-%s", now.Format("20060102"), suffix)
}

func requiresTargetAssignment(changeType string) bool {
	switch changeType {
	case domain.PersonnelChangePromotion, domain.PersonnelChangeDemotion, domain.PersonnelChangeTransfer, domain.PersonnelChangeRehire:
		return true
	default:
		return false
	}
}

// List 按租户边界返回异动请求，状态、类型与关键词过滤直接下推仓储，不做内存二次裁剪。
func (s *PersonnelChangeService) List(ctx context.Context, tenant, status, changeType, keyword string) ([]PersonnelChangeRequest, error) {
	return s.repo.List(ctx, tenant, status, changeType, keyword)
}

// Get 返回租户内指定异动请求；仓储强制 tenant_id 条件，跨租户读取不会命中。
func (s *PersonnelChangeService) Get(ctx context.Context, tenant, id string) (PersonnelChangeRequest, error) {
	return s.repo.Get(ctx, tenant, id)
}

// ListHandoverItems 仅在交接检查器支持管理协议（HandoverManager）时可用；否则一律按
// 校验失败拒绝，不向未接入交接的系统暴露空列表假装完成。
func (s *PersonnelChangeService) ListHandoverItems(ctx context.Context, tenant, requestID string) ([]HandoverItem, error) {
	manager, ok := s.handover.(HandoverManager)
	if !ok || strings.TrimSpace(tenant) == "" || strings.TrimSpace(requestID) == "" {
		return nil, ErrValidation
	}
	return manager.List(ctx, tenant, requestID)
}

func (s *PersonnelChangeService) HandoverOrchestrationEnabled() bool { return s.coordinator != nil }

// CompleteHandoverItem 完成一个交接项。仅离职且处于 PENDING_HANDOVER 的请求允许操作；
// 接收人、操作者等必填项缺失时拒绝，交接完成状态由管理协议在租户边界内落库。
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
	items, err := manager.List(ctx, tenant, requestID)
	if err != nil {
		return HandoverItem{}, err
	}
	for _, item := range items {
		if item.ID != itemID {
			continue
		}
		if item.BatchID == "" {
			return manager.Complete(ctx, tenant, requestID, itemID, targetUserID, operator)
		}
		coordinator, ok := s.coordinator.(interface {
			AssignTarget(context.Context, string, string, string) error
			DispatchOnce(context.Context, string, string, int) (int, error)
		})
		if !ok {
			return HandoverItem{}, ErrConflict
		}
		if err := coordinator.AssignTarget(ctx, tenant, itemID, targetUserID); err != nil {
			return HandoverItem{}, err
		}
		if _, err := coordinator.DispatchOnce(ctx, tenant, item.BatchID, 100); err != nil {
			return HandoverItem{}, err
		}
		latest, err := manager.List(ctx, tenant, requestID)
		if err != nil {
			return HandoverItem{}, err
		}
		for _, result := range latest {
			if result.ID == itemID {
				return result, nil
			}
		}
		return HandoverItem{}, ErrNotFound
	}
	return HandoverItem{}, ErrNotFound
}

// RetryHandover 重做失败的冻结/发现，不改写远端完成状态；转移仍由逐项入口派发。
func (s *PersonnelChangeService) RetryHandover(ctx context.Context, tenant, requestID string) (HandoverBatch, error) {
	request, err := s.repo.Get(ctx, tenant, requestID)
	if err != nil {
		return HandoverBatch{}, err
	}
	if request.ChangeType != domain.PersonnelChangeTermination || request.Status != domain.PersonnelChangePendingHandover || s.coordinator == nil {
		return HandoverBatch{}, ErrConflict
	}
	return s.coordinator.Start(ctx, tenant, requestID, request.UserID)
}

func (s *PersonnelChangeService) verifyTerminationHandover(ctx context.Context, cur PersonnelChangeRequest) (string, error) {
	if s.handover == nil {
		return "", fmt.Errorf("handover checker is unavailable: %w", ErrConflict)
	}
	report, err := s.handover.Check(ctx, cur)
	if err != nil {
		return "", fmt.Errorf("handover check unavailable: %w", err)
	}
	if !report.Ready {
		return "", fmt.Errorf("responsibility handover is incomplete: %w", ErrConflict)
	}
	if s.coordinator == nil {
		return "", nil
	}
	verifier, ok := s.coordinator.(HandoverCompletionVerifier)
	if !ok {
		return "", fmt.Errorf("cross-system handover verifier is unavailable: %w", ErrConflict)
	}
	verification, err := verifier.VerifyCrossSystemCompletion(ctx, cur.TenantID, cur.ID)
	if err != nil {
		return "", fmt.Errorf("cross-system handover verification failed: %w", err)
	}
	// 旧版排期已经释放远端冻结；历史回执不能证明等待期间没有新增责任。
	if verification.BatchStatus == HandoverBatchCompleted {
		return "", ErrHandoverFreezeReleased
	}
	if !verification.BatchFound || !verification.CrossSystemComplete {
		return "", fmt.Errorf("cross-system responsibility handover is incomplete: %w", ErrConflict)
	}
	if verification.BatchID == "" || (verification.BatchStatus != HandoverBatchFrozen && verification.BatchStatus != HandoverBatchTransferring) {
		return "", fmt.Errorf("cross-system handover batch is not frozen: %w", ErrConflict)
	}
	return verification.BatchID, nil
}

// HandoverCompletionFinisher 仅提供释放已执行离职批次与有界补偿能力，不授予冻结或派发权限。
type HandoverCompletionFinisher interface {
	Complete(context.Context, string, string) error
	CompleteExecuted(context.Context, int) (int, error)
}

// CompleteExecutedHandovers 补偿已提交离职的未完成批次，不扫描历史人员请求。
func (s *PersonnelChangeService) CompleteExecutedHandovers(ctx context.Context, limit int) (int, error) {
	if s.coordinator == nil {
		return 0, nil
	}
	finisher, ok := s.coordinator.(HandoverCompletionFinisher)
	if !ok {
		return 0, fmt.Errorf("handover completion retry is unavailable: %w", ErrConflict)
	}
	return finisher.CompleteExecuted(ctx, limit)
}

// Transition 推进异动状态。闸门包括：领域状态机、审批人不得自批（超管例外必须由特权主体
// 提出且留下审计记录）、审批凭据必填、离职必须先交接完成再排期、执行只接受已到生效时间的
// 请求，取消前必须先与各系统确认没有在途转移。任何闸门失败即拒绝，不做部分推进。
func (s *PersonnelChangeService) Transition(ctx context.Context, in PersonnelChangeTransitionInput) (PersonnelChangeRequest, error) {
	if in.TenantID == "" || in.OperatorID == "" || in.ID == "" || in.ToStatus == "" {
		return PersonnelChangeRequest{}, ErrValidation
	}
	if len(strings.TrimSpace(in.ApprovalReference)) > 128 || len([]rune(strings.TrimSpace(in.ExceptionReason))) > 500 {
		return PersonnelChangeRequest{}, fmt.Errorf("transition reason or reference exceeds the length limit: %w", ErrValidation)
	}
	// 立即执行仅对 EXECUTED 转移有意义；其他转移携带 immediate 一律拒绝，
	// 防止调用方把它当作跳过审批或交接阶段的开关。
	if in.Immediate && in.ToStatus != domain.PersonnelChangeExecuted {
		return PersonnelChangeRequest{}, fmt.Errorf("immediate execution only applies to EXECUTED: %w", ErrValidation)
	}
	cur, err := s.repo.Get(ctx, in.TenantID, in.ID)
	if err != nil {
		return PersonnelChangeRequest{}, err
	}
	if !domain.CanTransitionPersonnelChange(cur.Status, in.ToStatus) {
		return PersonnelChangeRequest{}, ErrConflict
	}
	// 显式审批人约束：普通审批人必须同时不同于申请人与异动本人。超管例外只在服务端
	// 判定为特权主体（ApproverIsPrivileged，由认证主体推导）且给出原因时才成立，
	// 且只豁免这一条——交接、租户、版本与有效期闸门照旧执行。
	exceptionUsed := false
	if cur.Status == domain.PersonnelChangePendingApproval && (in.ToStatus == domain.PersonnelChangePendingHandover || in.ToStatus == domain.PersonnelChangeScheduled || in.ToStatus == domain.PersonnelChangeRejected) {
		if in.OperatorID == cur.SubmittedBy || in.OperatorID == cur.UserID {
			if !in.ApproverIsPrivileged {
				return PersonnelChangeRequest{}, fmt.Errorf("approver must differ from the applicant and the subject: %w", ErrSelfApprovalForbidden)
			}
			if strings.TrimSpace(in.ExceptionReason) == "" {
				return PersonnelChangeRequest{}, fmt.Errorf("privileged self-approval requires an exception reason: %w", ErrValidation)
			}
			// 例外必须可留痕：没有审计写入端时直接拒绝，不产生"用了例外却无记录"的审批。
			// outbox 与审批状态同事务提交，审计端由 dispatcher 异步投递；没有投递端时
			// outbox 永远无法送达，同样按 fail-closed 拒绝。
			if s.audit == nil {
				return PersonnelChangeRequest{}, fmt.Errorf("approval exception audit sink is unavailable: %w", ErrConflict)
			}
			if _, ok := s.repo.(PersonnelChangeApprovalExceptionWriter); !ok {
				return PersonnelChangeRequest{}, fmt.Errorf("approval exception outbox store is unavailable: %w", ErrConflict)
			}
			exceptionUsed = true
		}
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
		// 离职交接排期：凭据编号只是审计索引，责任转移的证据是交接项记录与不可变
		// 状态轨迹，因此空凭据由服务端权威生成（前端不再要求手工编造编号）；调用方
		// 显式传入的外部交接编号仍然接受，但必须保持 HANDOVER- 前缀。
		if cur.ChangeType == domain.PersonnelChangeTermination && cur.Status == domain.PersonnelChangePendingHandover {
			if strings.TrimSpace(in.ApprovalReference) == "" {
				in.ApprovalReference = GenerateHandoverReference(s.clock.Now().UTC(), cur.ID)
			} else if !strings.HasPrefix(strings.ToUpper(strings.TrimSpace(in.ApprovalReference)), "HANDOVER-") {
				return PersonnelChangeRequest{}, fmt.Errorf("termination requires a HANDOVER-* reference: %w", ErrConflict)
			}
		} else if strings.TrimSpace(in.ApprovalReference) == "" {
			return PersonnelChangeRequest{}, fmt.Errorf("approval reference is required: %w", ErrValidation)
		}
		if cur.ChangeType == domain.PersonnelChangeTermination {
			if _, err := s.verifyTerminationHandover(ctx, cur); err != nil {
				return PersonnelChangeRequest{}, err
			}
		}
	}
	if in.ToStatus == domain.PersonnelChangeExecuted {
		var batchID string
		var finisher HandoverCompletionFinisher
		if cur.ChangeType == domain.PersonnelChangeTermination {
			batchID, err = s.verifyTerminationHandover(ctx, cur)
			if err != nil {
				return PersonnelChangeRequest{}, err
			}
			if s.coordinator != nil {
				var ok bool
				finisher, ok = s.coordinator.(HandoverCompletionFinisher)
				if !ok {
					return PersonnelChangeRequest{}, fmt.Errorf("handover completion retry is unavailable: %w", ErrConflict)
				}
			}
		}
		// 立即执行只豁免生效时间闸门（审批与离职交接闸门在排期前已通过）；
		// 其余状态转移不接受 immediate 语义，避免调用方误以为可以跳过阶段。
		// 执行只接受已到生效时间的请求（立即执行除外），防止提前变更身份与权限。
		if !in.Immediate && (cur.EffectiveAt == nil || cur.EffectiveAt.After(s.clock.Now().UTC())) {
			return PersonnelChangeRequest{}, fmt.Errorf("personnel change is not yet effective: %w", ErrConflict)
		}
		result, execErr := s.repo.Execute(ctx, cur, in.OperatorID, s.clock.Now().UTC())
		if execErr == nil && finisher != nil {
			if finishErr := finisher.Complete(ctx, cur.TenantID, batchID); finishErr != nil {
				// EXECUTED 已提交，不得伪报本地失败。批次保留 FROZEN/TRANSFERRING，
				// 与已提交请求的关联就是持久待完成队列，worker 重启后仍能补偿。
				slog.WarnContext(ctx, "personnel termination committed; handover completion pending retry", "tenant_id", cur.TenantID, "request_id", cur.ID, "batch_id", batchID, "error", finishErr)
			}
		}
		if execErr == nil && s.notifier != nil {
			// 只有仓储已经持久化转移结果后才发通知；重试或执行失败不得宣布虚假完成。
			s.notify(ctx, result, in.OperatorID)
		}
		return result, execErr
	}
	// 取消必须在宣布取消之前先与各系统确认没有在途操作：取消失败时不得推进状态，
	// 否则会出现"平台显示已取消、远端仍在转移"的分裂。
	if in.ToStatus == domain.PersonnelChangeCancelled && s.coordinator != nil && cur.ChangeType == domain.PersonnelChangeTermination {
		if cancelErr := s.coordinator.CancelByRequest(ctx, cur.TenantID, cur.ID); cancelErr != nil {
			return PersonnelChangeRequest{}, cancelErr
		}
	}
	// 审批状态、例外原因与审计 outbox 必须同事务提交。例外事件 ID 在写入前生成并
	// 随 outbox 持久化，异步投递重试复用同一 ID，接收端据此幂等去重。
	now := s.clock.Now().UTC()
	var exception *PersonnelChangeAuditEvent
	if exceptionUsed {
		eventID, idErr := s.ids.New(now)
		if idErr != nil {
			return PersonnelChangeRequest{}, fmt.Errorf("generate approval exception audit event id: %w", idErr)
		}
		event := PersonnelChangeAuditEvent{
			EventID: eventID, TenantID: cur.TenantID, OperatorID: in.OperatorID, RequestID: cur.ID, ToStatus: in.ToStatus,
			SubmittedBy: cur.SubmittedBy, SubjectUserID: cur.UserID, Reason: strings.TrimSpace(in.ExceptionReason),
			RuleHit: "SELF_APPROVAL_EXCEPTION", ApprovalReference: in.ApprovalReference, OccurredAt: now,
		}
		exception = &event
	}
	// 仓储同时校验读取到的旧状态和版本，避免取消/审批与 worker 执行并发时
	// 后提交的一方覆盖已经完成的终态。
	updated, updateErr := s.persistTransition(ctx, cur, in, now, exception)
	if updateErr != nil {
		return updated, updateErr
	}
	// 离职进入交接阶段后立即创建交接批次并完成冻结与责任发现；EnsureBatch 幂等，重试安全。
	if in.ToStatus == domain.PersonnelChangePendingHandover && updated.ChangeType == domain.PersonnelChangeTermination && s.coordinator != nil {
		if _, startErr := s.coordinator.Start(ctx, updated.TenantID, updated.ID, updated.UserID); startErr != nil {
			return updated, fmt.Errorf("start responsibility handover: %w", startErr)
		}
	}
	return updated, nil
}

// persistTransition 把状态推进交给仓储。使用超管例外时要求仓储提供"状态 + outbox"
// 同事务写入能力；缺少该能力时不提交任何状态，避免接口成功但审计事实缺失。
func (s *PersonnelChangeService) persistTransition(ctx context.Context, cur PersonnelChangeRequest, in PersonnelChangeTransitionInput, now time.Time, exception *PersonnelChangeAuditEvent) (PersonnelChangeRequest, error) {
	if exception == nil {
		return s.repo.UpdateStatus(ctx, cur, in.ToStatus, in.ApprovalReference, in.OperatorID, now)
	}
	store, ok := s.repo.(PersonnelChangeApprovalExceptionWriter)
	if !ok {
		return PersonnelChangeRequest{}, fmt.Errorf("approval exception outbox store is unavailable: %w", ErrConflict)
	}
	return store.UpdateStatusWithApprovalException(ctx, cur, in.ToStatus, in.ApprovalReference, in.OperatorID, now, *exception)
}

// Preview 返回已落库请求的权限影响差异（新增/移除/保留角色）。差异由仓储按真实授权来源
// 计算，预览不产生任何变更。
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
