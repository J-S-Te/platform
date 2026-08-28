package infrastructure

import (
	"testing"
	"time"

	"github.com/J-S-Te/Basic-Platform/internal/platform/filetask/domain"
)

func TestAsyncJobMappingPreservesReliabilityMetadata(t *testing.T) {
	now := time.Date(2026, 8, 27, 12, 0, 0, 0, time.UTC)
	job := domain.Job{
		PublicID: "job-1", ParentJobID: 42, TenantID: "tenant-1", JobType: "EXPORT_REPORT",
		IdempotencyKey: "report:42", Payload: []byte(`{"id":42}`), RequestHash: make([]byte, 32),
		RequestID: "request-1", TraceID: "0123456789abcdef0123456789abcdef",
		CorrelationID: "business-42", BusinessRef: "contract-42", Status: domain.JobStatusPending,
		Priority: 100, AvailableAt: now, RetryCount: 4, MaxAttempts: 3, CreatedAt: now,
	}
	mapped := toJob(toJobModel(job))
	if mapped.ParentJobID != 42 || mapped.IdempotencyKey != job.IdempotencyKey || mapped.CorrelationID != job.CorrelationID || mapped.RetryCount != 4 || len(mapped.RequestHash) != 32 {
		t.Fatalf("mapped job = %#v", mapped)
	}
}
