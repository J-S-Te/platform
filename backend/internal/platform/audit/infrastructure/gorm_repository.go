// Package infrastructure persists append-only audit events through GORM without AutoMigrate.
package infrastructure

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"strings"
	"time"

	"github.com/J-S-Te/Basic-Platform/backend/internal/platform/audit/application"
	"github.com/J-S-Te/Basic-Platform/backend/internal/platform/audit/domain"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

type Repository struct{ database *gorm.DB }

func NewRepository(database *gorm.DB) (*Repository, error) {
	if database == nil {
		return nil, errors.New("audit database must not be nil")
	}
	return &Repository{database: database}, nil
}

type applicationModel struct{ ID, TenantID, Code, Name, Status string }

func (applicationModel) TableName() string { return "platform_application" }

type environmentModel struct{ ID, ApplicationID, Environment, Status string }

func (environmentModel) TableName() string { return "platform_application_environment" }

type eventModel struct {
	ID                                                                                         uint64 `gorm:"column:id"`
	OccurredMonth                                                                              uint   `gorm:"column:occurred_month"`
	EventID, TenantID, ApplicationID, EnvironmentID                                            string
	EventCategory, EventType                                                                   string
	OccurredAt, ReceivedAt                                                                     time.Time
	ActorType                                                                                  string
	ActorID, ActorNameSnapshot, SessionID, ClientID                                            *string
	Action, ResourceType                                                                       string
	ResourceID, ResourceNameSnapshot, BusinessID, RequestID, TraceID, CorrelationID, UserAgent *string
	SourceIP                                                                                   []byte
	Result                                                                                     string
	ReasonCode                                                                                 *string
	RiskLevel, Classification                                                                  string
	Summary                                                                                    *string
	Metadata, Changes                                                                          []byte
	PayloadHash                                                                                []byte
}

func (eventModel) TableName() string { return "audit_event" }

// eventQueryRow is the flattened scan target used by audit list, detail and export queries.
// Event must be an exported field: GORM ignores unexported embedded fields while parsing a
// destination schema, which would otherwise leave every audit_event column at its Go zero value.
type eventQueryRow struct {
	Event           eventModel `gorm:"embedded"`
	ApplicationCode string     `gorm:"column:application_code"`
	ApplicationName string     `gorm:"column:application_name"`
	EnvironmentCode string     `gorm:"column:environment_code"`
}

type dedupModel struct {
	ApplicationID, EventID string
	AuditRowID             *uint64
	OccurredMonth          uint
	ReceivedAt             time.Time
}

func (dedupModel) TableName() string { return "audit_event_dedup" }

// ingestionReceiptModel stores transport-level metadata for one batch Outbox delivery. It is
// intentionally separate from audit_event so the delivery correlation triplet cannot replace an
// individual event's original operation correlation fields.
type ingestionReceiptModel struct {
	ID                                               uint64 `gorm:"column:id"`
	TenantID, ApplicationID, EnvironmentID, ClientID string
	RequestID, TraceID, CorrelationID                string
	EventCount, AcceptedCount, DuplicateCount        uint
	Status                                           string
	SourceIP                                         []byte
	UserAgent                                        *string
	ReceivedAt, CreatedAt                            time.Time
}

func (ingestionReceiptModel) TableName() string { return "audit_ingestion_receipt" }

type asyncJobModel struct {
	ID, Priority                              uint64
	PublicID                                  *string
	TenantID                                  string
	ApplicationID, AggregateType, AggregateID *string
	JobType                                   string
	Payload                                   []byte
	Status                                    string
	AvailableAt                               time.Time
	LockedBy                                  *string
	LockedAt                                  *time.Time
	Attempts, MaxAttempts                     uint
	LastErrorCode, LastErrorMessage           *string
	ResultFileID                              *string
	CreatedAt                                 time.Time
	CompletedAt                               *time.Time
}

type fileObjectModel struct {
	ID, TenantID, ApplicationID, OriginalName, MediaType, Classification, Status string
	FileExtension                                                                *string
	OwnerUserID, CurrentVersionID                                                *string
	CreatedBy, UpdatedBy                                                         *string
	CurrentVersionNo                                                             uint
	CreatedAt, UpdatedAt                                                         time.Time
}

func (fileObjectModel) TableName() string { return "file_object" }

type fileVersionModel struct {
	ID, FileID, StorageRelativePath, MediaType, OriginalName, Status string
	VersionNo                                                        uint
	SizeBytes                                                        uint64
	SHA256                                                           []byte
	CreatedAt                                                        time.Time
}

func (fileVersionModel) TableName() string { return "file_version" }

type exportPayload struct {
	OperatorID string             `json:"operator_id"`
	Query      domain.ExportQuery `json:"query"`
}

func (asyncJobModel) TableName() string { return "async_job" }

func (r *Repository) Ingest(ctx context.Context, tenantID string, input application.EventInput, now time.Time) (domain.Receipt, error) {
	// 来源应用和环境必须在同一租户内保持启用；解析来源、写事件和写去重行放在同一事务，
	// 避免出现事件已保存但重试仍被当作首次提交的窗口。
	var receipt domain.Receipt
	err := r.database.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		app, env, err := resolveActiveSource(tx, tenantID, input.ApplicationCode, input.EnvironmentCode)
		if err != nil {
			return err
		}
		receipt, err = ingestEvent(tx, tenantID, app, env, input, now)
		return err
	})
	if err != nil {
		return domain.Receipt{}, r.mapError(err)
	}
	return receipt, nil
}

// 批量接收把新事件、重复事件判定及整批投递回执放在同一事务；数据库任一步失败会回滚
// 整批。重复 event_id 是可重试投递的正常完成结果，会单独计数而不是让整批失败。
func (r *Repository) IngestBatch(ctx context.Context, tenantID string, delivery application.BatchDeliveryInput, inputs []application.EventInput, now time.Time) ([]domain.Receipt, error) {
	receipts := make([]domain.Receipt, 0, len(inputs))
	err := r.database.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		app, env, err := resolveActiveSource(tx, tenantID, delivery.ApplicationCode, delivery.EnvironmentCode)
		if err != nil {
			return err
		}

		acceptedCount := uint(0)
		duplicateCount := uint(0)
		for _, input := range inputs {
			receipt, err := ingestEvent(tx, tenantID, app, env, input, now)
			if err != nil {
				return err
			}
			switch receipt.Status {
			case "ACCEPTED":
				acceptedCount++
			case "DUPLICATE":
				duplicateCount++
			default:
				return fmt.Errorf("unexpected audit batch receipt status %q", receipt.Status)
			}
			receipts = append(receipts, receipt)
		}

		receipt := newIngestionReceiptModel(tenantID, app, env, delivery, uint(len(inputs)), acceptedCount, duplicateCount, now)
		return tx.Create(&receipt).Error
	})
	if err != nil {
		return nil, r.mapError(err)
	}
	return receipts, nil
}

func resolveActiveSource(database *gorm.DB, tenantID, applicationCode, environmentCode string) (applicationModel, environmentModel, error) {
	// 来源由机器凭据对应租户和事件声明的应用/环境共同解析，只接受有效登记，防止审计
	// 发布者把事件归入其他租户或已经下线的环境。
	var app applicationModel
	if err := database.Where("tenant_id = ? AND code = ? AND status = ?", tenantID, applicationCode, "ACTIVE").Take(&app).Error; err != nil {
		return applicationModel{}, environmentModel{}, err
	}
	var env environmentModel
	if err := database.Where("application_id = ? AND environment = ? AND status = ?", app.ID, environmentCode, "ACTIVE").Take(&env).Error; err != nil {
		return applicationModel{}, environmentModel{}, err
	}
	return app, env, nil
}

func ingestEvent(tx *gorm.DB, tenantID string, app applicationModel, env environmentModel, input application.EventInput, now time.Time) (domain.Receipt, error) {
	// 去重键按应用隔离，同一应用重试相同 event_id 返回 DUPLICATE；事件行和去重行在同一
	// 事务写入，即使并发竞争唯一键，也不会留下只有一侧记录的不完整状态。
	var existing dedupModel
	err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where("application_id = ? AND event_id = ?", app.ID, input.EventID).Take(&existing).Error
	if err == nil {
		return domain.Receipt{EventID: input.EventID, Status: "DUPLICATE"}, nil
	}
	if !errors.Is(err, gorm.ErrRecordNotFound) {
		return domain.Receipt{}, err
	}

	// 可展示的元数据和字段变更先脱敏再持久化；payload_hash 则对原始规范输入计算，用于
	// 完整性取证。摘要不提供明文恢复能力，但其访问权限仍应按敏感审计数据控制。
	metadata, err := json.Marshal(redactMetadata(input.Metadata))
	if err != nil {
		return domain.Receipt{}, fmt.Errorf("marshal audit metadata: %w", err)
	}
	changes, err := json.Marshal(redactChanges(input.Changes))
	if err != nil {
		return domain.Receipt{}, fmt.Errorf("marshal audit changes: %w", err)
	}
	payload, err := json.Marshal(input)
	if err != nil {
		return domain.Receipt{}, fmt.Errorf("marshal audit payload: %w", err)
	}
	hash := sha256.Sum256(payload)
	event := eventModel{OccurredMonth: uint(input.OccurredAt.UTC().Year()*100 + int(input.OccurredAt.UTC().Month())), EventID: input.EventID, TenantID: tenantID, ApplicationID: app.ID, EnvironmentID: env.ID, EventCategory: input.EventCategory, EventType: input.EventType, OccurredAt: input.OccurredAt.UTC(), ReceivedAt: now.UTC(), ActorType: input.ActorType, ActorID: optional(input.ActorID), ActorNameSnapshot: optional(input.ActorName), SessionID: optional(input.SessionID), ClientID: optional(input.ClientID), Action: input.Action, ResourceType: input.ResourceType, ResourceID: optional(input.ResourceID), ResourceNameSnapshot: optional(input.ResourceName), BusinessID: optional(input.BusinessID), RequestID: optional(input.RequestID), TraceID: optionalTraceID(input.TraceID), CorrelationID: optional(input.CorrelationID), SourceIP: optionalIP(input.SourceIP), UserAgent: optional(input.UserAgent), Result: input.Result, ReasonCode: optional(input.ReasonCode), RiskLevel: input.RiskLevel, Classification: input.Classification, Summary: optional(input.Summary), Metadata: metadata, Changes: changes, PayloadHash: hash[:]}
	if err := tx.Create(&event).Error; err != nil {
		return domain.Receipt{}, err
	}
	rowID := event.ID
	if err := tx.Create(&dedupModel{ApplicationID: app.ID, EventID: input.EventID, AuditRowID: &rowID, OccurredMonth: event.OccurredMonth, ReceivedAt: now.UTC()}).Error; err != nil {
		return domain.Receipt{}, err
	}
	return domain.Receipt{EventID: input.EventID, Status: "ACCEPTED"}, nil
}

func newIngestionReceiptModel(tenantID string, app applicationModel, env environmentModel, delivery application.BatchDeliveryInput, eventCount, acceptedCount, duplicateCount uint, now time.Time) ingestionReceiptModel {
	// 投递级回执保留客户端、请求链路和计数证据，便于发布方确认批次已被完整消费；它不
	// 复制每条事件正文，事件详情仍以 audit_event 为权威来源。
	receivedAt := now.UTC()
	return ingestionReceiptModel{
		TenantID:       tenantID,
		ApplicationID:  app.ID,
		EnvironmentID:  env.ID,
		ClientID:       delivery.ClientID,
		RequestID:      delivery.RequestID,
		TraceID:        delivery.TraceID,
		CorrelationID:  delivery.CorrelationID,
		EventCount:     eventCount,
		AcceptedCount:  acceptedCount,
		DuplicateCount: duplicateCount,
		Status:         "COMPLETED",
		SourceIP:       optionalIP(delivery.SourceIP),
		UserAgent:      optional(delivery.UserAgent),
		ReceivedAt:     receivedAt,
		CreatedAt:      receivedAt,
	}
}

func (r *Repository) List(ctx context.Context, tenantID string, query application.PageRequest) (application.PageResult[domain.Event], error) {
	db := r.eventQuery(ctx, tenantID, query)
	var total int64
	if err := db.Count(&total).Error; err != nil {
		return application.PageResult[domain.Event]{}, err
	}
	var rows []eventQueryRow
	if err := db.Select("audit_event.*, app.code AS application_code, app.name AS application_name, env.environment AS environment_code").Joins("JOIN platform_application app ON app.id = audit_event.application_id").Joins("JOIN platform_application_environment env ON env.id = audit_event.environment_id").Order("audit_event.occurred_at DESC, audit_event.id DESC").Offset((query.Page - 1) * query.PageSize).Limit(query.PageSize).Scan(&rows).Error; err != nil {
		return application.PageResult[domain.Event]{}, err
	}
	items := make([]domain.Event, 0, len(rows))
	for _, row := range rows {
		items = append(items, toEvent(row.Event, row.ApplicationCode, row.ApplicationName, row.EnvironmentCode))
	}
	return application.PageResult[domain.Event]{Items: items, Page: query.Page, PageSize: query.PageSize, Total: total}, nil
}
func (r *Repository) Get(ctx context.Context, tenantID, eventID string) (domain.Event, error) {
	var record eventQueryRow
	err := r.database.WithContext(ctx).Table("audit_event").Select("audit_event.*, app.code AS application_code, app.name AS application_name, env.environment AS environment_code").Joins("JOIN platform_application app ON app.id = audit_event.application_id").Joins("JOIN platform_application_environment env ON env.id = audit_event.environment_id").Where("audit_event.tenant_id = ? AND audit_event.event_id = ?", tenantID, eventID).Order("audit_event.occurred_at DESC, audit_event.id DESC").Take(&record).Error
	if err != nil {
		return domain.Event{}, r.mapError(err)
	}
	return toEvent(record.Event, record.ApplicationCode, record.ApplicationName, record.EnvironmentCode), nil
}
func (r *Repository) CreateExportJob(ctx context.Context, tenantID, operatorID string, query application.PageRequest, publicID string, now time.Time) (domain.ExportJob, error) {
	// 导出条件和操作者固化进任务快照，Worker 不再读取浏览器请求上下文；任务归属平台
	// 应用仅用于文件元数据关联，所有后续读取仍必须携带租户 ID。
	var platformApplication applicationModel
	if err := r.database.WithContext(ctx).Where("tenant_id = ? AND code = ? AND status = ?", tenantID, "platform", "ACTIVE").Take(&platformApplication).Error; err != nil {
		return domain.ExportJob{}, r.mapError(err)
	}
	payload, err := json.Marshal(exportPayload{OperatorID: operatorID, Query: exportQuery(query)})
	if err != nil {
		return domain.ExportJob{}, fmt.Errorf("marshal audit export job payload: %w", err)
	}
	job := asyncJobModel{PublicID: &publicID, TenantID: tenantID, ApplicationID: &platformApplication.ID, JobType: "AUDIT_EXPORT", Payload: payload, Status: "PENDING", AvailableAt: now.UTC(), CreatedAt: now.UTC()}
	if err := r.database.WithContext(ctx).Create(&job).Error; err != nil {
		return domain.ExportJob{}, r.mapError(err)
	}
	return domain.ExportJob{JobID: publicID, Status: "PENDING", CreatedAt: now.UTC()}, nil
}
func (r *Repository) GetExportJob(ctx context.Context, tenantID, jobID string) (domain.ExportJob, error) {
	var job asyncJobModel
	if err := r.database.WithContext(ctx).Where("tenant_id = ? AND public_id = ? AND job_type = ?", tenantID, jobID, "AUDIT_EXPORT").Take(&job).Error; err != nil {
		return domain.ExportJob{}, r.mapError(err)
	}
	result := domain.ExportJob{JobID: jobID, Status: job.Status, CreatedAt: job.CreatedAt, ErrorCode: value(job.LastErrorCode), ErrorMessage: value(job.LastErrorMessage)}
	if job.CompletedAt != nil {
		result.CompletedAt = *job.CompletedAt
	}
	if job.Status == "SUCCEEDED" && job.ResultFileID != nil {
		result.DownloadURL = "/api/v1/audit/export-jobs/" + jobID + "/download"
	}
	return result, nil
}

// 领取时先回收超时租约，再用 SKIP LOCKED 让多个 Worker 并行争抢不同任务；事务只更新
// 状态和租约，文件生成放在事务外，避免慢磁盘长期占用 MySQL 行锁。
func (r *Repository) ClaimExportJob(ctx context.Context, workerID string, now, staleBefore time.Time) (domain.ExportWork, bool, error) {
	var result domain.ExportWork
	err := r.database.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := tx.Model(&asyncJobModel{}).Where("job_type = ? AND status = ? AND locked_at < ?", "AUDIT_EXPORT", "RUNNING", staleBefore).Updates(map[string]any{"status": "PENDING", "locked_by": nil, "locked_at": nil}).Error; err != nil {
			return err
		}
		var job asyncJobModel
		err := tx.Clauses(clause.Locking{Strength: "UPDATE", Options: "SKIP LOCKED"}).Where("job_type = ? AND status = ? AND available_at <= ?", "AUDIT_EXPORT", "PENDING", now.UTC()).Order("priority ASC, id ASC").Take(&job).Error
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil
		}
		if err != nil {
			return err
		}
		if job.PublicID == nil || job.ApplicationID == nil {
			return fmt.Errorf("audit export job %d is missing public or application ID", job.ID)
		}
		var payload exportPayload
		if err := json.Unmarshal(job.Payload, &payload); err != nil {
			return fmt.Errorf("decode audit export job %d payload: %w", job.ID, err)
		}
		if err := tx.Model(&asyncJobModel{}).Where("id = ? AND status = ?", job.ID, "PENDING").Updates(map[string]any{"status": "RUNNING", "locked_by": workerID, "locked_at": now.UTC(), "attempts": gorm.Expr("attempts + 1")}).Error; err != nil {
			return err
		}
		result = domain.ExportWork{JobID: *job.PublicID, TenantID: job.TenantID, ApplicationID: *job.ApplicationID, OperatorID: payload.OperatorID, Query: payload.Query, Attempts: job.Attempts + 1, MaxAttempts: job.MaxAttempts}
		return nil
	})
	if err != nil {
		return domain.ExportWork{}, false, err
	}
	return result, result.JobID != "", nil
}

const maxAuditExportEvents = 10000

func (r *Repository) ListExportEvents(ctx context.Context, tenantID string, query domain.ExportQuery) ([]domain.Event, error) {
	db := r.eventQuery(ctx, tenantID, pageRequest(query))
	var rows []eventQueryRow
	if err := db.Select("audit_event.*, app.code AS application_code, app.name AS application_name, env.environment AS environment_code").Joins("JOIN platform_application app ON app.id = audit_event.application_id").Joins("JOIN platform_application_environment env ON env.id = audit_event.environment_id").Order("audit_event.occurred_at DESC, audit_event.id DESC").Limit(maxAuditExportEvents + 1).Scan(&rows).Error; err != nil {
		return nil, err
	}
	if len(rows) > maxAuditExportEvents {
		return nil, fmt.Errorf("audit export contains more than %d events; narrow the filters before retrying", maxAuditExportEvents)
	}
	items := make([]domain.Event, 0, len(rows))
	for _, row := range rows {
		items = append(items, toEvent(row.Event, row.ApplicationCode, row.ApplicationName, row.EnvironmentCode))
	}
	return items, nil
}

func (r *Repository) CompleteExportJob(ctx context.Context, work domain.ExportWork, file domain.ExportFile, completedAt time.Time) error {
	// 文件对象、首个版本和任务成功状态在同一数据库事务发布；只有仍处于 RUNNING 的任务
	// 能完成，迟到 Worker 不能覆盖已经重试或终结的任务元数据。
	return r.database.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		object := fileObjectModel{ID: file.FileID, TenantID: work.TenantID, ApplicationID: work.ApplicationID, OriginalName: file.OriginalName, FileExtension: optional("csv"), MediaType: file.MediaType, Classification: "INTERNAL", OwnerUserID: optional(work.OperatorID), CurrentVersionNo: 1, CurrentVersionID: optional(file.VersionID), Status: "ACTIVE", CreatedAt: completedAt.UTC(), UpdatedAt: completedAt.UTC(), CreatedBy: optional(work.OperatorID), UpdatedBy: optional(work.OperatorID)}
		if err := tx.Create(&object).Error; err != nil {
			return err
		}
		version := fileVersionModel{ID: file.VersionID, FileID: file.FileID, VersionNo: 1, StorageRelativePath: file.StorageRelativePath, SizeBytes: file.SizeBytes, SHA256: file.SHA256, MediaType: file.MediaType, OriginalName: file.OriginalName, Status: "ACTIVE", CreatedAt: completedAt.UTC()}
		if err := tx.Create(&version).Error; err != nil {
			return err
		}
		updates := map[string]any{"status": "SUCCEEDED", "result_file_id": file.FileID, "completed_at": completedAt.UTC(), "locked_by": nil, "locked_at": nil, "last_error_code": nil, "last_error_message": nil}
		result := tx.Model(&asyncJobModel{}).Where("tenant_id = ? AND public_id = ? AND job_type = ? AND status = ?", work.TenantID, work.JobID, "AUDIT_EXPORT", "RUNNING").Updates(updates)
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected != 1 {
			return application.ErrConflict
		}
		return nil
	})
}

func (r *Repository) FailExportJob(ctx context.Context, work domain.ExportWork, code, message string, retryAt, now time.Time) error {
	// 达到最大尝试次数后终结任务，否则清除租约并按退避时间重新排队；限定 RUNNING 状态
	// 可避免迟到失败回写把后来已成功的任务重新置为待处理。
	updates := map[string]any{"last_error_code": optional(code), "last_error_message": optional(message), "locked_by": nil, "locked_at": nil}
	if work.Attempts >= work.MaxAttempts {
		updates["status"] = "FAILED"
		updates["completed_at"] = now.UTC()
	} else {
		updates["status"] = "PENDING"
		updates["available_at"] = retryAt.UTC()
	}
	return r.database.WithContext(ctx).Model(&asyncJobModel{}).Where("tenant_id = ? AND public_id = ? AND status = ?", work.TenantID, work.JobID, "RUNNING").Updates(updates).Error
}

func (r *Repository) GetExportFile(ctx context.Context, tenantID, jobID string) (domain.ExportFile, error) {
	type row struct {
		FileID, StorageRelativePath, OriginalName, MediaType string
		CreatedAt                                            time.Time
	}
	var result row
	err := r.database.WithContext(ctx).Table("async_job").Select("file_object.id AS file_id, file_version.storage_relative_path, file_version.original_name, file_version.media_type, file_version.created_at").Joins("JOIN file_object ON file_object.id = async_job.result_file_id").Joins("JOIN file_version ON file_version.id = file_object.current_version_id").Where("async_job.tenant_id = ? AND async_job.public_id = ? AND async_job.job_type = ? AND async_job.status = ?", tenantID, jobID, "AUDIT_EXPORT", "SUCCEEDED").Take(&result).Error
	if err != nil {
		return domain.ExportFile{}, r.mapError(err)
	}
	return domain.ExportFile{FileID: result.FileID, StorageRelativePath: result.StorageRelativePath, OriginalName: result.OriginalName, MediaType: result.MediaType, CreatedAt: result.CreatedAt}, nil
}

func exportQuery(query application.PageRequest) domain.ExportQuery {
	return domain.ExportQuery{Keyword: query.Keyword, ApplicationCode: query.ApplicationCode, EnvironmentCode: query.EnvironmentCode, Action: query.Action, ActionCategory: query.ActionCategory, Result: query.Result, RiskLevel: query.RiskLevel, OccurredFrom: query.OccurredFrom, OccurredTo: query.OccurredTo}
}
func pageRequest(query domain.ExportQuery) application.PageRequest {
	return application.PageRequest{Keyword: query.Keyword, ApplicationCode: query.ApplicationCode, EnvironmentCode: query.EnvironmentCode, Action: query.Action, ActionCategory: query.ActionCategory, Result: query.Result, RiskLevel: query.RiskLevel, OccurredFrom: query.OccurredFrom, OccurredTo: query.OccurredTo, Page: 1, PageSize: 100}
}

func (r *Repository) eventQuery(ctx context.Context, tenantID string, query application.PageRequest) *gorm.DB {
	db := r.database.WithContext(ctx).Table("audit_event").Where("audit_event.tenant_id = ?", tenantID)
	if query.Keyword != "" {
		// Search every administrator-visible correlation dimension promised by the console. ASCII
		// columns are converted before comparison so Chinese input cannot trigger MySQL collation errors.
		pattern := "%" + query.Keyword + "%"
		db = db.Where(`(
			CONVERT(audit_event.action USING utf8mb4) LIKE ? OR
			audit_event.actor_name_snapshot LIKE ? OR
			audit_event.resource_name_snapshot LIKE ? OR
			audit_event.summary LIKE ? OR
			CONVERT(COALESCE(audit_event.request_id, '') USING utf8mb4) LIKE ? OR
			CONVERT(COALESCE(audit_event.trace_id, '') USING utf8mb4) LIKE ? OR
			CONVERT(COALESCE(audit_event.correlation_id, '') USING utf8mb4) LIKE ? OR
			COALESCE(JSON_UNQUOTE(JSON_EXTRACT(audit_event.metadata, '$.path')), '') LIKE ?
		)`, pattern, pattern, pattern, pattern, pattern, pattern, pattern, pattern)
	}
	if query.ApplicationCode != "" {
		db = db.Joins("JOIN platform_application filter_app ON filter_app.id = audit_event.application_id").Where("filter_app.code = ?", query.ApplicationCode)
	}
	if query.EnvironmentCode != "" {
		db = db.Joins("JOIN platform_application_environment filter_env ON filter_env.id = audit_event.environment_id").Where("filter_env.environment = ?", query.EnvironmentCode)
	}
	if query.Action != "" {
		// 显式转换也保护直接调用接口的客户端，非 ASCII action 只会返回空结果而不会导致 500。
		db = db.Where("CONVERT(audit_event.action USING utf8mb4) = ?", query.Action)
	}
	if query.ActionCategory != "" {
		clause, arguments := actionCategoryPredicate(query.ActionCategory)
		db = db.Where(clause, arguments...)
	}
	if query.Result != "" {
		db = db.Where("audit_event.result = ?", query.Result)
	}
	if query.RiskLevel != "" {
		db = db.Where("audit_event.risk_level = ?", query.RiskLevel)
	}
	if query.OccurredFrom != nil {
		db = db.Where("audit_event.occurred_at >= ?", query.OccurredFrom.UTC())
	}
	if query.OccurredTo != nil {
		db = db.Where("audit_event.occurred_at <= ?", query.OccurredTo.UTC())
	}
	return db
}

// actionCategoryPredicate converts the UI's stable operation category into predicates over the
// machine-readable action and HTTP metadata. Values are never interpolated into SQL.
func actionCategoryPredicate(category string) (string, []any) {
	const (
		action = "LOWER(CONVERT(audit_event.action USING utf8mb4))"
		method = "UPPER(COALESCE(JSON_UNQUOTE(JSON_EXTRACT(audit_event.metadata, '$.method')), ''))"
		path   = "LOWER(COALESCE(JSON_UNQUOTE(JSON_EXTRACT(audit_event.metadata, '$.path')), ''))"
	)
	const statusPattern = "(^|[.:/ _-])(enable|disable|activate|deactivate|lock|unlock|publish|resolve|replay|retry|cancel|rerun|read|read-all)([.:/ _-]|$)"

	switch strings.ToUpper(strings.TrimSpace(category)) {
	case "LOGIN":
		// 登录分类覆盖认证会话的建立与退出，避免 POST /auth/logout 被误判为新增操作。
		return "(" + action + " = ? OR " + action + " LIKE ? OR " + action + " = ? OR " + action + " LIKE ?)", []any{"auth.login", "auth.login.%", "auth.logout", "auth.logout.%"}
	case "EXPORT":
		return "(" + action + " LIKE ? OR " + path + " LIKE ?)", []any{"%export%", "%export%"}
	case "DELETE":
		return "(" + method + " = ? OR " + action + " REGEXP ?)", []any{"DELETE", "(^|[.:/ _-])(delete|remove|revoke)([.:/ _-]|$)"}
	case "AUTHORIZATION_CHANGE":
		return "(" + action + " LIKE ? OR " + path + " LIKE ? OR " + path + " LIKE ?)", []any{"authorization.%", "%role-binding%", "%/access%"}
	case "SECRET_ROTATION":
		return "(" + action + " REGEXP ? OR " + path + " REGEXP ?)", []any{"(^|[.:/ _-])(secret|credential).*(rotate|revoke)", "(credential|secret).*(rotate|revoke)"}
	case "PASSWORD_RESET":
		return "(" + action + " LIKE ? OR " + path + " LIKE ?)", []any{"%password%reset%", "%password%reset%"}
	case "CATALOG_SYNC":
		return "(" + action + " LIKE ? OR " + path + " LIKE ?)", []any{"%catalog%sync%", "%authorization-catalog%"}
	case "AUDIT_ACCESS":
		return "(" + path + " LIKE ?)", []any{"%/audit/%"}
	case "IMPORT":
		return "(" + action + " LIKE ? OR " + path + " LIKE ? OR " + path + " LIKE ?)", []any{"%import%", "%import%", "%/batch%"}
	case "STATUS_CHANGE":
		return "(" + action + " REGEXP ? OR " + path + " REGEXP ?)", []any{statusPattern, statusPattern}
	case "UPDATE":
		return "((" + method + " IN (?, ?) OR " + action + " REGEXP ?) AND NOT (" + action + " LIKE ? OR " + path + " LIKE ?) AND NOT (" + action + " REGEXP ? OR " + path + " REGEXP ?))", []any{"PUT", "PATCH", "(^|[.:/ _-])update([.:/ _-]|$)", "%export%", "%export%", statusPattern, statusPattern}
	case "CREATE":
		return "((" + method + " = ? OR " + action + " REGEXP ?) AND NOT (" + action + " = ? OR " + action + " LIKE ? OR " + action + " = ? OR " + action + " LIKE ?) AND NOT (" + action + " LIKE ? OR " + path + " LIKE ?) AND NOT (" + action + " REGEXP ? OR " + path + " REGEXP ?))", []any{"POST", "(^|[.:/ _-])create([.:/ _-]|$)", "auth.login", "auth.login.%", "auth.logout", "auth.logout.%", "%export%", "%export%", statusPattern, statusPattern}
	default:
		// 应用层会拒绝未知分类；该分支用于防御绕过应用层的仓储调用。
		return "1 = 0", nil
	}
}

func (r *Repository) mapError(err error) error {
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return application.ErrNotFound
	}
	return err
}
func toEvent(model eventModel, appCode, appName, environmentCode string) domain.Event {
	var changes []domain.FieldChange
	_ = json.Unmarshal(model.Changes, &changes)
	return domain.Event{ID: fmt.Sprint(model.ID), EventID: model.EventID, TenantID: model.TenantID, ApplicationID: model.ApplicationID, ApplicationCode: appCode, ApplicationName: appName, EnvironmentID: model.EnvironmentID, EnvironmentCode: environmentCode, OccurredAt: model.OccurredAt, OperatorDisplayName: value(model.ActorNameSnapshot), ActionType: model.EventType, Action: model.Action, Result: model.Result, ResourceType: model.ResourceType, ResourceID: value(model.ResourceID), ResourceName: value(model.ResourceNameSnapshot), Method: valueFromMetadata(model.Metadata, "method"), Path: valueFromMetadata(model.Metadata, "path"), ClientIP: ipString(model.SourceIP), UserAgent: value(model.UserAgent), RequestID: value(model.RequestID), TraceID: value(model.TraceID), CorrelationID: value(model.CorrelationID), StatusCode: intFromMetadata(model.Metadata, "status_code"), RiskLevel: model.RiskLevel, Detail: value(model.ReasonCode), Summary: value(model.Summary), ChangeSummary: changes}
}
func optional(value string) *string {
	value = strings.TrimSpace(value)
	if value == "" {
		return nil
	}
	return &value
}
func optionalTraceID(value string) *string {
	value = strings.TrimSpace(value)
	if len(value) != 32 {
		return nil
	}
	return &value
}

// optionalIP converts a textual client address to the binary form required by audit_event.source_ip.
func optionalIP(value string) []byte {
	parsed := net.ParseIP(strings.TrimSpace(value))
	if parsed == nil {
		return nil
	}
	if ipv4 := parsed.To4(); ipv4 != nil {
		return append([]byte(nil), ipv4...)
	}
	if ipv6 := parsed.To16(); ipv6 != nil {
		return append([]byte(nil), ipv6...)
	}
	return nil
}

func ipString(raw []byte) string {
	if len(raw) == 0 {
		return ""
	}
	return net.IP(raw).String()
}
func value(pointer *string) string {
	if pointer == nil {
		return ""
	}
	return *pointer
}
func valueFromMetadata(raw []byte, key string) string {
	var metadata map[string]any
	_ = json.Unmarshal(raw, &metadata)
	value, _ := metadata[key].(string)
	return value
}
func intFromMetadata(raw []byte, key string) int {
	var metadata map[string]any
	_ = json.Unmarshal(raw, &metadata)
	number, ok := metadata[key].(float64)
	if !ok {
		return 0
	}
	return int(number)
}
func redactChanges(changes []domain.FieldChange) []domain.FieldChange {
	redacted := make([]domain.FieldChange, 0, len(changes))
	for _, change := range changes {
		if sensitiveField(change.Field) {
			change.Before = "***"
			change.After = "***"
		}
		redacted = append(redacted, change)
	}
	return redacted
}

// redactMetadata removes credential material recursively before metadata reaches append-only storage.
func redactMetadata(metadata map[string]any) map[string]any {
	if len(metadata) == 0 {
		return nil
	}
	redacted := make(map[string]any, len(metadata))
	for key, value := range metadata {
		if sensitiveField(key) {
			redacted[key] = "***"
			continue
		}
		redacted[key] = redactMetadataValue(value)
	}
	return redacted
}

func redactMetadataValue(value any) any {
	switch typed := value.(type) {
	case map[string]any:
		return redactMetadata(typed)
	case []any:
		redacted := make([]any, len(typed))
		for index, item := range typed {
			redacted[index] = redactMetadataValue(item)
		}
		return redacted
	default:
		return value
	}
}
func sensitiveField(field string) bool {
	field = strings.ToLower(field)
	return strings.Contains(field, "password") || strings.Contains(field, "token") || strings.Contains(field, "secret") || strings.Contains(field, "cookie")
}
