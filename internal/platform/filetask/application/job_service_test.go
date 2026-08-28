package application

import (
	"context"
	"testing"
	"time"

	"github.com/J-S-Te/Basic-Platform/internal/platform/filetask/domain"
)

type jobRepositoryStub struct {
	JobRepository
	created   domain.Job
	original  domain.Job
	completed domain.Job
}

func (stub *jobRepositoryStub) CompleteJob(_ context.Context, job domain.Job, _ time.Time) error {
	stub.completed = job
	return nil
}

func (stub *jobRepositoryStub) CreateJob(_ context.Context, job domain.Job) (domain.Job, error) {
	stub.created = job
	return job, nil
}

func (stub *jobRepositoryStub) GetJob(context.Context, string, string) (domain.Job, error) {
	return stub.original, nil
}

func (stub *jobRepositoryStub) CreateRerun(_ context.Context, job domain.Job) (domain.Job, error) {
	stub.created = job
	return job, nil
}

type jobIDGeneratorStub struct{ values []string }

func (stub *jobIDGeneratorStub) New(time.Time) (string, error) {
	value := stub.values[0]
	stub.values = stub.values[1:]
	return value, nil
}

type jobClockStub struct{ now time.Time }

func (stub jobClockStub) Now() time.Time { return stub.now }

func TestEnqueuePersistsIdempotencyAndCorrelationMetadata(t *testing.T) {
	now := time.Date(2026, 8, 27, 12, 0, 0, 0, time.UTC)
	repository := &jobRepositoryStub{}
	service, err := NewJobService(repository, &jobIDGeneratorStub{values: []string{"job-1"}}, jobClockStub{now: now})
	if err != nil {
		t.Fatalf("NewJobService() error = %v", err)
	}
	job, err := service.Enqueue(context.Background(), JobCreateInput{
		TenantID: "tenant-1", ApplicationID: "application-1", JobType: "EXPORT_REPORT",
		Payload: []byte(`{"b":2,"a":1}`), IdempotencyKey: "report:42", RequestID: "request-1",
		TraceID: "0123456789abcdef0123456789abcdef", CorrelationID: "business-42", BusinessRef: "contract-42",
	})
	if err != nil {
		t.Fatalf("Enqueue() error = %v", err)
	}
	if job.IdempotencyKey != "report:42" || len(job.RequestHash) != 32 || job.CorrelationID != "business-42" || job.BusinessRef != "contract-42" {
		t.Fatalf("created job = %#v", job)
	}

	secondRepository := &jobRepositoryStub{}
	secondService, _ := NewJobService(secondRepository, &jobIDGeneratorStub{values: []string{"job-2"}}, jobClockStub{now: now.Add(time.Minute)})
	second, err := secondService.Enqueue(context.Background(), JobCreateInput{
		TenantID: "tenant-1", ApplicationID: "application-1", JobType: "EXPORT_REPORT",
		Payload: []byte("{\n  \"a\": 1, \"b\": 2\n}"), IdempotencyKey: "report:42",
	})
	if err != nil {
		t.Fatalf("second Enqueue() error = %v", err)
	}
	if string(job.RequestHash) != string(second.RequestHash) {
		t.Fatal("semantically equal JSON must produce the same request hash")
	}
}

func TestRerunPersistsParentAndResetsExecutionState(t *testing.T) {
	now := time.Date(2026, 8, 27, 12, 0, 0, 0, time.UTC)
	completedAt := now.Add(-time.Hour)
	repository := &jobRepositoryStub{original: domain.Job{
		ID: 42, PublicID: "job-original", TenantID: "tenant-1", JobType: "EXPORT_REPORT",
		Payload: []byte(`{"id":42}`), Status: domain.JobStatusFailed, Attempts: 3, RetryCount: 2,
		IdempotencyKey: "original-key", CompletedAt: &completedAt, LastSucceededAt: &completedAt,
	}}
	service, _ := NewJobService(repository, &jobIDGeneratorStub{values: []string{"job-rerun"}}, jobClockStub{now: now})
	job, err := service.Rerun(context.Background(), "tenant-1", "job-original")
	if err != nil {
		t.Fatalf("Rerun() error = %v", err)
	}
	if job.ParentJobID != 42 || job.ParentPublicID != "job-original" || job.IdempotencyKey != "" || job.Attempts != 0 || job.RetryCount != 0 || job.CompletedAt != nil || job.LastSucceededAt != nil {
		t.Fatalf("rerun job = %#v", job)
	}
}

func TestCompleteRequiresCurrentWorkerLease(t *testing.T) {
	repository := &jobRepositoryStub{}
	service, _ := NewJobService(repository, &jobIDGeneratorStub{}, jobClockStub{now: time.Now().UTC()})
	job := domain.Job{TenantID: "tenant-1", PublicID: "job-1", Status: domain.JobStatusRunning}
	if err := service.Complete(context.Background(), job); err == nil {
		t.Fatal("Complete() accepted a running job without locked_by")
	}
	job.LockedBy = "worker-1"
	if err := service.Complete(context.Background(), job); err != nil {
		t.Fatalf("Complete() error = %v", err)
	}
	if repository.completed.LockedBy != "worker-1" {
		t.Fatalf("completed lease = %q", repository.completed.LockedBy)
	}
}
