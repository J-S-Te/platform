package infrastructure

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/J-S-Te/Basic-Platform/internal/platform/audit/application"
	"github.com/J-S-Te/Basic-Platform/internal/platform/audit/domain"
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

// CreateRetentionTask 只插入任务记录；worker 执行由行锁协调。
func (r *Repository) CreateRetentionTask(ctx context.Context, task domain.RetentionTask) (domain.RetentionTask, error) {
	model := retentionTaskModel{TaskID: task.TaskID, TenantID: task.TenantID, ApplicationID: task.ApplicationID, RequestedBy: task.RequestedBy, Mode: task.Mode, Status: task.Status, ArchiveID: task.ArchiveID, CutoffAt: task.CutoffAt, CreatedAt: task.CreatedAt}
	if err := r.database.WithContext(ctx).Create(&model).Error; err != nil {
		return domain.RetentionTask{}, err
	}
	return task, nil
}

// ClaimRetentionTask 以 SKIP LOCKED 认领一个 PENDING 或租约过期的 RUNNING 任务并写入执行者。
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

// CountRetentionEvents 在写入一个归档文件前核对精确且不可变的来源集合。
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

// SetRetentionTaskCandidateCount 在任务运行期间记录已核实的来源集合大小。
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

// ListRetentionEvents 按任务截止时间读取截点前的在线审计事件，顺序稳定以保证归档可复现。
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
		events = append(events, toEvent(row.Event, row.ApplicationCode, row.ApplicationName, row.EnvironmentCode))
	}
	return events, nil
}

// retentionEventRow 显式命名嵌入字段并使用 gorm:"embedded"。
//
// 匿名嵌入未导出类型时 GORM/database/sql 无法写入该类型的字段（reflect 不允许设置
// 通过未导出嵌入字段提升出来的字段），归档出来的事件会只剩应用与环境元数据，
// 事件本身的字段全部为空。
type retentionEventRow struct {
	Event                                             eventModel `gorm:"embedded"`
	ApplicationCode, ApplicationName, EnvironmentCode string
}

// CreateArchive 在单事务写入归档元数据与事件清单，清单是后续 PURGE 的唯一依据。
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

// ArchiveByID 返回租户内归档元数据；PURGE 前必须先取得该清单。
func (r *Repository) ArchiveByID(ctx context.Context, tenantID, archiveID string) (domain.Archive, error) {
	var model archiveModel
	if err := r.database.WithContext(ctx).Where("tenant_id = ? AND archive_id = ?", tenantID, archiveID).Take(&model).Error; err != nil {
		return domain.Archive{}, r.mapError(err)
	}
	return toArchive(model), nil
}

// PurgeArchivedEvents 有意只能经 RetentionService 调用。仅当该租户所有的归档存在
// audit_archive_item 记录时才删除事件，并返回被标记为已处理的归档清单条目数
// （而不是单纯的物理删除行数）。
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

// CompleteRetentionTask 把 RUNNING 任务置为完成；回写绑定租约持有者，迟到的旧执行者被拒绝。
func (r *Repository) CompleteRetentionTask(ctx context.Context, task domain.RetentionTask, processed uint64, completedAt time.Time) error {
	updates := map[string]any{"status": domain.RetentionTaskCompleted, "processed_count": processed, "completed_at": completedAt, "failure_code": nil, "failure_message": nil}
	if task.Mode == domain.RetentionTaskArchive && strings.TrimSpace(task.ArchiveID) != "" {
		updates["archive_id"] = task.ArchiveID
	}
	// 与租约持有者绑定：租约过期被接管后，旧执行者的迟到完结不能覆盖新执行者。
	result := r.database.WithContext(ctx).Model(&retentionTaskModel{}).Where("task_id = ? AND status = ? AND worker_id = ?", task.TaskID, domain.RetentionTaskRunning, task.WorkerID).Updates(updates)
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected != 1 {
		return application.ErrConflict
	}
	return nil
}

// FailRetentionTask 记录失败码与安全消息并把任务置为 FAILED；同样绑定租约持有者。
func (r *Repository) FailRetentionTask(ctx context.Context, task domain.RetentionTask, code, message string, now time.Time) error {
	result := r.database.WithContext(ctx).Model(&retentionTaskModel{}).Where("task_id = ? AND status = ? AND worker_id = ?", task.TaskID, domain.RetentionTaskRunning, task.WorkerID).Updates(map[string]any{"status": domain.RetentionTaskFailed, "failure_code": nullable(code), "failure_message": nullable(message), "completed_at": now})
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected != 1 {
		return application.ErrConflict
	}
	return nil
}

// ListRetentionTasks 向租户隔离的运营控制台暴露任务历史。
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

// CreateDeadLetter 持久化脱敏后的死信载荷，初始状态为 PENDING。
func (r *Repository) CreateDeadLetter(ctx context.Context, letter domain.DeadLetter) (domain.DeadLetter, error) {
	model := deadLetterModel{DeadLetterID: letter.DeadLetterID, TenantID: letter.TenantID, ApplicationCode: letter.ApplicationCode, EnvironmentCode: letter.EnvironmentCode, EventID: letter.EventID, Status: letter.Status, Payload: append([]byte(nil), letter.Payload...), LastErrorCode: []byte(letter.LastErrorCode), LastErrorMessage: []byte(letter.LastErrorMessage), Attempts: letter.Attempts, CreatedAt: letter.CreatedAt, UpdatedAt: letter.UpdatedAt}
	if err := r.database.WithContext(ctx).Create(&model).Error; err != nil {
		return domain.DeadLetter{}, err
	}
	return letter, nil
}

// GetDeadLetter 返回租户内单条死信（含脱敏载荷），供受控重放使用。
func (r *Repository) GetDeadLetter(ctx context.Context, tenantID, deadLetterID string) (domain.DeadLetter, error) {
	var model deadLetterModel
	if err := r.database.WithContext(ctx).Where("tenant_id = ? AND dead_letter_id = ?", tenantID, deadLetterID).Take(&model).Error; err != nil {
		return domain.DeadLetter{}, r.mapError(err)
	}
	return toDeadLetter(model), nil
}

// ListDeadLetters 分页返回租户内死信，可按应用与状态过滤。
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

// MarkDeadLetterReplayed 只允许把 PENDING 死信置为 REPLAYED 终态并递增尝试次数。
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

// MarkDeadLetterReplayFailed 在记录最新安全失败摘要并原子递增尝试次数的同时，
// 保持该待处理消息仍可被后续受控重放领取。
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

// DeadLetterStatus 按状态统计租户（可含应用过滤）的死信数量。
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

// ingestionReceiptRow 与 retentionEventRow 一样显式命名嵌入字段：匿名嵌入未导出模型时
// database/sql 无法写回提升出来的字段，接收回执会只剩应用与环境元数据。
type ingestionReceiptRow struct {
	Receipt                                           ingestionReceiptModel `gorm:"embedded"`
	ApplicationCode, ApplicationName, EnvironmentCode string
}

// ListIngestionReceipts 返回批量投递确认。有意只关联应用元数据；单条事件 payload
// 不属于这个运营端点的返回范围。
func (r *Repository) ListIngestionReceipts(ctx context.Context, tenantID string, query application.IngestionReceiptPageRequest) (application.PageResult[domain.IngestionReceipt], error) {
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
	var rows []ingestionReceiptRow
	if err := db.Select("audit_ingestion_receipt.*, app.code AS application_code, app.name AS application_name, env.environment AS environment_code").Order("audit_ingestion_receipt.received_at DESC, audit_ingestion_receipt.id DESC").Offset((query.Page - 1) * query.PageSize).Limit(query.PageSize).Scan(&rows).Error; err != nil {
		return application.PageResult[domain.IngestionReceipt]{}, err
	}
	items := make([]domain.IngestionReceipt, 0, len(rows))
	for _, item := range rows {
		items = append(items, toIngestionReceipt(item.Receipt, item.ApplicationCode, item.ApplicationName, item.EnvironmentCode))
	}
	return application.PageResult[domain.IngestionReceipt]{Items: items, Page: query.Page, PageSize: query.PageSize, Total: total}, nil
}

func toIngestionReceipt(model ingestionReceiptModel, applicationCode, applicationName, environmentCode string) domain.IngestionReceipt {
	return domain.IngestionReceipt{ID: model.ID, TenantID: model.TenantID, ApplicationID: model.ApplicationID, ApplicationCode: applicationCode, ApplicationName: applicationName, EnvironmentID: model.EnvironmentID, EnvironmentCode: environmentCode, ClientID: model.ClientID, RequestID: model.RequestID, TraceID: model.TraceID, CorrelationID: model.CorrelationID, EventCount: model.EventCount, AcceptedCount: model.AcceptedCount, DuplicateCount: model.DuplicateCount, Status: model.Status, SourceIP: ipString(model.SourceIP), UserAgent: dereference(model.UserAgent), ReceivedAt: model.ReceivedAt, CreatedAt: model.CreatedAt}
}

func toRetentionTask(model retentionTaskModel) domain.RetentionTask {
	return domain.RetentionTask{TaskID: model.TaskID, TenantID: model.TenantID, ApplicationID: model.ApplicationID, RequestedBy: model.RequestedBy, Mode: model.Mode, Status: model.Status, ArchiveID: model.ArchiveID, CutoffAt: model.CutoffAt, CandidateCount: model.CandidateCount, ProcessedCount: model.ProcessedCount, FailureCode: dereference(model.FailureCode), FailureMessage: dereference(model.FailureMessage), CreatedAt: model.CreatedAt, StartedAt: dereferenceTime(model.StartedAt), CompletedAt: dereferenceTime(model.CompletedAt), WorkerID: model.WorkerID}
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

// MarshalDeadLetterForDiagnostics 仅供受控运维工具保留使用。
func MarshalDeadLetterForDiagnostics(letter domain.DeadLetter) string {
	payload, _ := json.Marshal(map[string]any{"dead_letter_id": letter.DeadLetterID, "status": letter.Status, "event_id": letter.EventID})
	return fmt.Sprintf("%s", payload)
}
