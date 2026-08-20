package application

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/J-S-Te/Basic-Platform/internal/platform/identity/domain"
)

// personnelChangeCASRepository 在内存中模拟仓储的状态/版本条件更新，让两个服务调用
// 必须基于同一旧快照竞争，从而覆盖数据库中执行与取消的真实竞态。
type personnelChangeCASRepository struct {
	mu           sync.Mutex
	request      PersonnelChangeRequest
	getCount     int
	bothRead     chan struct{}
	bothReadOnce sync.Once
}

func (repository *personnelChangeCASRepository) Create(_ context.Context, request PersonnelChangeRequest) (PersonnelChangeRequest, error) {
	return request, nil
}

func (repository *personnelChangeCASRepository) List(context.Context, string, string, string, string) ([]PersonnelChangeRequest, error) {
	return nil, nil
}

func (repository *personnelChangeCASRepository) Get(context.Context, string, string) (PersonnelChangeRequest, error) {
	repository.mu.Lock()
	snapshot := repository.request
	repository.getCount++
	if repository.getCount == 2 {
		repository.bothReadOnce.Do(func() { close(repository.bothRead) })
	}
	repository.mu.Unlock()

	// 强制执行与取消都读取同一个版本后再继续，稳定复现先读后写竞争。
	<-repository.bothRead
	return snapshot, nil
}

func (repository *personnelChangeCASRepository) UpdateStatus(_ context.Context, expected PersonnelChangeRequest, status, _ string, now time.Time) (PersonnelChangeRequest, error) {
	repository.mu.Lock()
	defer repository.mu.Unlock()
	if repository.request.Status != expected.Status || repository.request.Version != expected.Version {
		return PersonnelChangeRequest{}, ErrConflict
	}
	repository.request.Status = status
	repository.request.Version++
	repository.request.UpdatedAt = now
	return repository.request, nil
}

func (repository *personnelChangeCASRepository) Execute(_ context.Context, expected PersonnelChangeRequest, _ string, now time.Time) (PersonnelChangeRequest, error) {
	repository.mu.Lock()
	defer repository.mu.Unlock()
	if repository.request.Status != domain.PersonnelChangeScheduled || repository.request.Status != expected.Status || repository.request.Version != expected.Version {
		return PersonnelChangeRequest{}, ErrConflict
	}
	repository.request.Status = domain.PersonnelChangeExecuted
	repository.request.Version++
	repository.request.ExecutedAt = &now
	repository.request.UpdatedAt = now
	return repository.request, nil
}

func (repository *personnelChangeCASRepository) PreviewPermissions(context.Context, PersonnelChangeRequest) (PersonnelChangePermissionPreview, error) {
	return PersonnelChangePermissionPreview{}, nil
}

type personnelChangeConcurrencyIDGenerator struct{}

func (personnelChangeConcurrencyIDGenerator) New(time.Time) (string, error) { return "unused", nil }

type personnelChangeConcurrencyClock struct{ now time.Time }

func (clock personnelChangeConcurrencyClock) Now() time.Time { return clock.now }

func TestPersonnelChangeExecuteAndCancelAreMutuallyExclusive(t *testing.T) {
	now := time.Date(2026, time.August, 20, 10, 0, 0, 0, time.UTC)
	effectiveAt := now.Add(-time.Minute)
	repository := &personnelChangeCASRepository{
		request: PersonnelChangeRequest{
			ID: "change-1", TenantID: "tenant-1", UserID: "user-1",
			Status: domain.PersonnelChangeScheduled, Version: 7, EffectiveAt: &effectiveAt,
		},
		bothRead: make(chan struct{}),
	}
	service, err := NewPersonnelChangeService(repository, personnelChangeConcurrencyIDGenerator{}, personnelChangeConcurrencyClock{now: now})
	if err != nil {
		t.Fatal(err)
	}

	errorsByOperation := make(chan error, 2)
	var calls sync.WaitGroup
	calls.Add(2)
	go func() {
		defer calls.Done()
		_, executeErr := service.Transition(context.Background(), PersonnelChangeTransitionInput{
			TenantID: "tenant-1", OperatorID: "worker-1", ID: "change-1", ToStatus: domain.PersonnelChangeExecuted,
		})
		errorsByOperation <- executeErr
	}()
	go func() {
		defer calls.Done()
		_, cancelErr := service.Transition(context.Background(), PersonnelChangeTransitionInput{
			TenantID: "tenant-1", OperatorID: "admin-1", ID: "change-1", ToStatus: domain.PersonnelChangeCancelled,
		})
		errorsByOperation <- cancelErr
	}()
	calls.Wait()
	close(errorsByOperation)

	successes, conflicts := 0, 0
	for transitionErr := range errorsByOperation {
		switch {
		case transitionErr == nil:
			successes++
		case errors.Is(transitionErr, ErrConflict):
			conflicts++
		default:
			t.Fatalf("unexpected transition error: %v", transitionErr)
		}
	}
	if successes != 1 || conflicts != 1 {
		t.Fatalf("successes=%d conflicts=%d, want 1/1", successes, conflicts)
	}
	if repository.request.Version != 8 {
		t.Fatalf("final version=%d, want 8", repository.request.Version)
	}
	if repository.request.Status != domain.PersonnelChangeExecuted && repository.request.Status != domain.PersonnelChangeCancelled {
		t.Fatalf("final status=%q, want EXECUTED or CANCELLED", repository.request.Status)
	}
}
