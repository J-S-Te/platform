package worker

import (
	"context"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/J-S-Te/Basic-Platform/internal/platform/identity/application"
	"github.com/J-S-Te/Basic-Platform/internal/platform/identity/domain"
)

type personnelRepoStub struct {
	request  application.PersonnelChangeRequest
	executed int
}

func (r *personnelRepoStub) Create(context.Context, application.PersonnelChangeRequest) (application.PersonnelChangeRequest, error) {
	return application.PersonnelChangeRequest{}, nil
}
func (r *personnelRepoStub) List(_ context.Context, _, status, _, _ string) ([]application.PersonnelChangeRequest, error) {
	if status != domain.PersonnelChangeScheduled {
		return nil, nil
	}
	return []application.PersonnelChangeRequest{r.request}, nil
}
func (r *personnelRepoStub) Get(context.Context, string, string) (application.PersonnelChangeRequest, error) {
	return r.request, nil
}
func (r *personnelRepoStub) UpdateStatus(context.Context, application.PersonnelChangeRequest, string, string, string, time.Time) (application.PersonnelChangeRequest, error) {
	return r.request, nil
}
func (r *personnelRepoStub) Execute(_ context.Context, request application.PersonnelChangeRequest, _ string, now time.Time) (application.PersonnelChangeRequest, error) {
	r.executed++
	request.Status = domain.PersonnelChangeExecuted
	request.ExecutedAt = &now
	return request, nil
}
func (r *personnelRepoStub) PreviewPermissions(context.Context, application.PersonnelChangeRequest) (application.PersonnelChangePermissionPreview, error) {
	return application.PersonnelChangePermissionPreview{}, nil
}
func (r *personnelRepoStub) ValidateCreate(context.Context, application.PersonnelChangeCreateInput) error {
	return nil
}

type personnelIDStub struct{}

type workerHandoverGate struct{ ready bool }

func (g *workerHandoverGate) Check(context.Context, application.PersonnelChangeRequest) (application.HandoverReport, error) {
	return application.HandoverReport{Ready: g.ready}, nil
}

type workerCrossHandoverGate struct {
	complete bool
	calls    int
}

type workerCompletionRetry struct {
	workerCrossHandoverGate
	cancel          context.CancelFunc
	limit, attempts int
}

func (r *workerCompletionRetry) CompleteExecuted(_ context.Context, limit int) (int, error) {
	r.limit = limit
	r.attempts++
	r.cancel()
	return 1, nil
}

func TestPersonnelWorkerRetriesCompletionWithoutDueRequests(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	repo := &personnelRepoStub{request: application.PersonnelChangeRequest{EffectiveAt: ptr(time.Now().Add(time.Hour))}}
	service, err := application.NewPersonnelChangeService(repo, personnelIDStub{}, application.SystemClock{})
	if err != nil {
		t.Fatal(err)
	}
	retry := &workerCompletionRetry{cancel: cancel}
	service.SetHandoverCoordinator(retry)
	w, err := NewPersonnelChangeWorker(service, slog.New(slog.NewTextHandler(io.Discard, nil)), "completion-worker", time.Second)
	if err != nil {
		t.Fatal(err)
	}
	w.Run(ctx)
	if retry.attempts != 1 || retry.limit != 50 || repo.executed != 0 {
		t.Fatalf("background completion attempts=%d limit=%d executed=%d", retry.attempts, retry.limit, repo.executed)
	}
}

func (*workerCrossHandoverGate) Start(context.Context, string, string, string) (application.HandoverBatch, error) {
	return application.HandoverBatch{}, application.ErrConflict
}
func (*workerCrossHandoverGate) CancelByRequest(context.Context, string, string) error {
	return application.ErrConflict
}
func (g *workerCrossHandoverGate) VerifyCrossSystemCompletion(context.Context, string, string) (application.HandoverCompletionVerification, error) {
	g.calls++
	status := application.HandoverBatchBlocked
	if g.complete {
		status = application.HandoverBatchFrozen
	}
	return application.HandoverCompletionVerification{BatchFound: true, BatchID: "batch", BatchStatus: status, CrossSystemComplete: g.complete}, nil
}
func (*workerCrossHandoverGate) Complete(context.Context, string, string) error     { return nil }
func (*workerCrossHandoverGate) CompleteExecuted(context.Context, int) (int, error) { return 0, nil }
func TestPersonnelWorkerRechecksLocalAndCrossSystemHandoverBeforeExecution(t *testing.T) {
	now := time.Now().UTC()
	repo := &personnelRepoStub{request: application.PersonnelChangeRequest{ID: "change", TenantID: "tenant", UserID: "leaver", ChangeType: domain.PersonnelChangeTermination, Status: domain.PersonnelChangeScheduled, EffectiveAt: ptr(now.Add(-time.Minute))}}
	gate := &workerHandoverGate{}
	service, err := application.NewPersonnelChangeService(repo, personnelIDStub{}, application.SystemClock{}, gate)
	if err != nil {
		t.Fatal(err)
	}
	cross := &workerCrossHandoverGate{}
	service.SetHandoverCoordinator(cross)
	w, err := NewPersonnelChangeWorker(service, slog.New(slog.NewTextHandler(io.Discard, nil)), "worker", time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if w.ProcessDue(context.Background()) != 0 || repo.executed != 0 {
		t.Fatal("local pending handover executed")
	}
	gate.ready = true
	if w.ProcessDue(context.Background()) != 0 || repo.executed != 0 || cross.calls != 1 {
		t.Fatal("cross-system blocked handover executed")
	}
	cross.complete = true
	if w.ProcessDue(context.Background()) != 1 || repo.executed != 1 || cross.calls != 2 {
		t.Fatal("verified completed termination did not execute")
	}
}

func (personnelIDStub) New(time.Time) (string, error) { return "01J00000000000000000000000", nil }

func TestPersonnelChangeWorkerProcessesDueOnly(t *testing.T) {
	now := time.Now().UTC()
	repo := &personnelRepoStub{request: application.PersonnelChangeRequest{ID: "01J00000000000000000000001", TenantID: "01J00000000000000000000000", UserID: "01J00000000000000000000002", Status: domain.PersonnelChangeScheduled, EffectiveAt: ptr(now.Add(-time.Minute))}}
	service, err := application.NewPersonnelChangeService(repo, personnelIDStub{}, application.SystemClock{})
	if err != nil {
		t.Fatal(err)
	}
	w, err := NewPersonnelChangeWorker(service, slog.New(slog.NewTextHandler(io.Discard, nil)), "worker-1", time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if got := w.ProcessDue(context.Background()); got != 1 || repo.executed != 1 {
		t.Fatalf("processed=%d executed=%d, want 1/1", got, repo.executed)
	}

	repo.request.EffectiveAt = ptr(now.Add(time.Hour))
	if got := w.ProcessDue(context.Background()); got != 0 || repo.executed != 1 {
		t.Fatalf("future processed=%d executed=%d, want 0/1", got, repo.executed)
	}
}

// approvalExceptionDispatcherStub 记录 worker 是否调用了 outbox 投递以及返回条数。
type approvalExceptionDispatcherStub struct {
	calls  int
	limit  int
	report application.ApprovalExceptionDispatchReport
	err    error
}

func (stub *approvalExceptionDispatcherStub) DispatchOnce(_ context.Context, limit int) (application.ApprovalExceptionDispatchReport, error) {
	stub.calls++
	stub.limit = limit
	return stub.report, stub.err
}

func TestPersonnelChangeWorkerDispatchesApprovalExceptionOutbox(t *testing.T) {
	service, err := application.NewPersonnelChangeService(&personnelRepoStub{}, personnelIDStub{}, application.SystemClock{})
	if err != nil {
		t.Fatal(err)
	}
	w, err := NewPersonnelChangeWorker(service, slog.New(slog.NewTextHandler(io.Discard, nil)), "worker-1", time.Second)
	if err != nil {
		t.Fatal(err)
	}
	// 未注入投递器时必须是安全 no-op。
	if got := w.DispatchApprovalExceptions(context.Background()); got != 0 {
		t.Fatalf("delivered without a dispatcher = %d, want 0", got)
	}

	dispatcher := &approvalExceptionDispatcherStub{report: application.ApprovalExceptionDispatchReport{Inspected: 2, Delivered: 2}}
	w.SetApprovalExceptionDispatcher(dispatcher)
	if got := w.DispatchApprovalExceptions(context.Background()); got != 2 {
		t.Fatalf("delivered = %d, want 2", got)
	}
	if dispatcher.calls != 1 || dispatcher.limit != approvalExceptionDispatchBatch {
		t.Fatalf("dispatcher calls=%d limit=%d, want one batch of %d", dispatcher.calls, dispatcher.limit, approvalExceptionDispatchBatch)
	}
}

// Run 每一轮都应在执行到期异动后补投审计 outbox；已取消的 context 仍允许完成当前轮。
func TestPersonnelChangeWorkerRunDispatchesApprovalExceptions(t *testing.T) {
	service, err := application.NewPersonnelChangeService(&personnelRepoStub{}, personnelIDStub{}, application.SystemClock{})
	if err != nil {
		t.Fatal(err)
	}
	w, err := NewPersonnelChangeWorker(service, slog.New(slog.NewTextHandler(io.Discard, nil)), "worker-1", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	dispatcher := &approvalExceptionDispatcherStub{report: application.ApprovalExceptionDispatchReport{Delivered: 1}}
	w.SetApprovalExceptionDispatcher(dispatcher)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	w.Run(ctx)
	if dispatcher.calls != 1 {
		t.Fatalf("dispatcher calls = %d, want the worker loop to dispatch once before exiting", dispatcher.calls)
	}
}

func ptr(t time.Time) *time.Time { return &t }
