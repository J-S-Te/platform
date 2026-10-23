package application

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/J-S-Te/Basic-Platform/internal/platform/identity/domain"
)

type freezeLifecycleClient struct {
	handoverFakeClient
	personnel *personnelChangeExecutionRepository
	finishErr error
	premature bool
}

func (c *freezeLifecycleClient) Finish(ctx context.Context, batch, request, outcome string) error {
	if c.personnel.request.Status != domain.PersonnelChangeExecuted {
		c.premature = true
	}
	_ = c.handoverFakeClient.Finish(ctx, batch, request, outcome)
	return c.finishErr
}

func TestTerminationFreezeLifecycle(t *testing.T) {
	for _, tc := range []struct {
		name                  string
		executeErr, finishErr error
	}{
		{name: "success"},
		{name: "execute failure", executeErr: errors.New("transaction rolled back")},
		{name: "finish failure", finishErr: errors.New("remote unavailable")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			request := pendingHandoverTermination()
			request.Status = domain.PersonnelChangeScheduled
			personnel := &personnelChangeExecutionRepository{request: request, err: tc.executeErr}
			repo := newHandoverFakeRepository()
			client := &freezeLifecycleClient{personnel: personnel, finishErr: tc.finishErr}
			orchestrator := handoverOrchestratorFor(t, repo, handoverFakeInventory{states: []HandoverSystemState{{System: "contract", Capability: HandoverSystemApplicable}}}, map[string]HandoverSystemClient{"contract": client})
			if _, err := orchestrator.Start(context.Background(), request.TenantID, request.ID, request.UserID); err != nil {
				t.Fatal(err)
			}
			service, err := NewPersonnelChangeService(personnel, personnelChangeLifecycleIDGenerator{}, personnelChangeLifecycleClock{}, stubHandoverChecker{ready: true})
			if err != nil {
				t.Fatal(err)
			}
			service.SetHandoverCoordinator(orchestrator)
			result, err := service.Transition(context.Background(), PersonnelChangeTransitionInput{TenantID: request.TenantID, ID: request.ID, OperatorID: "worker", ToStatus: domain.PersonnelChangeExecuted, Immediate: true})
			if tc.executeErr != nil {
				if !errors.Is(err, tc.executeErr) {
					t.Fatalf("execution error = %v", err)
				}
				if containsCall(client.calls, "finish:COMPLETED") || repo.batch.Status != HandoverBatchFrozen {
					t.Fatal("failed local execution released freeze")
				}
				return
			}
			if err != nil || result.Status != domain.PersonnelChangeExecuted || personnel.request.Status != domain.PersonnelChangeExecuted {
				t.Fatalf("committed execution must succeed: result=%+v err=%v", result, err)
			}
			if client.premature || !containsCall(client.calls, "finish:COMPLETED") {
				t.Fatal("finish must occur after local execution succeeds")
			}
			if tc.finishErr == nil && repo.batch.Status != HandoverBatchCompleted {
				t.Fatal("successful finish not persisted")
			}
			if tc.finishErr != nil && repo.batch.Status != HandoverBatchFrozen {
				t.Fatal("failed finish lost durable pending batch")
			}
		})
	}
}

func TestLegacyReleasedTerminationRequiresControlledRecovery(t *testing.T) {
	for _, status := range []string{domain.PersonnelChangePendingHandover, domain.PersonnelChangeScheduled} {
		request := pendingHandoverTermination()
		request.Status = status
		coordinator := &handoverCoordinatorStub{verification: HandoverCompletionVerification{BatchFound: true, BatchID: "old-batch", BatchStatus: HandoverBatchCompleted, CrossSystemComplete: true}}
		service, repo := terminationSchedulingService(t, request, coordinator)
		to := domain.PersonnelChangeScheduled
		if status == domain.PersonnelChangeScheduled {
			to = domain.PersonnelChangeExecuted
		}
		_, err := service.Transition(context.Background(), PersonnelChangeTransitionInput{TenantID: request.TenantID, ID: request.ID, OperatorID: "worker", ToStatus: to, Immediate: to == domain.PersonnelChangeExecuted})
		if !errors.Is(err, ErrConflict) || !errors.Is(err, ErrHandoverFreezeReleased) || !strings.Contains(err.Error(), "controlled recovery") {
			t.Fatalf("legacy released freeze needs explicit recovery: %v", err)
		}
		if repo.request.Status != status {
			t.Fatal("legacy request advanced without a real freeze")
		}
	}
}

// The durable queue must be separate from the unbounded historical personnel list.
type pendingCompletionRepository struct {
	*handoverFakeRepository
	requests    *personnelChangeExecutionRepository
	after       string
	limit       int
	completeErr error
}

func (r *pendingCompletionRepository) CompleteBatch(ctx context.Context, tenant, batch, status string, now time.Time) error {
	if status == HandoverBatchCompleted && r.completeErr != nil {
		return r.completeErr
	}
	return r.handoverFakeRepository.CompleteBatch(ctx, tenant, batch, status, now)
}

func (r *pendingCompletionRepository) ListPendingCompletionBatches(_ context.Context, after string, limit int) ([]HandoverBatch, error) {
	r.after, r.limit = after, limit
	if r.requests.request.Status != domain.PersonnelChangeExecuted || r.batch.Status == HandoverBatchCompleted || r.batch.ID <= after {
		return nil, nil
	}
	return []HandoverBatch{r.batch}, nil
}

func TestExecutedCompletionCheckpointFailureIsRetried(t *testing.T) {
	personnel := &personnelChangeExecutionRepository{request: PersonnelChangeRequest{Status: domain.PersonnelChangeExecuted}}
	repo := &pendingCompletionRepository{handoverFakeRepository: newHandoverFakeRepository(), requests: personnel, completeErr: errors.New("checkpoint write failed")}
	repo.batch = HandoverBatch{ID: "batch-1", TenantID: "tenant-1", RequestID: "request-1", Status: HandoverBatchFrozen}
	repo.systems = []HandoverSystemState{{System: "contract", Capability: HandoverSystemApplicable, Frozen: true}}
	client := &freezeLifecycleClient{personnel: personnel}
	orchestrator, err := NewHandoverOrchestrator(repo, handoverFakeInventory{}, map[string]HandoverSystemClient{"contract": client}, personnelChangeLifecycleIDGenerator{}, fixedClock{})
	if err != nil {
		t.Fatal(err)
	}
	if err := orchestrator.Complete(context.Background(), "tenant-1", "batch-1"); !errors.Is(err, repo.completeErr) {
		t.Fatalf("checkpoint failure=%v", err)
	}
	if repo.batch.Status != HandoverBatchFrozen || len(client.finishOutcomes) != 1 {
		t.Fatal("finish success must leave retry checkpoint when persistence fails")
	}
	repo.completeErr = nil
	if n, err := orchestrator.CompleteExecuted(context.Background(), 1); n != 1 || err != nil {
		t.Fatalf("checkpoint retry=%d err=%v", n, err)
	}
	if len(client.finishOutcomes) != 2 || repo.batch.Status != HandoverBatchCompleted {
		t.Fatal("completion must replay the same batch idempotently")
	}
}

type completionFairRepository struct {
	*handoverFakeRepository
	batches []HandoverBatch
	cursors []string
}

func (r *completionFairRepository) ListPendingCompletionBatches(_ context.Context, after string, limit int) ([]HandoverBatch, error) {
	r.cursors = append(r.cursors, after)
	var pending []HandoverBatch
	for _, b := range r.batches {
		if b.Status != HandoverBatchCompleted && b.ID > after {
			pending = append(pending, b)
		}
		if len(pending) == limit {
			break
		}
	}
	return pending, nil
}
func (r *completionFairRepository) GetBatchByID(_ context.Context, tenant, id string) (HandoverBatch, error) {
	for _, b := range r.batches {
		if b.TenantID == tenant && b.ID == id {
			return b, nil
		}
	}
	return HandoverBatch{}, ErrNotFound
}
func (r *completionFairRepository) GetBatch(_ context.Context, tenant, request string) (HandoverBatch, error) {
	for _, b := range r.batches {
		if b.TenantID == tenant && b.RequestID == request {
			return b, nil
		}
	}
	return HandoverBatch{}, ErrNotFound
}
func (r *completionFairRepository) CompleteBatch(_ context.Context, tenant, id, status string, _ time.Time) error {
	for i, b := range r.batches {
		if b.TenantID == tenant && b.ID == id {
			r.batches[i].Status = status
			return nil
		}
	}
	return ErrNotFound
}

type completionPartialClient struct {
	handoverFakeClient
	failBatch string
	finished  []string
}

func (c *completionPartialClient) Finish(_ context.Context, batch, request, outcome string) error {
	c.finished = append(c.finished, batch+"|"+request+"|"+outcome)
	if batch == c.failBatch {
		return errors.New("temporary remote failure")
	}
	return nil
}

func TestCompletionRetryAdvancesPastFailingBatch(t *testing.T) {
	repo := &completionFairRepository{handoverFakeRepository: newHandoverFakeRepository(), batches: []HandoverBatch{
		{ID: "batch-1", TenantID: "tenant-1", RequestID: "request-1", Status: HandoverBatchFrozen},
		{ID: "batch-2", TenantID: "tenant-2", RequestID: "request-2", Status: HandoverBatchTransferring},
	}}
	repo.systems = []HandoverSystemState{{System: "contract", Capability: HandoverSystemApplicable, Frozen: true}}
	client := &completionPartialClient{failBatch: "batch-1"}
	orchestrator, err := NewHandoverOrchestrator(repo, handoverFakeInventory{}, map[string]HandoverSystemClient{"contract": client}, personnelChangeLifecycleIDGenerator{}, fixedClock{})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if n, err := orchestrator.CompleteExecuted(ctx, 1); n != 0 || err == nil {
		t.Fatalf("first retry=%d err=%v", n, err)
	}
	if n, err := orchestrator.CompleteExecuted(ctx, 1); n != 1 || err != nil {
		t.Fatalf("second retry=%d err=%v", n, err)
	}
	if repo.batches[0].Status != HandoverBatchFrozen || repo.batches[1].Status != HandoverBatchCompleted {
		t.Fatal("poison batch starved another tenant")
	}
	if n, err := orchestrator.CompleteExecuted(ctx, 1); n != 0 || err != nil {
		t.Fatalf("end of queue=%d err=%v", n, err)
	}
	client.failBatch = ""
	if n, err := orchestrator.CompleteExecuted(ctx, 1); n != 1 || err != nil {
		t.Fatalf("rotated retry=%d err=%v", n, err)
	}
	want := []string{"", "batch-1", "batch-2", ""}
	for i := range want {
		if repo.cursors[i] != want[i] {
			t.Fatalf("retry cursors=%v", repo.cursors)
		}
	}
	if client.finished[0] != "batch-1|request-1|COMPLETED" || client.finished[1] != "batch-2|request-2|COMPLETED" || client.finished[2] != client.finished[0] {
		t.Fatalf("unstable finish identities=%v", client.finished)
	}
	for _, limit := range []int{0, -1, 101} {
		if _, err := orchestrator.CompleteExecuted(ctx, limit); !errors.Is(err, ErrValidation) {
			t.Fatalf("invalid completion bound %d=%v", limit, err)
		}
	}
}

func TestCompletedBatchNoLongerProvesFreeze(t *testing.T) {
	repo := newHandoverFakeRepository()
	repo.batch = HandoverBatch{ID: "batch-1", RequestID: "request-1", Status: HandoverBatchCompleted}
	repo.systems = []HandoverSystemState{{System: "contract", Capability: HandoverSystemApplicable, Frozen: true}}
	orchestrator := staleOrchestrator(t, repo, HandoverSystemApplicable, &handoverFakeClient{})
	result, err := orchestrator.VerifyCrossSystemCompletion(context.Background(), "tenant-1", "request-1")
	if err != nil || result.CrossSystemComplete {
		t.Fatalf("historical frozen flag misrepresents released batch: %+v err=%v", result, err)
	}
}

func TestExecutedTerminationCompletionSurvivesRestart(t *testing.T) {
	request := pendingHandoverTermination()
	request.Status = domain.PersonnelChangeExecuted
	personnel := &personnelChangeExecutionRepository{request: request}
	repo := &pendingCompletionRepository{handoverFakeRepository: newHandoverFakeRepository(), requests: personnel}
	repo.batch = HandoverBatch{ID: "batch-1", TenantID: request.TenantID, RequestID: request.ID, UserID: request.UserID, Status: HandoverBatchFrozen}
	repo.systems = []HandoverSystemState{{System: "contract", Capability: HandoverSystemApplicable, Frozen: true}}
	client := &freezeLifecycleClient{personnel: personnel, finishErr: errors.New("temporary failure")}
	newRunner := func() interface {
		CompleteExecuted(context.Context, int) (int, error)
	} { o, err := NewHandoverOrchestrator(repo, handoverFakeInventory{}, map[string]HandoverSystemClient{"contract": client}, personnelChangeLifecycleIDGenerator{}, fixedClock{now: time.Now()}); if err != nil {
		t.Fatal(err)
	}; runner, ok := any(o).(interface {
		CompleteExecuted(context.Context, int) (int, error)
	}); if !ok {
		t.Fatal("missing durable bounded completion retry")
	}; return runner }
	if n, err := newRunner().CompleteExecuted(context.Background(), 1); n != 0 || err == nil {
		t.Fatalf("retry failure = %d, %v", n, err)
	}
	if repo.batch.Status != HandoverBatchFrozen {
		t.Fatal("failed finish lost retry state")
	}
	client.finishErr = nil
	if n, err := newRunner().CompleteExecuted(context.Background(), 1); n != 1 || err != nil {
		t.Fatalf("restart completion = %d, %v", n, err)
	}
	if repo.limit != 1 || repo.batch.Status != HandoverBatchCompleted {
		t.Fatal("bounded completion did not persist success")
	}
	before := len(client.calls)
	if n, err := newRunner().CompleteExecuted(context.Background(), 1); n != 0 || err != nil || len(client.calls) != before {
		t.Fatal("completed batch was released again")
	}
}
