package infrastructure

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/J-S-Te/Basic-Platform/backend/internal/platform/audit/application"
	"github.com/J-S-Te/Basic-Platform/backend/internal/platform/audit/domain"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

type retentionTaskModel struct {
	TaskID, TenantID, ApplicationID, RequestedBy, Mode, Status, ArchiveID, WorkerID string
	CutoffAt                                                                        time.Time
	CandidateCount, ProcessedCount                                                  uint64
	FailureCode, FailureMessage                                                     *string
	CreatedAt                                                                       time.Time
	StartedAt, CompletedAt                                                          *time.Time
}

func (retentionTaskModel) TableName() string { return "audit_retention_task" }

type archiveModel struct {
	ArchiveID, TenantID, ApplicationID, StorageRelativePath, MediaType string
	SHA256                                                             []byte
	EventCount                                                         uint64
	OccurredFrom, OccurredTo, CreatedAt                                time.Time
}

func (archiveModel) TableName() string { return "audit_archive" }

type archiveItemModel struct {
	ArchiveID     string
	AuditRowID    uint64
	OccurredMonth uint
	PurgedAt      *time.Time
}

func (archiveItemModel) TableName() string { return "audit_archive_item" }

type deadLetterModel struct {
	DeadLetterID, TenantID, ApplicationCode, EnvironmentCode, EventID, Status string
	Payload, LastErrorCode, LastErrorMessage                                  []byte
	Attempts                                                                  uint
	CreatedAt, UpdatedAt                                                      time.Time
	ReplayedAt                                                                *time.Time
}

func (deadLetterModel) TableName() string { return "audit_dead_letter" }

// CreateRetentionTask inserts a task record only; worker execution is coordinated by row locks.
func (r *Repository) CreateRetentionTask(ctx context.Context, task domain.RetentionTask) (domain.RetentionTask, error) {
	model := retentionTaskModel{TaskID: task.TaskID, TenantID: task.TenantID, ApplicationID: task.ApplicationID, RequestedBy: task.RequestedBy, Mode: task.Mode, Status: task.Status, ArchiveID: task.ArchiveID, CutoffAt: task.CutoffAt, CreatedAt: task.CreatedAt}
	if err := r.database.WithContext(ctx).Create(&model).Error; err != nil {
		return domain.RetentionTask{}, err
	}
	return task, nil
}

func (r *Repository) ClaimRetentionTask(ctx context.Context, workerID string, now, staleBefore time.Time) (domain.RetentionTask, bool, error) {
	var model retentionTaskModel
	err := r.database.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		query := tx.Clauses(clause.Locking{Strength: "UPDATE", Options: "SKIP LOCKED"}).Where("status = ? OR (status = ? AND started_at < ?)", domain.RetentionTaskPending, domain.RetentionTaskRunning, staleBefore).Order("created_at ASC").Limit(1)
		if err := query.Take(&model).Error; err != nil {
			return err
		}
		return tx.Model(&retentionTaskModel{}).Where("task_id = ?", model.TaskID).Updates(map[string]any{"status": domain.RetentionTaskRunning, "worker_id": workerID, "started_at": now, "failure_code": nil, "failure_message": nil}).Error
	})
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return domain.RetentionTask{}, false, nil
	}
	if err != nil {
		return domain.RetentionTask{}, false, err
	}
	model.Status, model.WorkerID = domain.RetentionTaskRunning, workerID
	model.StartedAt = &now
	model.FailureCode, model.FailureMessage = nil, nil
	return toRetentionTask(model), true, nil
}

// CountRetentionEvents checks the exact immutable source set before one archive file is written.
func (r *Repository) CountRetentionEvents(ctx context.Context, task domain.RetentionTask) (uint64, error) {
	var total int64
	err := r.database.WithContext(ctx).Table("audit_event").
		Where("tenant_id = ? AND application_id = ? AND occurred_at < ?", task.TenantID, task.ApplicationID, task.CutoffAt.UTC()).
		Count(&total).Error
	if err != nil {
		return 0, err
	}
	if total < 0 {
		return 0, application.ErrValidation
	}
	return uint64(total), nil
}

// SetRetentionTaskCandidateCount records the verified source-set size while the task is running.
func (r *Repository) SetRetentionTaskCandidateCount(ctx context.Context, taskID string, candidateCount uint64) error {
	result := r.database.WithContext(ctx).Model(&retentionTaskModel{}).
		Where("task_id = ? AND status = ?", taskID, domain.RetentionTaskRunning).
		Update("candidate_count", candidateCount)
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected != 1 {
		return application.ErrConflict
	}
	return nil
}

func (r *Repository) ListRetentionEvents(ctx context.Context, task domain.RetentionTask, limit int) ([]domain.Event, error) {
	if limit < 1 {
		return nil, application.ErrValidation
	}
	var rows []retentionEventRow
	query := r.database.WithContext(ctx).Table("audit_event").
		Select("audit_event.*, app.code AS application_code, app.name AS application_name, env.environment AS environment_code").
		Joins("JOIN platform_application app ON app.id = audit_event.application_id").
		Joins("JOIN platform_application_environment env ON env.id = audit_event.environment_id").
		Where("audit_event.tenant_id = ? AND audit_event.application_id = ? AND audit_event.occurred_at < ?", task.TenantID, task.ApplicationID, task.CutoffAt.UTC()).
		Order("audit_event.occurred_at ASC, audit_event.id ASC").Limit(limit)
	if err := query.Scan(&rows).Error; err != nil {
		return nil, err
	}
	events := make([]domain.Event, 0, len(rows))
	for _, row := range rows {
		events = append(events, toEvent(row.eventModel, row.ApplicationCode, row.ApplicationName, row.EnvironmentCode))
	}
	return events, nil
}

type retentionEventRow struct {
	eventModel
	ApplicationCode, ApplicationName, EnvironmentCode string
}

func (r *Repository) CreateArchive(ctx context.Context, archive domain.Archive, items []domain.ArchiveItem) error {
	return r.database.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		model := archiveModel{ArchiveID: archive.ArchiveID, TenantID: archive.TenantID, ApplicationID: archive.ApplicationID, StorageRelativePath: archive.StorageRelativePath, MediaType: archive.MediaType, SHA256: append([]byte(nil), archive.SHA256...), EventCount: archive.EventCount, OccurredFrom: archive.OccurredFrom, OccurredTo: archive.OccurredTo, CreatedAt: archive.CreatedAt}
		if err := tx.Create(&model).Error; err != nil {
			return err
		}
		if len(items) == 0 {
			return nil
		}
		models := make([]archiveItemModel, 0, len(items))
		for _, item := range items {
			models = append(models, archiveItemModel{ArchiveID: archive.ArchiveID, AuditRowID: item.AuditRowID, OccurredMonth: item.OccurredMonth})
		}
		return tx.CreateInBatches(models, 500).Error
	})
}

func (r *Repository) ArchiveByID(ctx context.Context, tenantID, archiveID string) (domain.Archive, error) {
	var model archiveModel
	if err := r.database.WithContext(ctx).Where("tenant_id = ? AND archive_id = ?", tenantID, archiveID).Take(&model).Error; err != nil {
		return domain.Archive{}, r.mapError(err)
	}
	return toArchive(model), nil
}

// PurgeArchivedEvents is intentionally only reachable through RetentionService. It deletes an
// event only when an audit_archive_item record exists for this tenant-owned archive and returns
// the number of archive-manifest items marked processed (not merely rows physically deleted).
func (r *Repository) PurgeArchivedEvents(ctx context.Context, tenantID, archiveID string, limit int) (uint64, error) {
	if limit < 1 {
		return 0, application.ErrValidation
	}
	var purged uint64
	err := r.database.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var items []archiveItemModel
		if err := tx.Table("audit_archive_item item").
			Select("item.*").
			Joins("JOIN audit_archive archive ON archive.archive_id = item.archive_id").
			Where("archive.tenant_id = ? AND item.archive_id = ? AND item.purged_at IS NULL", tenantID, archiveID).
			Order("item.audit_row_id ASC").Limit(limit).
			Clauses(clause.Locking{Strength: "UPDATE"}).Find(&items).Error; err != nil {
			return err
		}
		now := time.Now().UTC().Truncate(time.Millisecond)
		for _, item := range items {
			result := tx.Where("id = ? AND occurred_month = ?", item.AuditRowID, item.OccurredMonth).Delete(&eventModel{})
			if result.Error != nil {
				return result.Error
			}
			if err := tx.Model(&archiveItemModel{}).Where("archive_id = ? AND audit_row_id = ? AND occurred_month = ? AND purged_at IS NULL", archiveID, item.AuditRowID, item.OccurredMonth).Update("purged_at", now).Error; err != nil {
				return err
			}
			purged++
		}
		return nil
	})
	return purged, err
}

func (r *Repository) CompleteRetentionTask(ctx context.Context, task domain.RetentionTask, processed uint64, completedAt time.Time) error {
	updates := map[string]any{"status": domain.RetentionTaskCompleted, "processed_count": processed, "completed_at": completedAt, "failure_code": nil, "failure_message": nil}
	if task.Mode == domain.RetentionTaskArchive && strings.TrimSpace(task.ArchiveID) != "" {
		updates["archive_id"] = task.ArchiveID
	}
	result := r.database.WithContext(ctx).Model(&retentionTaskModel{}).Where("task_id = ? AND status = ?", task.TaskID, domain.RetentionTaskRunning).Updates(updates)
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected != 1 {
		return application.ErrConflict
	}
	return nil
}

func (r *Repository) FailRetentionTask(ctx context.Context, task domain.RetentionTask, code, message string, now time.Time) error {
	result := r.database.WithContext(ctx).Model(&retentionTaskModel{}).Where("task_id = ? AND status = ?", task.TaskID, domain.RetentionTaskRunning).Updates(map[string]any{"status": domain.RetentionTaskFailed, "failure_code": nullable(code), "failure_message": nullable(message), "completed_at": now})
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected != 1 {
		return application.ErrConflict
	}
	return nil
}

// ListRetentionTasks exposes task history to a tenant-scoped operations console.
func (r *Repository) ListRetentionTasks(ctx context.Context, tenantID string, query application.RetentionTaskPageRequest) (application.PageResult[domain.RetentionTask], error) {
	db := r.database.WithContext(ctx).Model(&retentionTaskModel{}).Where("tenant_id = ?", tenantID)
	if query.ApplicationID != "" {
		db = db.Where("application_id = ?", query.ApplicationID)
	}
	if query.Mode != "" {
		db = db.Where("mode = ?", query.Mode)
	}
	if query.Status != "" {
		db = db.Where("status = ?", query.Status)
	}
	var total int64
	if err := db.Count(&total).Error; err != nil {
		return application.PageResult[domain.RetentionTask]{}, err
	}
	var models []retentionTaskModel
	if err := db.Order("created_at DESC, task_id DESC").Offset((query.Page - 1) * query.PageSize).Limit(query.PageSize).Find(&models).Error; err != nil {
		return application.PageResult[domain.RetentionTask]{}, err
	}
	items := make([]domain.RetentionTask, 0, len(models))
	for _, model := range models {
		items = append(items, toRetentionTask(model))
	}
	return application.PageResult[domain.RetentionTask]{Items: items, Page: query.Page, PageSize: query.PageSize, Total: total}, nil
}

func (r *Repository) CreateDeadLetter(ctx context.Context, letter domain.DeadLetter) (domain.DeadLetter, error) {
	model := deadLetterModel{DeadLetterID: letter.DeadLetterID, TenantID: letter.TenantID, ApplicationCode: letter.ApplicationCode, EnvironmentCode: letter.EnvironmentCode, EventID: letter.EventID, Status: letter.Status, Payload: append([]byte(nil), letter.Payload...), LastErrorCode: []byte(letter.LastErrorCode), LastErrorMessage: []byte(letter.LastErrorMessage), Attempts: letter.Attempts, CreatedAt: letter.CreatedAt, UpdatedAt: letter.UpdatedAt}
	if err := r.database.WithContext(ctx).Create(&model).Error; err != nil {
		return domain.DeadLetter{}, err
	}
	return letter, nil
}

func (r *Repository) GetDeadLetter(ctx context.Context, tenantID, deadLetterID string) (domain.DeadLetter, error) {
	var model deadLetterModel
	if err := r.database.WithContext(ctx).Where("tenant_id = ? AND dead_letter_id = ?", tenantID, deadLetterID).Take(&model).Error; err != nil {
		return domain.DeadLetter{}, r.mapError(err)
	}
	return toDeadLetter(model), nil
}

func (r *Repository) ListDeadLetters(ctx context.Context, tenantID string, query application.DeadLetterPageRequest) (application.PageResult[domain.DeadLetter], error) {
	db := r.database.WithContext(ctx).Model(&deadLetterModel{}).Where("tenant_id = ?", tenantID)
	if query.ApplicationCode != "" {
		db = db.Where("application_code = ?", query.ApplicationCode)
	}
	if query.Status != "" {
		db = db.Where("status = ?", query.Status)
	}
	var total int64
	if err := db.Count(&total).Error; err != nil {
		return application.PageResult[domain.DeadLetter]{}, err
	}
	var models []deadLetterModel
	if err := db.Order("created_at DESC").Offset((query.Page - 1) * query.PageSize).Limit(query.PageSize).Find(&models).Error; err != nil {
		return application.PageResult[domain.DeadLetter]{}, err
	}
	items := make([]domain.DeadLetter, 0, len(models))
	for _, model := range models {
		items = append(items, toDeadLetter(model))
	}
	return application.PageResult[domain.DeadLetter]{Items: items, Page: query.Page, PageSize: query.PageSize, Total: total}, nil
}

func (r *Repository) MarkDeadLetterReplayed(ctx context.Context, tenantID, deadLetterID string, replayedAt time.Time) error {
	result := r.database.WithContext(ctx).Model(&deadLetterModel{}).Where("tenant_id = ? AND dead_letter_id = ? AND status = ?", tenantID, deadLetterID, domain.DeadLetterPending).Updates(map[string]any{"status": domain.DeadLetterReplayed, "attempts": gorm.Expr("attempts + ?", 1), "replayed_at": replayedAt, "updated_at": replayedAt})
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected != 1 {
		return application.ErrConflict
	}
	return nil
}

// MarkDeadLetterReplayFailed keeps a pending message eligible for a later controlled replay while
// recording the latest safe failure summary and incrementing its attempt count atomically.
func (r *Repository) MarkDeadLetterReplayFailed(ctx context.Context, tenantID, deadLetterID, errorCode, errorMessage string, attemptedAt time.Time) error {
	result := r.database.WithContext(ctx).Model(&deadLetterModel{}).
		Where("tenant_id = ? AND dead_letter_id = ? AND status = ?", tenantID, deadLetterID, domain.DeadLetterPending).
		Updates(map[string]any{
			"attempts":           gorm.Expr("attempts + ?", 1),
			"last_error_code":    []byte(strings.TrimSpace(errorCode)),
			"last_error_message": []byte(trimAuditFailureMessage(errorMessage)),
			"updated_at":         attemptedAt.UTC(),
		})
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected != 1 {
		return application.ErrConflict
	}
	return nil
}

func trimAuditFailureMessage(value string) string {
	value = strings.TrimSpace(value)
	if len(value) > 1000 {
		return value[:1000]
	}
	return value
}

func (r *Repository) DeadLetterStatus(ctx context.Context, tenantID, applicationCode string) (domain.DeadLetterStatus, error) {
	type row struct {
		Status string
		Count  uint64
	}
	db := r.database.WithContext(ctx).Model(&deadLetterModel{}).Select("status, COUNT(*) AS count").Where("tenant_id = ?", tenantID)
	if applicationCode != "" {
		db = db.Where("application_code = ?", applicationCode)
	}
	var rows []row
	if err := db.Group("status").Scan(&rows).Error; err != nil {
		return domain.DeadLetterStatus{}, err
	}
	status := domain.DeadLetterStatus{TenantID: tenantID, ApplicationCode: applicationCode}
	for _, row := range rows {
		switch row.Status {
		case domain.DeadLetterPending:
			status.Pending = row.Count
		case domain.DeadLetterReplayed:
			status.Replayed = row.Count
		case domain.DeadLetterIgnored:
			status.Ignored = row.Count
		}
	}
	var oldest time.Time
	oldestQuery := r.database.WithContext(ctx).Model(&deadLetterModel{}).Select("MIN(created_at)").Where("tenant_id = ? AND status = ?", tenantID, domain.DeadLetterPending)
	if applicationCode != "" {
		oldestQuery = oldestQuery.Where("application_code = ?", applicationCode)
	}
	if err := oldestQuery.Scan(&oldest).Error; err != nil {
		return domain.DeadLetterStatus{}, err
	}
	if !oldest.IsZero() {
		status.OldestPendingAt = &oldest
	}
	return status, nil
}

// ListIngestionReceipts returns batch delivery acknowledgements. It deliberately joins only
// application metadata; individual event payloads are not part of this operations endpoint.
func (r *Repository) ListIngestionReceipts(ctx context.Context, tenantID string, query application.IngestionReceiptPageRequest) (application.PageResult[domain.IngestionReceipt], error) {
	type row struct {
		ingestionReceiptModel
		ApplicationCode, ApplicationName, EnvironmentCode string
	}
	db := r.database.WithContext(ctx).Table("audit_ingestion_receipt").
		Joins("JOIN platform_application app ON app.id = audit_ingestion_receipt.application_id").
		Joins("JOIN platform_application_environment env ON env.id = audit_ingestion_receipt.environment_id").
		Where("audit_ingestion_receipt.tenant_id = ?", tenantID)
	if query.ApplicationCode != "" {
		db = db.Where("app.code = ?", query.ApplicationCode)
	}
	if query.EnvironmentCode != "" {
		db = db.Where("env.environment = ?", query.EnvironmentCode)
	}
	if query.Status != "" {
		db = db.Where("audit_ingestion_receipt.status = ?", query.Status)
	}
	if query.RequestID != "" {
		db = db.Where("audit_ingestion_receipt.request_id = ?", query.RequestID)
	}
	if query.CorrelationID != "" {
		db = db.Where("audit_ingestion_receipt.correlation_id = ?", query.CorrelationID)
	}
	if query.ReceivedFrom != nil {
		db = db.Where("audit_ingestion_receipt.received_at >= ?", query.ReceivedFrom.UTC())
	}
	if query.ReceivedTo != nil {
		db = db.Where("audit_ingestion_receipt.received_at <= ?", query.ReceivedTo.UTC())
	}
	var total int64
	if err := db.Count(&total).Error; err != nil {
		return application.PageResult[domain.IngestionReceipt]{}, err
	}
	var rows []row
	if err := db.Select("audit_ingestion_receipt.*, app.code AS application_code, app.name AS application_name, env.environment AS environment_code").Order("audit_ingestion_receipt.received_at DESC, audit_ingestion_receipt.id DESC").Offset((query.Page - 1) * query.PageSize).Limit(query.PageSize).Scan(&rows).Error; err != nil {
		return application.PageResult[domain.IngestionReceipt]{}, err
	}
	items := make([]domain.IngestionReceipt, 0, len(rows))
	for _, item := range rows {
		items = append(items, toIngestionReceipt(item.ingestionReceiptModel, item.ApplicationCode, item.ApplicationName, item.EnvironmentCode))
	}
	return application.PageResult[domain.IngestionReceipt]{Items: items, Page: query.Page, PageSize: query.PageSize, Total: total}, nil
}

func toIngestionReceipt(model ingestionReceiptModel, applicationCode, applicationName, environmentCode string) domain.IngestionReceipt {
	return domain.IngestionReceipt{ID: model.ID, TenantID: model.TenantID, ApplicationID: model.ApplicationID, ApplicationCode: applicationCode, ApplicationName: applicationName, EnvironmentID: model.EnvironmentID, EnvironmentCode: environmentCode, ClientID: model.ClientID, RequestID: model.RequestID, TraceID: model.TraceID, CorrelationID: model.CorrelationID, EventCount: model.EventCount, AcceptedCount: model.AcceptedCount, DuplicateCount: model.DuplicateCount, Status: model.Status, SourceIP: ipString(model.SourceIP), UserAgent: dereference(model.UserAgent), ReceivedAt: model.ReceivedAt, CreatedAt: model.CreatedAt}
}

func toRetentionTask(model retentionTaskModel) domain.RetentionTask {
	return domain.RetentionTask{TaskID: model.TaskID, TenantID: model.TenantID, ApplicationID: model.ApplicationID, RequestedBy: model.RequestedBy, Mode: model.Mode, Status: model.Status, ArchiveID: model.ArchiveID, CutoffAt: model.CutoffAt, CandidateCount: model.CandidateCount, ProcessedCount: model.ProcessedCount, FailureCode: dereference(model.FailureCode), FailureMessage: dereference(model.FailureMessage), CreatedAt: model.CreatedAt, StartedAt: dereferenceTime(model.StartedAt), CompletedAt: dereferenceTime(model.CompletedAt)}
}
func toArchive(model archiveModel) domain.Archive {
	return domain.Archive{ArchiveID: model.ArchiveID, TenantID: model.TenantID, ApplicationID: model.ApplicationID, StorageRelativePath: model.StorageRelativePath, MediaType: model.MediaType, SHA256: append([]byte(nil), model.SHA256...), EventCount: model.EventCount, OccurredFrom: model.OccurredFrom, OccurredTo: model.OccurredTo, CreatedAt: model.CreatedAt}
}
func toDeadLetter(model deadLetterModel) domain.DeadLetter {
	return domain.DeadLetter{DeadLetterID: model.DeadLetterID, TenantID: model.TenantID, ApplicationCode: model.ApplicationCode, EnvironmentCode: model.EnvironmentCode, EventID: model.EventID, Status: model.Status, Payload: append([]byte(nil), model.Payload...), LastErrorCode: append([]byte(nil), model.LastErrorCode...), LastErrorMessage: append([]byte(nil), model.LastErrorMessage...), Attempts: model.Attempts, CreatedAt: model.CreatedAt, UpdatedAt: model.UpdatedAt, ReplayedAt: dereferenceTime(model.ReplayedAt)}
}
func nullable(value string) any {
	if strings.TrimSpace(value) == "" {
		return nil
	}
	return strings.TrimSpace(value)
}
func dereference(value *string) string {
	if value == nil {
		return ""
	}
	return *value
}

func dereferenceTime(value *time.Time) time.Time {
	if value == nil {
		return time.Time{}
	}
	return *value
}

// MarshalDeadLetterForDiagnostics is retained for controlled operational tooling only.
func MarshalDeadLetterForDiagnostics(letter domain.DeadLetter) string {
	payload, _ := json.Marshal(map[string]any{"dead_letter_id": letter.DeadLetterID, "status": letter.Status, "event_id": letter.EventID})
	return fmt.Sprintf("%s", payload)
}
