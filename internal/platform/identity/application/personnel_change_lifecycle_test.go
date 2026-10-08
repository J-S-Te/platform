package application

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/J-S-Te/Basic-Platform/internal/platform/identity/domain"
	notificationapp "github.com/J-S-Te/Basic-Platform/internal/platform/notification/application"
)

type personnelChangeCreateRepository struct {
	created       PersonnelChangeRequest
	validationErr error
}

func (r *personnelChangeCreateRepository) Create(_ context.Context, request PersonnelChangeRequest) (PersonnelChangeRequest, error) {
	r.created = request
	return request, nil
}
func (r *personnelChangeCreateRepository) List(context.Context, string, string, string, string) ([]PersonnelChangeRequest, error) {
	return nil, nil
}
func (r *personnelChangeCreateRepository) Get(context.Context, string, string) (PersonnelChangeRequest, error) {
	return PersonnelChangeRequest{}, ErrConflict
}
func (r *personnelChangeCreateRepository) UpdateStatus(context.Context, PersonnelChangeRequest, string, string, string, time.Time) (PersonnelChangeRequest, error) {
	return PersonnelChangeRequest{}, ErrConflict
}
func (r *personnelChangeCreateRepository) Execute(context.Context, PersonnelChangeRequest, string, time.Time) (PersonnelChangeRequest, error) {
	return PersonnelChangeRequest{}, ErrConflict
}
func (r *personnelChangeCreateRepository) PreviewPermissions(context.Context, PersonnelChangeRequest) (PersonnelChangePermissionPreview, error) {
	return PersonnelChangePermissionPreview{}, nil
}
func (r *personnelChangeCreateRepository) ValidateCreate(context.Context, PersonnelChangeCreateInput) error {
	return r.validationErr
}

type personnelChangeLifecycleIDGenerator struct{}

func (personnelChangeLifecycleIDGenerator) New(time.Time) (string, error) {
	return "01J00000000000000000000001", nil
}

type personnelChangeLifecycleClock struct{ now time.Time }

func (c personnelChangeLifecycleClock) Now() time.Time { return c.now }

func newPersonnelChangeForCreateTest(t *testing.T, repository *personnelChangeCreateRepository) *PersonnelChangeService {
	t.Helper()
	service, err := NewPersonnelChangeService(repository, personnelChangeLifecycleIDGenerator{}, personnelChangeLifecycleClock{now: time.Date(2026, 8, 25, 10, 0, 0, 0, time.UTC)})
	if err != nil {
		t.Fatal(err)
	}
	return service
}

func personnelChangeCreateInput(directScheduleAuthorized bool) PersonnelChangeCreateInput {
	return PersonnelChangeCreateInput{
		TenantID: "tenant-1", OperatorID: "operator-1", UserID: "user-1",
		SourceMembershipID: "membership-1", TargetOrgUnitID: "org-1", TargetPositionID: "position-1",
		ChangeType: domain.PersonnelChangeTransfer, Reason: "业务调整",
		EffectiveAt: time.Date(2026, 8, 26, 10, 0, 0, 0, time.UTC), DirectScheduleAuthorized: directScheduleAuthorized,
	}
}

func TestPersonnelChangeCreateValidatesFieldsByChangeType(t *testing.T) {
	tests := []struct {
		name    string
		change  string
		mutate  func(*PersonnelChangeCreateInput)
		wantErr bool
	}{
		{name: "transfer requires source membership", change: domain.PersonnelChangeTransfer, mutate: func(in *PersonnelChangeCreateInput) { in.SourceMembershipID = "" }, wantErr: true},
		{name: "promotion requires target assignment", change: domain.PersonnelChangePromotion, mutate: func(in *PersonnelChangeCreateInput) { in.TargetPositionID = "" }, wantErr: true},
		{name: "rehire requires target assignment", change: domain.PersonnelChangeRehire, mutate: func(in *PersonnelChangeCreateInput) {
			in.SourceMembershipID = ""
			in.TargetOrgUnitID = ""
			in.TargetPositionID = ""
		}, wantErr: true},
		{name: "termination requires source membership", change: domain.PersonnelChangeTermination, mutate: func(in *PersonnelChangeCreateInput) {
			in.SourceMembershipID = ""
			in.TargetOrgUnitID = ""
			in.TargetPositionID = ""
		}, wantErr: true},
		{name: "termination does not require target assignment", change: domain.PersonnelChangeTermination, mutate: func(in *PersonnelChangeCreateInput) {
			in.TargetOrgUnitID = ""
			in.TargetPositionID = ""
		}, wantErr: false},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			repository := &personnelChangeCreateRepository{}
			service := newPersonnelChangeForCreateTest(t, repository)
			input := personnelChangeCreateInput(false)
			input.ChangeType = test.change
			test.mutate(&input)
			_, err := service.Create(context.Background(), input)
			if (err != nil) != test.wantErr {
				t.Fatalf("Create(%s) error = %v, wantErr=%v", test.change, err, test.wantErr)
			}
		})
	}
}

func TestPersonnelChangeCreateRejectsRepositoryIdentityValidation(t *testing.T) {
	repository := &personnelChangeCreateRepository{validationErr: ErrValidation}
	service := newPersonnelChangeForCreateTest(t, repository)
	_, err := service.Create(context.Background(), personnelChangeCreateInput(false))
	if !errors.Is(err, ErrValidation) {
		t.Fatalf("error=%v, want ErrValidation", err)
	}
	if repository.created.ID != "" {
		t.Fatal("invalid request must not be persisted")
	}
}

func TestPersonnelChangeCreateStartsDraftForRegularAdministrator(t *testing.T) {
	repository := &personnelChangeCreateRepository{}
	service := newPersonnelChangeForCreateTest(t, repository)

	created, err := service.Create(context.Background(), personnelChangeCreateInput(false))
	if err != nil {
		t.Fatal(err)
	}
	if created.Status != domain.PersonnelChangeDraft || repository.created.Status != domain.PersonnelChangeDraft {
		t.Fatalf("status=%q repository status=%q, want DRAFT", created.Status, repository.created.Status)
	}
}

func TestPersonnelChangeCreateAllowsSuperAdminToScheduleNonTermination(t *testing.T) {
	repository := &personnelChangeCreateRepository{}
	service := newPersonnelChangeForCreateTest(t, repository)

	created, err := service.Create(context.Background(), personnelChangeCreateInput(true))
	if err != nil {
		t.Fatal(err)
	}
	if created.Status != domain.PersonnelChangeScheduled || repository.created.Status != domain.PersonnelChangeScheduled {
		t.Fatalf("status=%q repository status=%q, want SCHEDULED", created.Status, repository.created.Status)
	}
	if domain.CanTransitionPersonnelChange(created.Status, domain.PersonnelChangePendingApproval) {
		t.Fatal("directly scheduled request must not be submitted for approval")
	}
}

func TestPersonnelChangeCreateTerminationAlwaysStartsDraft(t *testing.T) {
	repository := &personnelChangeCreateRepository{}
	service := newPersonnelChangeForCreateTest(t, repository)
	input := personnelChangeCreateInput(true)
	input.ChangeType = domain.PersonnelChangeTermination
	input.TargetOrgUnitID = ""
	input.TargetPositionID = ""
	created, err := service.Create(context.Background(), input)
	if err != nil {
		t.Fatal(err)
	}
	if created.Status != domain.PersonnelChangeDraft {
		t.Fatalf("status=%q, want DRAFT", created.Status)
	}
}

func TestPersonnelChangeTerminationCannotSkipHandoverAfterApproval(t *testing.T) {
	repository := &personnelChangeExecutionRepository{request: PersonnelChangeRequest{
		ID: "change-termination", TenantID: "tenant-1", UserID: "user-1",
		ChangeType: domain.PersonnelChangeTermination, Status: domain.PersonnelChangePendingApproval,
	}}
	service, err := NewPersonnelChangeService(repository, personnelChangeLifecycleIDGenerator{}, personnelChangeLifecycleClock{now: time.Date(2026, 8, 25, 10, 0, 0, 0, time.UTC)})
	if err != nil {
		t.Fatal(err)
	}
	_, err = service.Transition(context.Background(), PersonnelChangeTransitionInput{
		TenantID: "tenant-1", OperatorID: "approver-1", ID: "change-termination",
		ToStatus: domain.PersonnelChangeScheduled, ApprovalReference: "HANDOVER-1001",
	})
	if !errors.Is(err, ErrConflict) {
		t.Fatalf("error=%v, want ErrConflict", err)
	}
}

func TestPersonnelChangeApprovalRequiresPersistentReference(t *testing.T) {
	repository := &personnelChangeExecutionRepository{request: PersonnelChangeRequest{
		ID: "change-transfer", TenantID: "tenant-1", UserID: "user-1",
		ChangeType: domain.PersonnelChangeTransfer, Status: domain.PersonnelChangePendingApproval,
	}}
	service, err := NewPersonnelChangeService(repository, personnelChangeLifecycleIDGenerator{}, personnelChangeLifecycleClock{now: time.Date(2026, 8, 25, 10, 0, 0, 0, time.UTC)})
	if err != nil {
		t.Fatal(err)
	}
	_, err = service.Transition(context.Background(), PersonnelChangeTransitionInput{
		TenantID: "tenant-1", OperatorID: "approver-1", ID: "change-transfer", ToStatus: domain.PersonnelChangeScheduled,
	})
	if !errors.Is(err, ErrValidation) {
		t.Fatalf("error=%v, want ErrValidation", err)
	}
}

type personnelChangeExecutionRepository struct {
	request PersonnelChangeRequest
	err     error
}

func (r *personnelChangeExecutionRepository) Create(context.Context, PersonnelChangeRequest) (PersonnelChangeRequest, error) {
	return PersonnelChangeRequest{}, errors.New("unexpected create")
}
func (r *personnelChangeExecutionRepository) List(context.Context, string, string, string, string) ([]PersonnelChangeRequest, error) {
	return nil, nil
}
func (r *personnelChangeExecutionRepository) Get(context.Context, string, string) (PersonnelChangeRequest, error) {
	return r.request, nil
}
func (r *personnelChangeExecutionRepository) UpdateStatus(context.Context, PersonnelChangeRequest, string, string, string, time.Time) (PersonnelChangeRequest, error) {
	return PersonnelChangeRequest{}, errors.New("unexpected status update")
}
func (r *personnelChangeExecutionRepository) Execute(_ context.Context, request PersonnelChangeRequest, _ string, now time.Time) (PersonnelChangeRequest, error) {
	if r.err != nil {
		return PersonnelChangeRequest{}, r.err
	}
	r.request = request
	r.request.Status = domain.PersonnelChangeExecuted
	r.request.ExecutedAt = &now
	return r.request, nil
}
func (r *personnelChangeExecutionRepository) PreviewPermissions(context.Context, PersonnelChangeRequest) (PersonnelChangePermissionPreview, error) {
	return PersonnelChangePermissionPreview{}, nil
}
func (r *personnelChangeExecutionRepository) ValidateCreate(context.Context, PersonnelChangeCreateInput) error {
	return nil
}

type personnelChangeNotifier struct {
	created []notificationapp.CreateInput
}

func (n *personnelChangeNotifier) Create(_ context.Context, input notificationapp.CreateInput) (notificationapp.CreateResult, error) {
	n.created = append(n.created, input)
	return notificationapp.CreateResult{}, nil
}

func TestPersonnelChangeNotifiesOnlyAfterSuccessfulExecution(t *testing.T) {
	now := time.Date(2026, 8, 25, 10, 0, 0, 0, time.UTC)
	effectiveAt := now.Add(-time.Minute)
	repository := &personnelChangeExecutionRepository{request: PersonnelChangeRequest{ID: "change-1", TenantID: "tenant-1", UserID: "user-1", TargetOrgUnitID: "org-1", ChangeType: domain.PersonnelChangeTransfer, Reason: "业务调整", Status: domain.PersonnelChangeScheduled, EffectiveAt: &effectiveAt}}
	notifier := &personnelChangeNotifier{}
	service, err := NewPersonnelChangeService(repository, personnelChangeLifecycleIDGenerator{}, personnelChangeLifecycleClock{now: now})
	if err != nil {
		t.Fatal(err)
	}
	service.SetNotifier(notifier)

	if _, err := service.Transition(context.Background(), PersonnelChangeTransitionInput{TenantID: "tenant-1", OperatorID: "operator-1", ID: "change-1", ToStatus: domain.PersonnelChangeExecuted}); err != nil {
		t.Fatal(err)
	}
	if len(notifier.created) != 1 {
		t.Fatalf("notification count=%d, want 1", len(notifier.created))
	}
	if notifier.created[0].ReferenceID != "change-1" || notifier.created[0].TemplateCode != "personnel_change_executed" {
		t.Fatalf("notification=%+v, want executed event for change-1", notifier.created[0])
	}
}

func TestPersonnelChangeDoesNotNotifyWhenExecutionFails(t *testing.T) {
	now := time.Date(2026, 8, 25, 10, 0, 0, 0, time.UTC)
	effectiveAt := now.Add(-time.Minute)
	repository := &personnelChangeExecutionRepository{request: PersonnelChangeRequest{ID: "change-1", TenantID: "tenant-1", UserID: "user-1", ChangeType: domain.PersonnelChangeTransfer, Reason: "业务调整", Status: domain.PersonnelChangeScheduled, EffectiveAt: &effectiveAt}, err: errors.New("persist failed")}
	notifier := &personnelChangeNotifier{}
	service, err := NewPersonnelChangeService(repository, personnelChangeLifecycleIDGenerator{}, personnelChangeLifecycleClock{now: now})
	if err != nil {
		t.Fatal(err)
	}
	service.SetNotifier(notifier)

	if _, err := service.Transition(context.Background(), PersonnelChangeTransitionInput{TenantID: "tenant-1", OperatorID: "operator-1", ID: "change-1", ToStatus: domain.PersonnelChangeExecuted}); err == nil {
		t.Fatal("expected execution failure")
	}
	if len(notifier.created) != 0 {
		t.Fatalf("notification count=%d, want 0 after failed execution", len(notifier.created))
	}
}

func TestPersonnelChangeImmediateExecutionSkipsEffectiveTimeGate(t *testing.T) {
	now := time.Date(2026, 8, 25, 10, 0, 0, 0, time.UTC)
	// 生效时间在未来：常规执行必须被时间闸门拒绝，立即执行应放行到仓储。
	effectiveAt := now.Add(24 * time.Hour)
	repository := &personnelChangeExecutionRepository{request: PersonnelChangeRequest{ID: "change-1", TenantID: "tenant-1", UserID: "user-1", TargetOrgUnitID: "org-1", ChangeType: domain.PersonnelChangeTransfer, Reason: "业务调整", Status: domain.PersonnelChangeScheduled, EffectiveAt: &effectiveAt}}
	notifier := &personnelChangeNotifier{}
	service, err := NewPersonnelChangeService(repository, personnelChangeLifecycleIDGenerator{}, personnelChangeLifecycleClock{now: now})
	if err != nil {
		t.Fatal(err)
	}
	service.SetNotifier(notifier)

	if _, err := service.Transition(context.Background(), PersonnelChangeTransitionInput{TenantID: "tenant-1", OperatorID: "operator-1", ID: "change-1", ToStatus: domain.PersonnelChangeExecuted}); !errors.Is(err, ErrConflict) {
		t.Fatalf("scheduled execution before effective time err=%v, want ErrConflict", err)
	}
	result, err := service.Transition(context.Background(), PersonnelChangeTransitionInput{TenantID: "tenant-1", OperatorID: "operator-1", ID: "change-1", ToStatus: domain.PersonnelChangeExecuted, Immediate: true})
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != domain.PersonnelChangeExecuted || result.ExecutedAt == nil {
		t.Fatalf("result=%+v, want executed with executed_at", result)
	}
	if len(notifier.created) != 1 {
		t.Fatalf("notification count=%d, want 1 after immediate execution", len(notifier.created))
	}
}

func TestPersonnelChangeImmediateFlagRejectedForNonExecutedStatus(t *testing.T) {
	now := time.Date(2026, 8, 25, 10, 0, 0, 0, time.UTC)
	effectiveAt := now.Add(-time.Minute)
	repository := &personnelChangeExecutionRepository{request: PersonnelChangeRequest{ID: "change-1", TenantID: "tenant-1", UserID: "user-1", ChangeType: domain.PersonnelChangeTransfer, Reason: "业务调整", Status: domain.PersonnelChangeScheduled, EffectiveAt: &effectiveAt}}
	service, err := NewPersonnelChangeService(repository, personnelChangeLifecycleIDGenerator{}, personnelChangeLifecycleClock{now: now})
	if err != nil {
		t.Fatal(err)
	}

	if _, err := service.Transition(context.Background(), PersonnelChangeTransitionInput{TenantID: "tenant-1", OperatorID: "operator-1", ID: "change-1", ToStatus: domain.PersonnelChangePendingApproval, ApprovalReference: "APPROVAL-1", Immediate: true}); !errors.Is(err, ErrValidation) {
		t.Fatalf("immediate on non-executed status err=%v, want ErrValidation", err)
	}
	if _, err := service.Transition(context.Background(), PersonnelChangeTransitionInput{TenantID: "tenant-1", OperatorID: "operator-1", ID: "change-1", ToStatus: domain.PersonnelChangeCancelled, Immediate: true}); !errors.Is(err, ErrValidation) {
		t.Fatalf("immediate on cancel err=%v, want ErrValidation", err)
	}
}

type stubHandoverChecker struct{ ready bool }

func (s stubHandoverChecker) Check(context.Context, PersonnelChangeRequest) (HandoverReport, error) {
	return HandoverReport{Ready: s.ready}, nil
}

// personnelChangeTransitionRepository 支持 UpdateStatus 路径的桩，用于交接排期测试。
type personnelChangeTransitionRepository struct {
	request PersonnelChangeRequest
	err     error
}

func (r *personnelChangeTransitionRepository) Create(context.Context, PersonnelChangeRequest) (PersonnelChangeRequest, error) {
	return PersonnelChangeRequest{}, errors.New("unexpected create")
}
func (r *personnelChangeTransitionRepository) List(context.Context, string, string, string, string) ([]PersonnelChangeRequest, error) {
	return nil, nil
}
func (r *personnelChangeTransitionRepository) Get(context.Context, string, string) (PersonnelChangeRequest, error) {
	return r.request, nil
}
func (r *personnelChangeTransitionRepository) UpdateStatus(_ context.Context, expected PersonnelChangeRequest, status, ref, _ string, now time.Time) (PersonnelChangeRequest, error) {
	if r.err != nil {
		return PersonnelChangeRequest{}, r.err
	}
	r.request = expected
	r.request.Status = status
	if status == domain.PersonnelChangeScheduled {
		r.request.HandoverReference = ref
	}
	r.request.UpdatedAt = now
	return r.request, nil
}
func (r *personnelChangeTransitionRepository) Execute(context.Context, PersonnelChangeRequest, string, time.Time) (PersonnelChangeRequest, error) {
	return PersonnelChangeRequest{}, errors.New("unexpected execute")
}
func (r *personnelChangeTransitionRepository) PreviewPermissions(context.Context, PersonnelChangeRequest) (PersonnelChangePermissionPreview, error) {
	return PersonnelChangePermissionPreview{}, nil
}
func (r *personnelChangeTransitionRepository) ValidateCreate(context.Context, PersonnelChangeCreateInput) error {
	return nil
}

func TestTerminationHandoverSchedulingGeneratesReferenceWhenOmitted(t *testing.T) {
	now := time.Date(2026, 8, 25, 10, 0, 0, 0, time.UTC)
	repository := &personnelChangeTransitionRepository{request: PersonnelChangeRequest{
		ID: "change-termination", TenantID: "tenant-1", UserID: "user-1",
		ChangeType: domain.PersonnelChangeTermination, Status: domain.PersonnelChangePendingHandover,
	}}
	service, err := NewPersonnelChangeService(repository, personnelChangeLifecycleIDGenerator{}, personnelChangeLifecycleClock{now: now}, stubHandoverChecker{ready: true})
	if err != nil {
		t.Fatal(err)
	}

	result, err := service.Transition(context.Background(), PersonnelChangeTransitionInput{
		TenantID: "tenant-1", OperatorID: "approver-1", ID: "change-termination", ToStatus: domain.PersonnelChangeScheduled,
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.HandoverReference != "HANDOVER-20260825-nation" {
		t.Fatalf("handover_reference=%q, want server-generated HANDOVER-20260825-nation", result.HandoverReference)
	}
	if result.Status != domain.PersonnelChangeScheduled {
		t.Fatalf("status=%q, want SCHEDULED", result.Status)
	}
}

func TestTerminationHandoverSchedulingStillRejectsForeignReferenceAndIncompleteHandover(t *testing.T) {
	now := time.Date(2026, 8, 25, 10, 0, 0, 0, time.UTC)
	newRepository := func() *personnelChangeTransitionRepository {
		return &personnelChangeTransitionRepository{request: PersonnelChangeRequest{
			ID: "change-termination", TenantID: "tenant-1", UserID: "user-1",
			ChangeType: domain.PersonnelChangeTermination, Status: domain.PersonnelChangePendingHandover,
		}}
	}

	foreignRepository := newRepository()
	foreignService, err := NewPersonnelChangeService(foreignRepository, personnelChangeLifecycleIDGenerator{}, personnelChangeLifecycleClock{now: now}, stubHandoverChecker{ready: true})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := foreignService.Transition(context.Background(), PersonnelChangeTransitionInput{
		TenantID: "tenant-1", OperatorID: "approver-1", ID: "change-termination",
		ToStatus: domain.PersonnelChangeScheduled, ApprovalReference: "APPROVAL-9001",
	}); !errors.Is(err, ErrConflict) {
		t.Fatalf("error=%v, want ErrConflict for non-HANDOVER reference", err)
	}

	incompleteRepository := newRepository()
	incompleteService, err := NewPersonnelChangeService(incompleteRepository, personnelChangeLifecycleIDGenerator{}, personnelChangeLifecycleClock{now: now}, stubHandoverChecker{ready: false})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := incompleteService.Transition(context.Background(), PersonnelChangeTransitionInput{
		TenantID: "tenant-1", OperatorID: "approver-1", ID: "change-termination", ToStatus: domain.PersonnelChangeScheduled,
	}); !errors.Is(err, ErrConflict) {
		t.Fatalf("error=%v, want ErrConflict when handover incomplete", err)
	}
}
