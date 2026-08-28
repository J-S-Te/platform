package application

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/J-S-Te/Basic-Platform/internal/platform/filetask/domain"
)

var (
	idempotencyKeyPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:-]{0,127}$`)
	traceIDPattern        = regexp.MustCompile(`^[0-9a-f]{32}$`)
)

// JobService 是通用任务调度控制面，负责入队、领取、失败重试、取消和人工重跑状态转换；
// Payload 的业务含义只属于注册该 JobType 的 worker，本服务既不解释也不执行它。
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
	requestHash, err := jobRequestHash(input, priority, maxAttempts)
	if err != nil {
		return domain.Job{}, err
	}
	job := domain.Job{
		PublicID: publicID, TenantID: strings.TrimSpace(input.TenantID), ApplicationID: strings.TrimSpace(input.ApplicationID),
		JobType: strings.TrimSpace(input.JobType), AggregateType: strings.TrimSpace(input.AggregateType), AggregateID: strings.TrimSpace(input.AggregateID),
		IdempotencyKey: strings.TrimSpace(input.IdempotencyKey), Payload: append(json.RawMessage(nil), input.Payload...), RequestHash: requestHash[:],
		RequestID: strings.TrimSpace(input.RequestID), TraceID: strings.ToLower(strings.TrimSpace(input.TraceID)),
		CorrelationID: strings.TrimSpace(input.CorrelationID), BusinessRef: strings.TrimSpace(input.BusinessRef),
		Status: domain.JobStatusPending, Priority: priority, AvailableAt: availableAt, MaxAttempts: maxAttempts, CreatedAt: now,
	}
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

func (service *JobService) Complete(ctx context.Context, job domain.Job) error {
	if strings.TrimSpace(job.TenantID) == "" || strings.TrimSpace(job.PublicID) == "" || strings.TrimSpace(job.LockedBy) == "" || job.Status != domain.JobStatusRunning {
		return validation("running job tenant, job ID and worker lease are required")
	}
	return service.repository.CompleteJob(ctx, job, service.clock.Now().UTC())
}

// Fail 将仍可重试的任务按退避时间退回 PENDING；不可重试任务进入 DEAD，耗尽自动次数的任务
// 进入 FAILED。后二者都必须由操作者明确 Retry 或 Rerun，避免错误任务形成无限重试风暴。
func (service *JobService) Fail(ctx context.Context, job domain.Job, code, message string, retryable bool) error {
	if strings.TrimSpace(job.TenantID) == "" || strings.TrimSpace(job.PublicID) == "" || strings.TrimSpace(job.LockedBy) == "" || job.Status != domain.JobStatusRunning {
		return validation("running job tenant, job ID and worker lease are required")
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

// Rerun 新建 PENDING 任务而不改写终态记录，并通过 parent_job_id 持久化重跑谱系。
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
	copy.ParentJobID = original.ID
	copy.ParentPublicID = original.PublicID
	copy.IdempotencyKey = ""
	copy.Status = domain.JobStatusPending
	copy.AvailableAt = now
	copy.LockedBy, copy.LockedAt = "", nil
	copy.Attempts = 0
	copy.RetryCount = 0
	copy.LastAttemptAt = nil
	copy.LastErrorCode, copy.LastErrorMessage = "", ""
	copy.ResultFileID, copy.CompletedAt, copy.LastSucceededAt = "", nil, nil
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
	if key := strings.TrimSpace(input.IdempotencyKey); key != "" && !idempotencyKeyPattern.MatchString(key) {
		return validation("idempotency_key is invalid")
	}
	if value := strings.TrimSpace(input.TraceID); value != "" && !traceIDPattern.MatchString(strings.ToLower(value)) {
		return validation("trace_id is invalid")
	}
	for name, value := range map[string]string{"request_id": input.RequestID, "correlation_id": input.CorrelationID, "business_ref": input.BusinessRef} {
		value = strings.TrimSpace(value)
		if len(value) > 128 || strings.ContainsAny(value, "\r\n\x00") {
			return validation(name + " is invalid")
		}
	}
	return nil
}

func jobRequestHash(input JobCreateInput, priority int, maxAttempts uint) ([32]byte, error) {
	var payload any
	if err := json.Unmarshal(input.Payload, &payload); err != nil {
		return [32]byte{}, fmt.Errorf("%w: job payload must be valid JSON", ErrPayloadUnsafe)
	}
	canonical := struct {
		ApplicationID, JobType, AggregateType, AggregateID string
		Payload                                            any
		Priority                                           int
		MaxAttempts                                        uint
		AvailableAt                                        *time.Time
	}{
		strings.TrimSpace(input.ApplicationID), strings.TrimSpace(input.JobType), strings.TrimSpace(input.AggregateType), strings.TrimSpace(input.AggregateID),
		payload, priority, maxAttempts, input.AvailableAt,
	}
	encoded, err := json.Marshal(canonical)
	if err != nil {
		return [32]byte{}, fmt.Errorf("encode async job request hash: %w", err)
	}
	return sha256.Sum256(encoded), nil
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

// containsSensitivePayloadKey 在通用任务进入 MySQL 前递归拒绝常见凭据字段。这只是纵深防御，
// 生产者仍负有不发送秘密的责任；键名检查不能识别伪装在普通字段中的敏感值。
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
