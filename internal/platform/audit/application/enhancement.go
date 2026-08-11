// Package application provides controlled audit retention and dead-letter operations.
package application

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/J-S-Te/Basic-Platform/internal/platform/audit/domain"
)

// AuditRecord 是内部治理动作产生的审计记录，不暴露任何认证秘密。
type AuditRecord struct {
	EventID, TenantID, ApplicationCode, EnvironmentCode, ActorID, ActorName string
	Action, ResourceType, ResourceID, Result, RiskLevel, Summary            string
	OccurredAt                                                              time.Time
	Metadata                                                                map[string]any
}

// GovernanceAuditRecorder 由主智能体把当前审计接收服务适配进来。任何归档、清理和重放动作
// 在持久化成功前都必须调用该合同，避免产生不可追溯的治理操作。
type GovernanceAuditRecorder interface {
	RecordGovernanceAudit(context.Context, AuditRecord) error
}

// RetentionTaskInput 只能申请归档或清理任务；不存在按审计事件 ID 删除的输入。
type RetentionTaskInput struct {
	TenantID, ApplicationID, RequestedBy, Mode, ArchiveID string
	CutoffAt                                              time.Time
}

// RetentionRepository 隔离 GORM 和文件归档编排。所有读取、状态变化都必须带 tenant_id。
type RetentionRepository interface {
	CreateRetentionTask(context.Context, domain.RetentionTask) (domain.RetentionTask, error)
	ClaimRetentionTask(context.Context, string, time.Time, time.Time) (domain.RetentionTask, bool, error)
	CountRetentionEvents(context.Context, domain.RetentionTask) (uint64, error)
	SetRetentionTaskCandidateCount(context.Context, string, uint64) error
	ListRetentionEvents(context.Context, domain.RetentionTask, int) ([]domain.Event, error)
	CreateArchive(context.Context, domain.Archive, []domain.ArchiveItem) error
	ArchiveByID(context.Context, string, string) (domain.Archive, error)
	PurgeArchivedEvents(context.Context, string, string, int) (uint64, error)
	CompleteRetentionTask(context.Context, domain.RetentionTask, uint64, time.Time) error
	FailRetentionTask(context.Context, domain.RetentionTask, string, string, time.Time) error
	ListRetentionTasks(context.Context, string, RetentionTaskPageRequest) (PageResult[domain.RetentionTask], error)
}

// ArchiveWriter 将事件写入受控只读归档目录。归档正文不进入业务 MySQL。
type ArchiveWriter interface {
	WriteArchive(context.Context, domain.RetentionTask, []domain.Event, time.Time) (domain.Archive, error)
}

// RetentionService 通过任务模型管理归档和清理；故意不公开直接删除审计事件的方法。
type RetentionService struct {
	repository RetentionRepository
	writer     ArchiveWriter
	ids        IdentifierGenerator
	clock      Clock
	recorder   GovernanceAuditRecorder
}

func NewRetentionService(repository RetentionRepository, writer ArchiveWriter, ids IdentifierGenerator, clock Clock, recorder GovernanceAuditRecorder) (*RetentionService, error) {
	if repository == nil || writer == nil || ids == nil || clock == nil || recorder == nil {
		return nil, errors.New("audit retention service dependencies must not be nil")
	}
	return &RetentionService{repository: repository, writer: writer, ids: ids, clock: clock, recorder: recorder}, nil
}

// Request 只创建受控任务，不直接触碰在线审计记录。PURGE 必须引用已完成归档清单，
// 从接口层消除“未留存证据即删除”的路径。
func (service *RetentionService) Request(ctx context.Context, input RetentionTaskInput) (domain.RetentionTask, error) {
	if err := validateRetentionTask(input); err != nil {
		return domain.RetentionTask{}, err
	}
	now := service.clock.Now().UTC().Truncate(time.Millisecond)
	id, err := service.ids.New(now)
	if err != nil {
		return domain.RetentionTask{}, fmt.Errorf("create retention task id: %w", err)
	}
	task := domain.RetentionTask{TaskID: id, TenantID: strings.TrimSpace(input.TenantID), ApplicationID: strings.TrimSpace(input.ApplicationID), RequestedBy: strings.TrimSpace(input.RequestedBy), Mode: strings.TrimSpace(input.Mode), ArchiveID: strings.TrimSpace(input.ArchiveID), CutoffAt: input.CutoffAt.UTC(), Status: domain.RetentionTaskPending, CreatedAt: now}
	created, err := service.repository.CreateRetentionTask(ctx, task)
	if err != nil {
		return domain.RetentionTask{}, err
	}
	if err := service.record(ctx, created, "audit.retention.request", "retention_task", created.TaskID, "已创建受控审计归档/清理任务"); err != nil {
		return domain.RetentionTask{}, err
	}
	return created, nil
}

// RunOnce 每次只领取一个到期任务；workerID 在一个执行实例生命周期内必须稳定，仓储才能
// 区分正常持有与超时接管。RetentionTaskPageRequest 仅用于租户隔离的运维历史查询。
type RetentionTaskPageRequest struct {
	Page, PageSize              int
	ApplicationID, Mode, Status string
}

// List returns task state without exposing archive file contents.
func (service *RetentionService) List(ctx context.Context, tenantID string, query RetentionTaskPageRequest) (PageResult[domain.RetentionTask], error) {
	if strings.TrimSpace(tenantID) == "" {
		return PageResult[domain.RetentionTask]{}, validation("tenant_id is required")
	}
	query.Page, query.PageSize = normalizedPage(query.Page, query.PageSize)
	query.ApplicationID = strings.TrimSpace(query.ApplicationID)
	query.Mode = strings.TrimSpace(query.Mode)
	query.Status = strings.TrimSpace(query.Status)
	return service.repository.ListRetentionTasks(ctx, strings.TrimSpace(tenantID), query)
}

func (service *RetentionService) RunOnce(ctx context.Context, workerID string, staleBefore time.Time) (bool, error) {
	workerID = strings.TrimSpace(workerID)
	if workerID == "" {
		return false, validation("worker_id is required")
	}
	now := service.clock.Now().UTC().Truncate(time.Millisecond)
	task, claimed, err := service.repository.ClaimRetentionTask(ctx, workerID, now, staleBefore.UTC())
	if err != nil || !claimed {
		return claimed, err
	}
	if task.Mode == domain.RetentionTaskArchive {
		return true, service.archive(ctx, task, now)
	}
	return true, service.purge(ctx, task, now)
}

const retentionArchiveEventLimit = 5000
const retentionPurgeBatchSize = 5000

func (service *RetentionService) archive(ctx context.Context, task domain.RetentionTask, now time.Time) error {
	candidateCount, err := service.repository.CountRetentionEvents(ctx, task)
	if err != nil {
		return service.fail(ctx, task, "ARCHIVE_SOURCE_COUNT_FAILED", err, now)
	}
	// 一个任务只生成一个由清单背书的不可变文件。超过上限时整项拒绝，不能把截断文件
	// 标成成功，否则后续 PURGE 会把“未归档部分”误认为已有留存副本。
	if err := service.repository.SetRetentionTaskCandidateCount(ctx, task.TaskID, candidateCount); err != nil {
		return service.fail(ctx, task, "ARCHIVE_CANDIDATE_COUNT_UPDATE_FAILED", err, now)
	}
	if candidateCount > retentionArchiveEventLimit {
		return service.fail(ctx, task, "ARCHIVE_EVENT_LIMIT_EXCEEDED", fmt.Errorf("archive candidate count %d exceeds limit %d", candidateCount, retentionArchiveEventLimit), now)
	}
	if candidateCount == 0 {
		if err := service.record(ctx, task, "audit.retention.archive", "audit_retention_task", task.TaskID, "没有符合条件的在线审计记录，归档任务已完成"); err != nil {
			return service.fail(ctx, task, "ARCHIVE_AUDIT_FAILED", err, now)
		}
		return service.repository.CompleteRetentionTask(ctx, task, 0, now)
	}
	events, err := service.repository.ListRetentionEvents(ctx, task, int(candidateCount))
	if err != nil {
		return service.fail(ctx, task, "ARCHIVE_SOURCE_READ_FAILED", err, now)
	}
	archiveID, err := service.ids.New(now.Add(time.Millisecond))
	if err != nil {
		return service.fail(ctx, task, "ARCHIVE_ID_FAILED", err, now)
	}
	task.ArchiveID = archiveID
	archive, err := service.writer.WriteArchive(ctx, task, events, now)
	if err != nil {
		return service.fail(ctx, task, "ARCHIVE_WRITE_FAILED", err, now)
	}
	archive.ArchiveID = archiveID
	archive.TenantID, archive.ApplicationID = task.TenantID, task.ApplicationID
	archive.EventCount = uint64(len(events))
	archive.CreatedAt = now
	items := make([]domain.ArchiveItem, 0, len(events))
	for _, event := range events {
		var rowID uint64
		_, _ = fmt.Sscan(event.ID, &rowID)
		items = append(items, domain.ArchiveItem{ArchiveID: archiveID, AuditRowID: rowID, OccurredMonth: uint(event.OccurredAt.Year()*100 + int(event.OccurredAt.Month()))})
	}
	if err := service.repository.CreateArchive(ctx, archive, items); err != nil {
		return service.fail(ctx, task, "ARCHIVE_MANIFEST_FAILED", err, now)
	}
	if err := service.record(ctx, task, "audit.retention.archive", "audit_archive", archiveID, "审计记录已归档到受控只读文件"); err != nil {
		return service.fail(ctx, task, "ARCHIVE_AUDIT_FAILED", err, now)
	}
	return service.repository.CompleteRetentionTask(ctx, task, uint64(len(events)), now)
}

func (service *RetentionService) purge(ctx context.Context, task domain.RetentionTask, now time.Time) error {
	archive, err := service.repository.ArchiveByID(ctx, task.TenantID, task.ArchiveID)
	if err != nil {
		return service.fail(ctx, task, "ARCHIVE_NOT_FOUND", err, now)
	}
	if err := service.repository.SetRetentionTaskCandidateCount(ctx, task.TaskID, archive.EventCount); err != nil {
		return service.fail(ctx, task, "PURGE_CANDIDATE_COUNT_UPDATE_FAILED", err, now)
	}
	if archive.EventCount == 0 {
		if err := service.record(ctx, task, "audit.retention.purge", "audit_archive", archive.ArchiveID, "空归档清理任务已完成"); err != nil {
			return service.fail(ctx, task, "PURGE_AUDIT_FAILED", err, now)
		}
		return service.repository.CompleteRetentionTask(ctx, task, 0, now)
	}
	var processed uint64
	for {
		batchCount, err := service.repository.PurgeArchivedEvents(ctx, task.TenantID, task.ArchiveID, retentionPurgeBatchSize)
		if err != nil {
			return service.fail(ctx, task, "PURGE_FAILED", err, now)
		}
		processed += batchCount
		if batchCount < retentionPurgeBatchSize {
			break
		}
	}
	if err := service.record(ctx, task, "audit.retention.purge", "audit_archive", archive.ArchiveID, "已按归档清单清理在线审计记录"); err != nil {
		return service.fail(ctx, task, "PURGE_AUDIT_FAILED", err, now)
	}
	return service.repository.CompleteRetentionTask(ctx, task, processed, now)
}

func (service *RetentionService) fail(ctx context.Context, task domain.RetentionTask, code string, cause error, now time.Time) error {
	if markErr := service.repository.FailRetentionTask(ctx, task, code, truncateError(cause), now); markErr != nil {
		return fmt.Errorf("%w; mark retention task failed: %v", cause, markErr)
	}
	return cause
}

func (service *RetentionService) record(ctx context.Context, task domain.RetentionTask, action, resourceType, resourceID, summary string) error {
	eventID, err := service.ids.New(service.clock.Now().UTC())
	if err != nil {
		return err
	}
	return service.recorder.RecordGovernanceAudit(ctx, AuditRecord{EventID: eventID, TenantID: task.TenantID, ActorID: task.RequestedBy, Action: action, ResourceType: resourceType, ResourceID: resourceID, Result: "SUCCESS", RiskLevel: "HIGH", Summary: summary, OccurredAt: service.clock.Now().UTC(), Metadata: map[string]any{"task_id": task.TaskID, "mode": task.Mode, "cutoff_at": task.CutoffAt.Format(time.RFC3339Nano)}})
}

func validateRetentionTask(input RetentionTaskInput) error {
	if strings.TrimSpace(input.TenantID) == "" || strings.TrimSpace(input.ApplicationID) == "" || strings.TrimSpace(input.RequestedBy) == "" || input.CutoffAt.IsZero() {
		return validation("tenant_id, application_id, requested_by and cutoff_at are required")
	}
	mode := strings.TrimSpace(input.Mode)
	if mode != domain.RetentionTaskArchive && mode != domain.RetentionTaskPurge {
		return validation("mode must be ARCHIVE or PURGE")
	}
	if mode == domain.RetentionTaskPurge && strings.TrimSpace(input.ArchiveID) == "" {
		return validation("archive_id is required for PURGE")
	}
	return nil
}

// DeadLetterInput 来自投递适配器的接收失败路径。ErrorMessage 与 Event.Metadata 在进入此层前
// 不得包含访问令牌或客户端密钥，死信是可长期保留并可被运维人员查看的故障证据。
type DeadLetterInput struct {
	TenantID, ApplicationCode, EnvironmentCode, ErrorCode, ErrorMessage string
	Event                                                               EventInput
}

type DeadLetterPageRequest struct {
	Page, PageSize          int
	ApplicationCode, Status string
}

// DeadLetterRepository 保存已脱敏载荷及全部人工状态转换；每个操作都必须同时带租户边界。
type DeadLetterRepository interface {
	CreateDeadLetter(context.Context, domain.DeadLetter) (domain.DeadLetter, error)
	GetDeadLetter(context.Context, string, string) (domain.DeadLetter, error)
	ListDeadLetters(context.Context, string, DeadLetterPageRequest) (PageResult[domain.DeadLetter], error)
	MarkDeadLetterReplayed(context.Context, string, string, time.Time) error
	MarkDeadLetterReplayFailed(context.Context, string, string, string, string, time.Time) error
	DeadLetterStatus(context.Context, string, string) (domain.DeadLetterStatus, error)
}

// EventIngestor is intentionally compatible with the current audit application Service.
type EventIngestor interface {
	Ingest(context.Context, string, EventInput) (domain.Receipt, error)
}

// DeadLetterService provides operations pages with tenant-isolated list/status/replay contracts.
type DeadLetterService struct {
	repository DeadLetterRepository
	ingestor   EventIngestor
	ids        IdentifierGenerator
	clock      Clock
	recorder   GovernanceAuditRecorder
}

func NewDeadLetterService(repository DeadLetterRepository, ingestor EventIngestor, ids IdentifierGenerator, clock Clock, recorder GovernanceAuditRecorder) (*DeadLetterService, error) {
	if repository == nil || ingestor == nil || ids == nil || clock == nil || recorder == nil {
		return nil, errors.New("audit dead-letter service dependencies must not be nil")
	}
	return &DeadLetterService{repository: repository, ingestor: ingestor, ids: ids, clock: clock, recorder: recorder}, nil
}

func (service *DeadLetterService) RecordFailure(ctx context.Context, input DeadLetterInput) (domain.DeadLetter, error) {
	if strings.TrimSpace(input.TenantID) == "" || strings.TrimSpace(input.ApplicationCode) == "" || strings.TrimSpace(input.ErrorCode) == "" {
		return domain.DeadLetter{}, validation("tenant_id, application_code and error_code are required")
	}
	if err := validateInput(input.TenantID, input.Event); err != nil {
		return domain.DeadLetter{}, err
	}
	now := service.clock.Now().UTC().Truncate(time.Millisecond)
	id, err := service.ids.New(now)
	if err != nil {
		return domain.DeadLetter{}, err
	}
	payload, err := marshalDeadLetterEvent(normalizeInput(input.Event))
	if err != nil {
		return domain.DeadLetter{}, err
	}
	return service.repository.CreateDeadLetter(ctx, domain.DeadLetter{DeadLetterID: id, TenantID: strings.TrimSpace(input.TenantID), ApplicationCode: strings.TrimSpace(input.ApplicationCode), EnvironmentCode: strings.TrimSpace(input.EnvironmentCode), EventID: input.Event.EventID, Status: domain.DeadLetterPending, Payload: payload, LastErrorCode: []byte(strings.TrimSpace(input.ErrorCode)), LastErrorMessage: []byte(trim(input.ErrorMessage, 1000)), Attempts: 1, CreatedAt: now, UpdatedAt: now})
}

// Get returns controlled dead-letter metadata for an operator. The returned model is converted by
// the HTTP adapter, which intentionally omits Payload and LastErrorMessage.
func (service *DeadLetterService) Get(ctx context.Context, tenantID, deadLetterID string) (domain.DeadLetter, error) {
	if strings.TrimSpace(tenantID) == "" || strings.TrimSpace(deadLetterID) == "" {
		return domain.DeadLetter{}, validation("tenant_id and dead_letter_id are required")
	}
	return service.repository.GetDeadLetter(ctx, strings.TrimSpace(tenantID), strings.TrimSpace(deadLetterID))
}

func (service *DeadLetterService) List(ctx context.Context, tenantID string, query DeadLetterPageRequest) (PageResult[domain.DeadLetter], error) {
	if strings.TrimSpace(tenantID) == "" {
		return PageResult[domain.DeadLetter]{}, validation("tenant_id is required")
	}
	query.Page, query.PageSize = normalizedPage(query.Page, query.PageSize)
	return service.repository.ListDeadLetters(ctx, strings.TrimSpace(tenantID), query)
}

func (service *DeadLetterService) Status(ctx context.Context, tenantID, applicationCode string) (domain.DeadLetterStatus, error) {
	if strings.TrimSpace(tenantID) == "" {
		return domain.DeadLetterStatus{}, validation("tenant_id is required")
	}
	return service.repository.DeadLetterStatus(ctx, strings.TrimSpace(tenantID), strings.TrimSpace(applicationCode))
}

// Replay 复用已脱敏且经过校验的原事件，保持原 event_id，从而让接收端幂等键阻止重复写入；
// 重放成功本身也属于高风险治理动作，必须另记一条审计记录。
func (service *DeadLetterService) Replay(ctx context.Context, tenantID, deadLetterID, operatorID string) (domain.Receipt, error) {
	if strings.TrimSpace(tenantID) == "" || strings.TrimSpace(deadLetterID) == "" || strings.TrimSpace(operatorID) == "" {
		return domain.Receipt{}, validation("tenant_id, dead_letter_id and operator_id are required")
	}
	letter, err := service.repository.GetDeadLetter(ctx, strings.TrimSpace(tenantID), strings.TrimSpace(deadLetterID))
	if err != nil {
		return domain.Receipt{}, err
	}
	if letter.Status != domain.DeadLetterPending {
		return domain.Receipt{}, ErrConflict
	}
	event, err := unmarshalDeadLetterEvent(letter.Payload)
	if err != nil {
		return domain.Receipt{}, fmt.Errorf("%w: dead-letter payload is invalid", ErrValidation)
	}
	receipt, err := service.ingestor.Ingest(ctx, tenantID, event)
	if err != nil {
		now := service.clock.Now().UTC().Truncate(time.Millisecond)
		// 运维状态只保留有界且安全的失败摘要，不落上游令牌、响应正文或原始事件载荷。
		if markErr := service.repository.MarkDeadLetterReplayFailed(ctx, tenantID, letter.DeadLetterID, "AUDIT_INGEST_REPLAY_FAILED", truncateError(err), now); markErr != nil {
			return domain.Receipt{}, fmt.Errorf("record audit dead-letter replay failure: %w", markErr)
		}
		return domain.Receipt{}, err
	}
	now := service.clock.Now().UTC().Truncate(time.Millisecond)
	if err := service.repository.MarkDeadLetterReplayed(ctx, tenantID, letter.DeadLetterID, now); err != nil {
		return domain.Receipt{}, err
	}
	eventID, err := service.ids.New(now)
	if err != nil {
		return domain.Receipt{}, err
	}
	if err := service.recorder.RecordGovernanceAudit(ctx, AuditRecord{EventID: eventID, TenantID: tenantID, ActorID: strings.TrimSpace(operatorID), Action: "audit.dead_letter.replay", ResourceType: "audit_dead_letter", ResourceID: letter.DeadLetterID, Result: "SUCCESS", RiskLevel: "HIGH", Summary: "人工重放审计死信成功", OccurredAt: now, Metadata: map[string]any{"source_event_id": letter.EventID, "receipt_status": receipt.Status}}); err != nil {
		return domain.Receipt{}, err
	}
	return receipt, nil
}

func normalizedPage(page, pageSize int) (int, int) {
	if page < 1 {
		page = 1
	}
	if pageSize < 1 {
		pageSize = 20
	}
	if pageSize > 100 {
		pageSize = 100
	}
	return page, pageSize
}

func truncateError(err error) string { return trim(err.Error(), 1000) }
func trim(value string, limit int) string {
	value = strings.TrimSpace(value)
	if len(value) > limit {
		return value[:limit]
	}
	return value
}

// IngestionReceiptPageRequest limits receipt queries to a tenant and optional verified delivery
// dimensions. Receipt bodies do not contain audit-event payloads.
type IngestionReceiptPageRequest struct {
	Page, PageSize                                      int
	ApplicationCode, EnvironmentCode, Status, RequestID string
	CorrelationID                                       string
	ReceivedFrom, ReceivedTo                            *time.Time
}

// IngestionReceiptRepository reads receiver-side batch delivery acknowledgements.
type IngestionReceiptRepository interface {
	ListIngestionReceipts(context.Context, string, IngestionReceiptPageRequest) (PageResult[domain.IngestionReceipt], error)
}

// IngestionReceiptService provides tenant-isolated operational acknowledgement queries.
type IngestionReceiptService struct{ repository IngestionReceiptRepository }

func NewIngestionReceiptService(repository IngestionReceiptRepository) (*IngestionReceiptService, error) {
	if repository == nil {
		return nil, errors.New("audit ingestion receipt service repository must not be nil")
	}
	return &IngestionReceiptService{repository: repository}, nil
}

func (service *IngestionReceiptService) List(ctx context.Context, tenantID string, query IngestionReceiptPageRequest) (PageResult[domain.IngestionReceipt], error) {
	if strings.TrimSpace(tenantID) == "" {
		return PageResult[domain.IngestionReceipt]{}, validation("tenant_id is required")
	}
	query.Page, query.PageSize = normalizedPage(query.Page, query.PageSize)
	if query.ReceivedFrom != nil && query.ReceivedTo != nil && query.ReceivedFrom.After(*query.ReceivedTo) {
		return PageResult[domain.IngestionReceipt]{}, validation("received_from must not be after received_to")
	}
	return service.repository.ListIngestionReceipts(ctx, strings.TrimSpace(tenantID), normalizeReceiptPageRequest(query))
}

func normalizeReceiptPageRequest(query IngestionReceiptPageRequest) IngestionReceiptPageRequest {
	query.ApplicationCode = strings.TrimSpace(query.ApplicationCode)
	query.EnvironmentCode = strings.TrimSpace(query.EnvironmentCode)
	query.Status = strings.TrimSpace(query.Status)
	query.RequestID = strings.TrimSpace(query.RequestID)
	query.CorrelationID = strings.TrimSpace(query.CorrelationID)
	return query
}

// ReplayBatch 独立重放每条待处理死信：单条失败继续保持可受控重试，不阻塞同批其他明确选择项；
// 这不是数据库原子批次，返回结果必须逐项解释。
func (service *DeadLetterService) ReplayBatch(ctx context.Context, tenantID string, deadLetterIDs []string, operatorID string) ([]domain.DeadLetterReplayResult, error) {
	if strings.TrimSpace(tenantID) == "" || strings.TrimSpace(operatorID) == "" {
		return nil, validation("tenant_id and operator_id are required")
	}
	if len(deadLetterIDs) == 0 || len(deadLetterIDs) > 100 {
		return nil, validation("dead_letter_ids must contain 1 to 100 items")
	}
	seen := make(map[string]struct{}, len(deadLetterIDs))
	results := make([]domain.DeadLetterReplayResult, 0, len(deadLetterIDs))
	for _, rawID := range deadLetterIDs {
		deadLetterID := strings.TrimSpace(rawID)
		if deadLetterID == "" {
			return nil, validation("dead_letter_ids must not contain empty values")
		}
		if _, exists := seen[deadLetterID]; exists {
			return nil, validation("dead_letter_ids must not contain duplicates")
		}
		seen[deadLetterID] = struct{}{}

		letter, getErr := service.repository.GetDeadLetter(ctx, strings.TrimSpace(tenantID), deadLetterID)
		if getErr != nil {
			results = append(results, replayFailureResult(deadLetterID, "", replayErrorCode(getErr)))
			continue
		}
		receipt, replayErr := service.Replay(ctx, tenantID, deadLetterID, operatorID)
		if replayErr != nil {
			results = append(results, replayFailureResult(deadLetterID, letter.EventID, replayErrorCode(replayErr)))
			continue
		}
		results = append(results, domain.DeadLetterReplayResult{DeadLetterID: deadLetterID, EventID: letter.EventID, Status: domain.DeadLetterReplayed, ReceiptStatus: receipt.Status, ReplayedAt: service.clock.Now().UTC().Truncate(time.Millisecond)})
	}
	return results, nil
}

func replayFailureResult(deadLetterID, eventID, errorCode string) domain.DeadLetterReplayResult {
	return domain.DeadLetterReplayResult{DeadLetterID: deadLetterID, EventID: eventID, Status: domain.DeadLetterPending, ErrorCode: errorCode}
}

func replayErrorCode(err error) string {
	switch {
	case errors.Is(err, ErrNotFound):
		return "AUDIT_DEAD_LETTER_NOT_FOUND"
	case errors.Is(err, ErrConflict):
		return "AUDIT_DEAD_LETTER_NOT_REPLAYABLE"
	case errors.Is(err, ErrValidation):
		return "AUDIT_DEAD_LETTER_REPLAY_INVALID"
	default:
		return "AUDIT_DEAD_LETTER_REPLAY_FAILED"
	}
}
