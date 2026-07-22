package application

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/J-S-Te/Basic-Platform/backend/internal/platform/filetask/domain"
)

// JobService is the generic scheduler control plane. It owns enqueue, claim, retry, cancellation
// and operator-triggered rerun state transitions; individual workers own their payload semantics.
type JobService struct {
	repository JobRepository
	ids        IDGenerator
	clock      Clock
}

func NewJobService(repository JobRepository, ids IDGenerator, clock Clock) (*JobService, error) {
	if repository == nil || ids == nil || clock == nil {
		return nil, errors.New("job service dependencies must not be nil")
	}
	return &JobService{repository: repository, ids: ids, clock: clock}, nil
}

func (service *JobService) Enqueue(ctx context.Context, input JobCreateInput) (domain.Job, error) {
	if err := validateJobInput(input); err != nil {
		return domain.Job{}, err
	}
	now := service.clock.Now().UTC()
	publicID, err := service.ids.New(now)
	if err != nil {
		return domain.Job{}, fmt.Errorf("generate async job ID: %w", err)
	}
	availableAt := now
	if input.AvailableAt != nil {
		availableAt = input.AvailableAt.UTC()
	}
	maxAttempts := input.MaxAttempts
	if maxAttempts == 0 {
		maxAttempts = 3
	}
	priority := input.Priority
	if priority == 0 {
		priority = 100
	}
	job := domain.Job{PublicID: publicID, TenantID: strings.TrimSpace(input.TenantID), ApplicationID: strings.TrimSpace(input.ApplicationID), JobType: strings.TrimSpace(input.JobType), AggregateType: strings.TrimSpace(input.AggregateType), AggregateID: strings.TrimSpace(input.AggregateID), Payload: append(json.RawMessage(nil), input.Payload...), Status: domain.JobStatusPending, Priority: priority, AvailableAt: availableAt, MaxAttempts: maxAttempts, CreatedAt: now}
	return service.repository.CreateJob(ctx, job)
}

func (service *JobService) List(ctx context.Context, tenantID string, query domain.PageRequest) (domain.PageResult[domain.Job], error) {
	if strings.TrimSpace(tenantID) == "" {
		return domain.PageResult[domain.Job]{}, validation("tenant_id is required")
	}
	query.Page, query.PageSize = normalizePage(query.Page, query.PageSize)
	return service.repository.ListJobs(ctx, strings.TrimSpace(tenantID), query)
}

func (service *JobService) Claim(ctx context.Context, workerID string, allowedTypes []string, staleBefore time.Time) (domain.Job, bool, error) {
	workerID = strings.TrimSpace(workerID)
	if workerID == "" || len(allowedTypes) == 0 {
		return domain.Job{}, false, validation("worker_id and allowed job types are required")
	}
	for _, jobType := range allowedTypes {
		if !jobTypePattern.MatchString(strings.TrimSpace(jobType)) {
			return domain.Job{}, false, validation("allowed job type is invalid")
		}
	}
	return service.repository.ClaimJob(ctx, workerID, allowedTypes, service.clock.Now().UTC(), staleBefore.UTC())
}

func (service *JobService) Complete(ctx context.Context, tenantID, jobID string) error {
	return service.repository.CompleteJob(ctx, strings.TrimSpace(tenantID), strings.TrimSpace(jobID), service.clock.Now().UTC())
}

// Fail returns retriable jobs to PENDING with the supplied backoff. Non-retriable jobs become
// DEAD; exhausted retriable jobs become FAILED and require an explicit operator retry or rerun.
func (service *JobService) Fail(ctx context.Context, job domain.Job, code, message string, retryable bool) error {
	if strings.TrimSpace(job.TenantID) == "" || strings.TrimSpace(job.PublicID) == "" || job.Status != domain.JobStatusRunning {
		return validation("running job tenant and job ID are required")
	}
	code = strings.TrimSpace(code)
	message = strings.TrimSpace(message)
	if code == "" {
		return validation("failure code is required")
	}
	if len(message) > 1000 {
		message = message[:1000]
	}
	now := service.clock.Now().UTC()
	return service.repository.FailJob(ctx, job, code, message, retryable, now.Add(backoff(job.Attempts)), now)
}

func (service *JobService) Cancel(ctx context.Context, tenantID, jobID string) error {
	return service.repository.CancelJob(ctx, strings.TrimSpace(tenantID), strings.TrimSpace(jobID), service.clock.Now().UTC())
}

func (service *JobService) Retry(ctx context.Context, tenantID, jobID string) error {
	return service.repository.RetryJob(ctx, strings.TrimSpace(tenantID), strings.TrimSpace(jobID), service.clock.Now().UTC())
}

// Rerun creates a new PENDING job rather than mutating a terminal job, preserving its operational
// history. The existing async_job schema has no parent_job_id; a lineage column needs migration if
// UIs require durable direct links between the original job and its rerun.
func (service *JobService) Rerun(ctx context.Context, tenantID, jobID string) (domain.Job, error) {
	original, err := service.repository.GetJob(ctx, strings.TrimSpace(tenantID), strings.TrimSpace(jobID))
	if err != nil {
		return domain.Job{}, err
	}
	if !terminalJobStatus(original.Status) {
		return domain.Job{}, ErrConflict
	}
	now := service.clock.Now().UTC()
	publicID, err := service.ids.New(now)
	if err != nil {
		return domain.Job{}, fmt.Errorf("generate rerun job ID: %w", err)
	}
	copy := original
	copy.ID = 0
	copy.PublicID = publicID
	copy.Status = domain.JobStatusPending
	copy.AvailableAt = now
	copy.LockedBy, copy.LockedAt = "", nil
	copy.Attempts = 0
	copy.LastErrorCode, copy.LastErrorMessage = "", ""
	copy.ResultFileID, copy.CompletedAt = "", nil
	copy.CreatedAt = now
	return service.repository.CreateRerun(ctx, copy)
}

func validateJobInput(input JobCreateInput) error {
	if strings.TrimSpace(input.TenantID) == "" || !jobTypePattern.MatchString(strings.TrimSpace(input.JobType)) {
		return validation("tenant_id and uppercase job_type are required")
	}
	if len(input.Payload) == 0 || !json.Valid(input.Payload) {
		return fmt.Errorf("%w: job payload must be valid JSON", ErrPayloadUnsafe)
	}
	if len(input.Payload) > 256<<10 {
		return fmt.Errorf("%w: job payload exceeds 256 KiB", ErrPayloadUnsafe)
	}
	if containsSensitivePayloadKey(input.Payload) {
		return fmt.Errorf("%w: job payload contains a credential-like field", ErrPayloadUnsafe)
	}
	if input.MaxAttempts > 100 {
		return validation("max_attempts must not exceed 100")
	}
	return nil
}

func terminalJobStatus(status string) bool {
	return status == domain.JobStatusSucceeded || status == domain.JobStatusFailed || status == domain.JobStatusDead || status == domain.JobStatusCancelled
}

func normalizePage(page, size int) (int, int) {
	if page < 1 {
		page = 1
	}
	if size < 1 {
		size = 20
	}
	if size > 100 {
		size = 100
	}
	return page, size
}

func backoff(attempts uint) time.Duration {
	if attempts == 0 {
		return time.Second
	}
	if attempts > 8 {
		attempts = 8
	}
	return time.Second * time.Duration(1<<(attempts-1))
}

// containsSensitivePayloadKey rejects the common credential field names before a generic job
// reaches MySQL. It is defense in depth, not an excuse for job producers to send secrets.
func containsSensitivePayloadKey(payload json.RawMessage) bool {
	var value any
	if json.Unmarshal(payload, &value) != nil {
		return true
	}
	return containsSensitiveValue(value)
}

func containsSensitiveValue(value any) bool {
	switch typed := value.(type) {
	case map[string]any:
		for key, child := range typed {
			normalized := strings.ToLower(strings.ReplaceAll(strings.ReplaceAll(strings.TrimSpace(key), "-", "_"), " ", "_"))
			switch normalized {
			case "password", "passwd", "secret", "client_secret", "access_token", "refresh_token", "id_token", "authorization", "api_key", "private_key":
				return true
			}
			if containsSensitiveValue(child) {
				return true
			}
		}
	case []any:
		for _, child := range typed {
			if containsSensitiveValue(child) {
				return true
			}
		}
	}
	return false
}
