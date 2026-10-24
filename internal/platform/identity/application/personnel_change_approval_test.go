package application

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/J-S-Te/Basic-Platform/internal/platform/identity/domain"
)

// approvalRepository 只服务审批分支用例：状态更新一律成功，便于区分"被闸门拒绝"与"落库成功"。
// UpdateStatusWithApprovalException 模拟仓储的同事务语义：状态与 outbox 要么一起成功，
// 要么一起不落库。
type approvalRepository struct {
	request            PersonnelChangeRequest
	approvalExceptions []PersonnelChangeAuditEvent
	atomicErr          error
}

var _ PersonnelChangeApprovalExceptionWriter = (*approvalRepository)(nil)

func (r *approvalRepository) Create(context.Context, PersonnelChangeRequest) (PersonnelChangeRequest, error) {
	return PersonnelChangeRequest{}, errors.New("unexpected create")
}
func (r *approvalRepository) List(context.Context, string, string, string, string) ([]PersonnelChangeRequest, error) {
	return nil, nil
}
func (r *approvalRepository) Get(context.Context, string, string) (PersonnelChangeRequest, error) {
	return r.request, nil
}
func (r *approvalRepository) UpdateStatus(_ context.Context, request PersonnelChangeRequest, status, _ string, _ string, now time.Time) (PersonnelChangeRequest, error) {
	r.request = request
	r.request.Status = status
	r.request.UpdatedAt = now
	return r.request, nil
}
func (r *approvalRepository) UpdateStatusWithApprovalException(_ context.Context, expected PersonnelChangeRequest, status, ref, operator string, now time.Time, exception PersonnelChangeAuditEvent) (PersonnelChangeRequest, error) {
	if r.atomicErr != nil {
		return PersonnelChangeRequest{}, r.atomicErr
	}
	updated, err := r.UpdateStatus(context.Background(), expected, status, ref, operator, now)
	if err != nil {
		return updated, err
	}
	r.approvalExceptions = append(r.approvalExceptions, exception)
	return updated, nil
}
func (r *approvalRepository) Execute(context.Context, PersonnelChangeRequest, string, time.Time) (PersonnelChangeRequest, error) {
	return PersonnelChangeRequest{}, errors.New("unexpected execute")
}
func (r *approvalRepository) PreviewPermissions(context.Context, PersonnelChangeRequest) (PersonnelChangePermissionPreview, error) {
	return PersonnelChangePermissionPreview{}, nil
}
func (r *approvalRepository) ValidateCreate(context.Context, PersonnelChangeCreateInput) error {
	return nil
}

func approvalTestService(t *testing.T, repository *approvalRepository) *PersonnelChangeService {
	t.Helper()
	service, err := NewPersonnelChangeService(repository, personnelChangeLifecycleIDGenerator{}, personnelChangeLifecycleClock{now: time.Date(2026, 10, 16, 10, 0, 0, 0, time.UTC)})
	if err != nil {
		t.Fatal(err)
	}
	return service
}

func pendingApprovalRequest(changeType string) PersonnelChangeRequest {
	return PersonnelChangeRequest{
		ID: "change-1", TenantID: "tenant-1", UserID: "user-1", SubmittedBy: "applicant-1",
		ChangeType: changeType, Status: domain.PersonnelChangePendingApproval, Version: 1,
	}
}

func transitionToScheduled(t *testing.T, request PersonnelChangeRequest, in PersonnelChangeTransitionInput) (PersonnelChangeRequest, error) {
	t.Helper()
	service := approvalTestService(t, &approvalRepository{request: request})
	in.TenantID, in.ID, in.ToStatus = "tenant-1", request.ID, domain.PersonnelChangeScheduled
	if in.ApprovalReference == "" {
		in.ApprovalReference = "APR-1001"
	}
	return service.Transition(context.Background(), in)
}

// 普通审批人不能审批自己发起的异动。
func TestTransitionRejectsApprovalByApplicant(t *testing.T) {
	if _, err := transitionToScheduled(t, pendingApprovalRequest(domain.PersonnelChangeTransfer), PersonnelChangeTransitionInput{OperatorID: "applicant-1"}); !errors.Is(err, ErrSelfApprovalForbidden) {
		t.Fatalf("err = %v, want ErrSelfApprovalForbidden for the applicant", err)
	}
}

// 普通审批人也不能审批涉及本人的异动。
func TestTransitionRejectsApprovalBySubject(t *testing.T) {
	if _, err := transitionToScheduled(t, pendingApprovalRequest(domain.PersonnelChangeTransfer), PersonnelChangeTransitionInput{OperatorID: "user-1"}); !errors.Is(err, ErrSelfApprovalForbidden) {
		t.Fatalf("err = %v, want ErrSelfApprovalForbidden for the subject", err)
	}
}

// 独立审批人照常放行。
func TestTransitionAllowsIndependentApprover(t *testing.T) {
	result, err := transitionToScheduled(t, pendingApprovalRequest(domain.PersonnelChangeTransfer), PersonnelChangeTransitionInput{OperatorID: "approver-2"})
	if err != nil {
		t.Fatalf("independent approver must be accepted: %v", err)
	}
	if result.Status != domain.PersonnelChangeScheduled {
		t.Fatalf("status = %s, want SCHEDULED", result.Status)
	}
}

// 超管例外必须给出原因，否则按校验失败拒绝（不能凭一个布尔开关静默自审）。
func TestTransitionPrivilegedExceptionRequiresReason(t *testing.T) {
	if _, err := transitionToScheduled(t, pendingApprovalRequest(domain.PersonnelChangeTransfer), PersonnelChangeTransitionInput{OperatorID: "applicant-1", ApproverIsPrivileged: true}); err == nil || errors.Is(err, ErrSelfApprovalForbidden) || !errors.Is(err, ErrValidation) {
		t.Fatalf("err = %v, want validation failure requiring an exception reason", err)
	}
}

// 例外只豁免"审批人必须不同"这一条，不豁免交接、租户、版本或有效期闸门。
func TestTransitionPrivilegedExceptionDoesNotBypassHandoverGate(t *testing.T) {
	service := approvalTestService(t, &approvalRepository{request: pendingApprovalRequest(domain.PersonnelChangeTermination)})
	service.SetAuditSink(&approvalAuditSinkStub{})
	_, err := service.Transition(context.Background(), PersonnelChangeTransitionInput{
		TenantID: "tenant-1", ID: "change-1", ToStatus: domain.PersonnelChangeScheduled, ApprovalReference: "APR-1001",
		OperatorID: "applicant-1", ApproverIsPrivileged: true, ExceptionReason: "唯一在岗审批人，已按例外流程复核",
	})
	if !errors.Is(err, ErrConflict) {
		t.Fatalf("err = %v, want the termination handover gate to still block scheduling", err)
	}
}

type approvalAuditSinkStub struct {
	events []PersonnelChangeAuditEvent
	err    error
}

func (s *approvalAuditSinkStub) RecordApprovalException(_ context.Context, event PersonnelChangeAuditEvent) error {
	if s.err != nil {
		return s.err
	}
	s.events = append(s.events, event)
	return nil
}

func privilegedTransitionService(t *testing.T, sink PersonnelChangeAuditSink) (*PersonnelChangeService, *approvalRepository) {
	t.Helper()
	repository := &approvalRepository{request: pendingApprovalRequest(domain.PersonnelChangeTransfer)}
	service := approvalTestService(t, repository)
	if sink != nil {
		service.SetAuditSink(sink)
	}
	return service, repository
}

func privilegedTransitionInput() PersonnelChangeTransitionInput {
	return PersonnelChangeTransitionInput{
		TenantID: "tenant-1", ID: "change-1", ToStatus: domain.PersonnelChangeScheduled, ApprovalReference: "APR-1001",
		OperatorID: "applicant-1", ApproverIsPrivileged: true, ExceptionReason: "唯一在岗审批人，已按例外流程复核",
	}
}

// 超管例外必须留下可核对的记录：操作者、原因、命中规则与稳定审计事件 ID。
// 记录必须与审批状态在同一事务写入本地 outbox；外部审计只能异步投递，
// 因此这里故意让审计端失败，仍应审批成功且不产生同步投递。
func TestTransitionPrivilegedExceptionPersistsAtomicOutbox(t *testing.T) {
	sink := &approvalAuditSinkStub{err: errors.New("audit sink must not be called synchronously")}
	service, repository := privilegedTransitionService(t, sink)
	if _, err := service.Transition(context.Background(), privilegedTransitionInput()); err != nil {
		t.Fatalf("privileged exception must be accepted once the outbox is persisted: %v", err)
	}
	if repository.request.Status != domain.PersonnelChangeScheduled {
		t.Fatalf("status = %s, want SCHEDULED", repository.request.Status)
	}
	if len(repository.approvalExceptions) != 1 {
		t.Fatalf("outbox entries = %d, want exactly one", len(repository.approvalExceptions))
	}
	event := repository.approvalExceptions[0]
	if event.EventID == "" || event.OperatorID != "applicant-1" || event.RuleHit != "SELF_APPROVAL_EXCEPTION" || event.Reason == "" ||
		event.RequestID != "change-1" || event.ToStatus != domain.PersonnelChangeScheduled ||
		event.SubmittedBy != "applicant-1" || event.SubjectUserID != "user-1" || event.OccurredAt.IsZero() {
		t.Fatalf("outbox audit trail incomplete: %+v", event)
	}
	if len(sink.events) != 0 {
		t.Fatalf("synchronous audit deliveries = %d, want 0", len(sink.events))
	}
}

// 没有审计写入端时不得使用例外（fail-closed），且状态不得变更。
func TestTransitionPrivilegedExceptionFailsClosedWithoutAuditSink(t *testing.T) {
	service, repository := privilegedTransitionService(t, nil)
	if _, err := service.Transition(context.Background(), privilegedTransitionInput()); !errors.Is(err, ErrConflict) {
		t.Fatalf("err = %v, want ErrConflict when the audit sink is unavailable", err)
	}
	if repository.request.Status != domain.PersonnelChangePendingApproval {
		t.Fatalf("status = %s, want the request to stay in PENDING_APPROVAL", repository.request.Status)
	}
}

// outbox 写入失败时状态必须同时回滚：不能出现审批已提交却没有任何例外原因/审计事实。
func TestTransitionPrivilegedExceptionRollsBackWhenOutboxWriteFails(t *testing.T) {
	sink := &approvalAuditSinkStub{}
	service, repository := privilegedTransitionService(t, sink)
	repository.atomicErr = errors.New("outbox insert failed")
	_, err := service.Transition(context.Background(), privilegedTransitionInput())
	if err == nil || !strings.Contains(err.Error(), "outbox") && !strings.Contains(err.Error(), "audit") {
		t.Fatalf("err = %v, want an explicit outbox write failure", err)
	}
	if repository.request.Status != domain.PersonnelChangePendingApproval {
		t.Fatalf("status = %s, want the request to stay in PENDING_APPROVAL", repository.request.Status)
	}
	if len(repository.approvalExceptions) != 0 {
		t.Fatalf("outbox entries = %d, want none after a failed transaction", len(repository.approvalExceptions))
	}
	if len(sink.events) != 0 {
		t.Fatalf("synchronous audit deliveries = %d, want 0", len(sink.events))
	}
}

// 仓储不支持与状态同事务写 outbox 时，超管例外必须失败关闭而不是只提交状态。
func TestTransitionPrivilegedExceptionRequiresAtomicOutboxRepository(t *testing.T) {
	service, err := NewPersonnelChangeService(&personnelChangeTransitionRepository{request: pendingApprovalRequest(domain.PersonnelChangeTransfer)}, personnelChangeLifecycleIDGenerator{}, personnelChangeLifecycleClock{now: time.Date(2026, 10, 16, 10, 0, 0, 0, time.UTC)})
	if err != nil {
		t.Fatal(err)
	}
	service.SetAuditSink(&approvalAuditSinkStub{})
	if _, err := service.Transition(context.Background(), privilegedTransitionInput()); !errors.Is(err, ErrConflict) {
		t.Fatalf("err = %v, want ErrConflict when the repository cannot persist the outbox atomically", err)
	}
}
