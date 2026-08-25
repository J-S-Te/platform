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
	created PersonnelChangeRequest
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
func (r *personnelChangeCreateRepository) UpdateStatus(context.Context, PersonnelChangeRequest, string, string, time.Time) (PersonnelChangeRequest, error) {
	return PersonnelChangeRequest{}, ErrConflict
}
func (r *personnelChangeCreateRepository) Execute(context.Context, PersonnelChangeRequest, string, time.Time) (PersonnelChangeRequest, error) {
	return PersonnelChangeRequest{}, ErrConflict
}
func (r *personnelChangeCreateRepository) PreviewPermissions(context.Context, PersonnelChangeRequest) (PersonnelChangePermissionPreview, error) {
	return PersonnelChangePermissionPreview{}, nil
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

func personnelChangeCreateInput(approvalRequired bool) PersonnelChangeCreateInput {
	return PersonnelChangeCreateInput{
		TenantID: "tenant-1", OperatorID: "operator-1", UserID: "user-1",
		ChangeType: domain.PersonnelChangeTransfer, Reason: "业务调整",
		EffectiveAt: time.Date(2026, 8, 26, 10, 0, 0, 0, time.UTC), ApprovalRequired: approvalRequired,
	}
}

func TestPersonnelChangeCreateKeepsLegacyDirectScheduleByDefault(t *testing.T) {
	repository := &personnelChangeCreateRepository{}
	service := newPersonnelChangeForCreateTest(t, repository)

	created, err := service.Create(context.Background(), personnelChangeCreateInput(false))
	if err != nil {
		t.Fatal(err)
	}
	if created.Status != domain.PersonnelChangeScheduled || repository.created.Status != domain.PersonnelChangeScheduled {
		t.Fatalf("status=%q repository status=%q, want SCHEDULED", created.Status, repository.created.Status)
	}
}

func TestPersonnelChangeCreateApprovalRequiredStartsDraft(t *testing.T) {
	repository := &personnelChangeCreateRepository{}
	service := newPersonnelChangeForCreateTest(t, repository)

	created, err := service.Create(context.Background(), personnelChangeCreateInput(true))
	if err != nil {
		t.Fatal(err)
	}
	if created.Status != domain.PersonnelChangeDraft || repository.created.Status != domain.PersonnelChangeDraft {
		t.Fatalf("status=%q repository status=%q, want DRAFT", created.Status, repository.created.Status)
	}
	if !domain.CanTransitionPersonnelChange(created.Status, domain.PersonnelChangePendingApproval) {
		t.Fatal("approval-required draft must be submit-able")
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
func (r *personnelChangeExecutionRepository) UpdateStatus(context.Context, PersonnelChangeRequest, string, string, time.Time) (PersonnelChangeRequest, error) {
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
