package infrastructure

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/J-S-Te/Basic-Platform/internal/platform/filetask/application"
	"github.com/J-S-Te/Basic-Platform/internal/platform/filetask/domain"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// Repository 映射既有 file_object、file_version、file_binding 与 async_job 表。
// 运行时不调用 AutoMigrate，表结构和状态约束只由编号 SQL 迁移维护。
type Repository struct{ database *gorm.DB }

func NewGORMRepository(database *gorm.DB) (*Repository, error) {
	if database == nil {
		return nil, errors.New("filetask GORM database must not be nil")
	}
	return &Repository{database: database}, nil
}

type fileObjectModel struct {
	ID, TenantID, ApplicationID, OriginalName, MediaType, Classification, Status string
	FileExtension                                                                *string
	OwnerUserID, OwnerOrgID, CurrentVersionID                                    *string
	CurrentVersionNo                                                             uint
	Version                                                                      uint64
	CreatedAt, UpdatedAt                                                         time.Time
	CreatedBy, UpdatedBy                                                         *string
}

func (fileObjectModel) TableName() string { return "file_object" }

type fileVersionModel struct {
	ID, FileID, StorageRelativePath, MediaType, OriginalName, Status string
	VersionNo                                                        uint
	SizeBytes                                                        uint64
	SHA256                                                           []byte
	UploaderUserID, UploadRequestID                                  *string
	CreatedAt                                                        time.Time
}

func (fileVersionModel) TableName() string { return "file_version" }

type fileBindingModel struct {
	ID, FileID, Status string
}

func (fileBindingModel) TableName() string { return "file_binding" }

type asyncJobModel struct {
	ID                                            uint64
	PublicID                                      *string
	TenantID                                      string
	ApplicationID, AggregateType, AggregateID     *string
	JobType                                       string
	Payload                                       []byte
	Status                                        string
	Priority                                      int
	AvailableAt                                   time.Time
	LockedBy                                      *string
	LockedAt                                      *time.Time
	Attempts, MaxAttempts                         uint
	LastErrorCode, LastErrorMessage, ResultFileID *string
	CreatedAt                                     time.Time
	CompletedAt                                   *time.Time
}

func (asyncJobModel) TableName() string { return "async_job" }

func (repository *Repository) CreateWriting(ctx context.Context, file domain.File, version domain.FileVersion) error {
	return repository.database.WithContext(ctx).Transaction(func(transaction *gorm.DB) error {
		fileModel := fileObjectModel{ID: file.ID, TenantID: file.TenantID, ApplicationID: file.ApplicationID, OriginalName: file.OriginalName, FileExtension: optional(file.FileExtension), MediaType: file.MediaType, Classification: file.Classification, OwnerUserID: optional(file.OwnerUserID), CurrentVersionNo: file.CurrentVersionNo, CurrentVersionID: optional(file.CurrentVersionID), Status: file.Status, Version: file.Version, CreatedAt: file.CreatedAt.UTC(), UpdatedAt: file.UpdatedAt.UTC(), CreatedBy: optional(file.OwnerUserID), UpdatedBy: optional(file.OwnerUserID)}
		if err := transaction.Create(&fileModel).Error; err != nil {
			return mapError(err)
		}
		versionModel := fileVersionModel{ID: version.ID, FileID: version.FileID, VersionNo: version.VersionNo, StorageRelativePath: version.StorageRelativePath, MediaType: version.MediaType, OriginalName: version.OriginalName, UploaderUserID: optional(version.UploaderUserID), UploadRequestID: optional(version.UploadRequestID), Status: version.Status, CreatedAt: version.CreatedAt.UTC()}
		if err := transaction.Create(&versionModel).Error; err != nil {
			return mapError(err)
		}
		return nil
	})
}

func (repository *Repository) MarkAvailable(ctx context.Context, tenantID, fileID string, size uint64, digest []byte, updatedAt time.Time) error {
	// 文件和当前版本必须在同一事务中同时从“写入中”推进为“可用”，避免下载查询看见半完成状态。
	return repository.database.WithContext(ctx).Transaction(func(transaction *gorm.DB) error {
		var object fileObjectModel
		if err := transaction.Clauses(clause.Locking{Strength: "UPDATE"}).Where("tenant_id = ? AND id = ? AND status = ?", tenantID, fileID, domain.FileStatusUploading).Take(&object).Error; err != nil {
			return mapError(err)
		}
		if object.CurrentVersionID == nil {
			return application.ErrConflict
		}
		updateVersion := transaction.Model(&fileVersionModel{}).Where("id = ? AND file_id = ? AND status = ?", *object.CurrentVersionID, fileID, domain.FileVersionStatusWriting).Updates(map[string]any{"size_bytes": size, "sha256": digest, "status": domain.FileVersionStatusReady})
		if updateVersion.Error != nil {
			return mapError(updateVersion.Error)
		}
		if updateVersion.RowsAffected != 1 {
			return application.ErrConflict
		}
		updateObject := transaction.Model(&fileObjectModel{}).Where("tenant_id = ? AND id = ? AND status = ?", tenantID, fileID, domain.FileStatusUploading).Updates(map[string]any{"status": domain.FileStatusAvailable, "updated_at": updatedAt.UTC(), "version": gorm.Expr("version + 1")})
		if updateObject.Error != nil {
			return mapError(updateObject.Error)
		}
		if updateObject.RowsAffected != 1 {
			return application.ErrConflict
		}
		return nil
	})
}

func (repository *Repository) MarkFailed(ctx context.Context, tenantID, fileID string, updatedAt time.Time) error {
	return repository.database.WithContext(ctx).Transaction(func(transaction *gorm.DB) error {
		var object fileObjectModel
		if err := transaction.Clauses(clause.Locking{Strength: "UPDATE"}).Where("tenant_id = ? AND id = ?", tenantID, fileID).Take(&object).Error; err != nil {
			return mapError(err)
		}
		if object.CurrentVersionID != nil {
			if err := transaction.Model(&fileVersionModel{}).Where("id = ? AND file_id = ?", *object.CurrentVersionID, fileID).Updates(map[string]any{"status": domain.FileVersionStatusFailed}).Error; err != nil {
				return mapError(err)
			}
		}
		return mapError(transaction.Model(&fileObjectModel{}).Where("tenant_id = ? AND id = ?", tenantID, fileID).Updates(map[string]any{"status": domain.FileStatusFailed, "updated_at": updatedAt.UTC(), "version": gorm.Expr("version + 1")}).Error)
	})
}

func (repository *Repository) GetAvailable(ctx context.Context, tenantID, fileID string) (domain.StoredFile, error) {
	var object fileObjectModel
	if err := repository.database.WithContext(ctx).Where("tenant_id = ? AND id = ? AND status IN ?", tenantID, fileID, []string{domain.FileStatusAvailable, "ACTIVE"}).Take(&object).Error; err != nil {
		return domain.StoredFile{}, mapError(err)
	}
	if object.CurrentVersionID == nil {
		return domain.StoredFile{}, application.ErrNotFound
	}
	var version fileVersionModel
	if err := repository.database.WithContext(ctx).Where("id = ? AND file_id = ? AND status IN ?", *object.CurrentVersionID, fileID, []string{domain.FileVersionStatusReady, "ACTIVE"}).Take(&version).Error; err != nil {
		return domain.StoredFile{}, mapError(err)
	}
	return domain.StoredFile{File: toFile(object), Version: toVersion(version)}, nil
}

// ClaimExpiredUnbound 用 FOR UPDATE SKIP LOCKED 领取一条旧且无绑定的文件，再推进到 DELETING。
// 慢速磁盘删除在短事务提交后执行，因此多个清理实例不会互相等待，也不会同时删除同一文件。
func (repository *Repository) ClaimExpiredUnbound(ctx context.Context, tenantID string, cutoff time.Time) (domain.StoredFile, bool, error) {
	var result domain.StoredFile
	err := repository.database.WithContext(ctx).Transaction(func(transaction *gorm.DB) error {
		var object fileObjectModel
		err := transaction.Clauses(clause.Locking{Strength: "UPDATE", Options: "SKIP LOCKED"}).Where("tenant_id = ? AND status IN ? AND created_at < ? AND current_version_id IS NOT NULL", tenantID, []string{domain.FileStatusAvailable, "ACTIVE"}, cutoff.UTC()).Where("NOT EXISTS (SELECT 1 FROM file_binding binding WHERE binding.file_id = file_object.id AND binding.status = ?)", "ACTIVE").Order("created_at ASC, id ASC").Take(&object).Error
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil
		}
		if err != nil {
			return err
		}
		var version fileVersionModel
		if err := transaction.Where("id = ? AND file_id = ? AND status IN ?", *object.CurrentVersionID, object.ID, []string{domain.FileVersionStatusReady, "ACTIVE"}).Take(&version).Error; err != nil {
			return err
		}
		update := transaction.Model(&fileObjectModel{}).Where("id = ? AND status = ?", object.ID, object.Status).Updates(map[string]any{"status": domain.FileStatusDeleting, "updated_at": time.Now().UTC(), "version": gorm.Expr("version + 1")})
		if update.Error != nil {
			return update.Error
		}
		if update.RowsAffected != 1 {
			return application.ErrConflict
		}
		result = domain.StoredFile{File: toFile(object), Version: toVersion(version)}
		return nil
	})
	if err != nil {
		return domain.StoredFile{}, false, mapError(err)
	}
	return result, result.File.ID != "", nil
}

func (repository *Repository) MarkDeleted(ctx context.Context, tenantID, fileID string, updatedAt time.Time) error {
	return repository.database.WithContext(ctx).Transaction(func(transaction *gorm.DB) error {
		var object fileObjectModel
		if err := transaction.Clauses(clause.Locking{Strength: "UPDATE"}).Where("tenant_id = ? AND id = ? AND status = ?", tenantID, fileID, domain.FileStatusDeleting).Take(&object).Error; err != nil {
			return mapError(err)
		}
		if object.CurrentVersionID != nil {
			if err := transaction.Model(&fileVersionModel{}).Where("id = ? AND file_id = ?", *object.CurrentVersionID, fileID).Updates(map[string]any{"status": domain.FileVersionStatusRemoved}).Error; err != nil {
				return mapError(err)
			}
		}
		update := transaction.Model(&fileObjectModel{}).Where("tenant_id = ? AND id = ? AND status = ?", tenantID, fileID, domain.FileStatusDeleting).Updates(map[string]any{"status": domain.FileStatusDeleted, "updated_at": updatedAt.UTC(), "version": gorm.Expr("version + 1")})
		if update.Error != nil {
			return mapError(update.Error)
		}
		if update.RowsAffected != 1 {
			return application.ErrConflict
		}
		return nil
	})
}

func (repository *Repository) ReleaseCleanupClaim(ctx context.Context, tenantID, fileID, restoreStatus string, updatedAt time.Time) error {
	if restoreStatus != domain.FileStatusAvailable && restoreStatus != "ACTIVE" {
		restoreStatus = domain.FileStatusAvailable
	}
	update := repository.database.WithContext(ctx).Model(&fileObjectModel{}).Where("tenant_id = ? AND id = ? AND status = ?", tenantID, fileID, domain.FileStatusDeleting).Updates(map[string]any{"status": restoreStatus, "updated_at": updatedAt.UTC(), "version": gorm.Expr("version + 1")})
	if update.Error != nil {
		return mapError(update.Error)
	}
	if update.RowsAffected != 1 {
		return application.ErrConflict
	}
	return nil
}

func (repository *Repository) CreateJob(ctx context.Context, job domain.Job) (domain.Job, error) {
	model := toJobModel(job)
	if err := repository.database.WithContext(ctx).Create(&model).Error; err != nil {
		return domain.Job{}, mapError(err)
	}
	return toJob(model), nil
}

func (repository *Repository) ListJobs(ctx context.Context, tenantID string, query domain.PageRequest) (domain.PageResult[domain.Job], error) {
	database := repository.database.WithContext(ctx).Model(&asyncJobModel{}).Where("tenant_id = ?", tenantID)
	if query.Status != "" {
		database = database.Where("status = ?", query.Status)
	}
	if query.JobType != "" {
		database = database.Where("job_type = ?", query.JobType)
	}
	if query.ApplicationID != "" {
		database = database.Where("application_id = ?", query.ApplicationID)
	}
	if query.Query != "" {
		like := "%" + strings.TrimSpace(query.Query) + "%"
		database = database.Where("public_id LIKE ? OR aggregate_id LIKE ?", like, like)
	}
	var total int64
	if err := database.Count(&total).Error; err != nil {
		return domain.PageResult[domain.Job]{}, mapError(err)
	}
	var models []asyncJobModel
	if err := database.Order("created_at DESC, id DESC").Offset((query.Page - 1) * query.PageSize).Limit(query.PageSize).Find(&models).Error; err != nil {
		return domain.PageResult[domain.Job]{}, mapError(err)
	}
	items := make([]domain.Job, 0, len(models))
	for _, model := range models {
		items = append(items, toJob(model))
	}
	return domain.PageResult[domain.Job]{Items: items, Page: query.Page, PageSize: query.PageSize, Total: total}, nil
}

func (repository *Repository) GetJob(ctx context.Context, tenantID, jobID string) (domain.Job, error) {
	var model asyncJobModel
	if err := repository.database.WithContext(ctx).Where("tenant_id = ? AND public_id = ?", tenantID, jobID).Take(&model).Error; err != nil {
		return domain.Job{}, mapError(err)
	}
	return toJob(model), nil
}

func (repository *Repository) ClaimJob(ctx context.Context, workerID string, allowedTypes []string, now, staleBefore time.Time) (domain.Job, bool, error) {
	// PENDING 到期任务和锁已过期的 RUNNING 任务都可被领取；行锁只覆盖选择与状态更新，
	// worker 的耗时业务处理不占用数据库锁。
	var claimed domain.Job
	err := repository.database.WithContext(ctx).Transaction(func(transaction *gorm.DB) error {
		// A stale worker cannot retain a job forever. Existing audit exports use the same recovery
		// approach; generic workers only requeue the types that they were explicitly registered for.
		if err := transaction.Model(&asyncJobModel{}).Where("job_type IN ? AND status = ? AND locked_at < ?", allowedTypes, domain.JobStatusRunning, staleBefore.UTC()).Updates(map[string]any{"status": domain.JobStatusPending, "locked_by": nil, "locked_at": nil}).Error; err != nil {
			return err
		}
		var model asyncJobModel
		err := transaction.Clauses(clause.Locking{Strength: "UPDATE", Options: "SKIP LOCKED"}).Where("job_type IN ? AND status = ? AND available_at <= ?", allowedTypes, domain.JobStatusPending, now.UTC()).Order("priority ASC, id ASC").Take(&model).Error
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil
		}
		if err != nil {
			return err
		}
		update := transaction.Model(&asyncJobModel{}).Where("id = ? AND status = ?", model.ID, domain.JobStatusPending).Updates(map[string]any{"status": domain.JobStatusRunning, "locked_by": workerID, "locked_at": now.UTC(), "attempts": gorm.Expr("attempts + 1")})
		if update.Error != nil {
			return update.Error
		}
		if update.RowsAffected != 1 {
			return application.ErrConflict
		}
		model.Status, model.LockedBy = domain.JobStatusRunning, &workerID
		model.LockedAt = &now
		model.Attempts++
		claimed = toJob(model)
		return nil
	})
	if err != nil {
		return domain.Job{}, false, mapError(err)
	}
	return claimed, claimed.PublicID != "", nil
}

func (repository *Repository) CompleteJob(ctx context.Context, tenantID, jobID string, completedAt time.Time) error {
	// 只有 RUNNING 可完成；RowsAffected 校验把重复完成或已被其他 worker 接管转换为显式冲突。
	result := repository.database.WithContext(ctx).Model(&asyncJobModel{}).Where("tenant_id = ? AND public_id = ? AND status = ?", tenantID, jobID, domain.JobStatusRunning).Updates(map[string]any{"status": domain.JobStatusSucceeded, "completed_at": completedAt.UTC(), "locked_by": nil, "locked_at": nil, "last_error_code": nil, "last_error_message": nil})
	if result.Error != nil {
		return mapError(result.Error)
	}
	if result.RowsAffected != 1 {
		return application.ErrConflict
	}
	return nil
}

func (repository *Repository) FailJob(ctx context.Context, job domain.Job, code, message string, retryable bool, retryAt, now time.Time) error {
	// 状态转换同时核对 locked_by，过期 worker 不能覆盖新持有者已经推进的任务状态。
	updates := map[string]any{"locked_by": nil, "locked_at": nil, "last_error_code": optional(code), "last_error_message": optional(message)}
	if !retryable {
		updates["status"] = domain.JobStatusDead
		updates["completed_at"] = now.UTC()
	} else if job.Attempts >= job.MaxAttempts {
		updates["status"] = domain.JobStatusFailed
		updates["completed_at"] = now.UTC()
	} else {
		updates["status"] = domain.JobStatusPending
		updates["available_at"] = retryAt.UTC()
	}
	result := repository.database.WithContext(ctx).Model(&asyncJobModel{}).Where("tenant_id = ? AND public_id = ? AND status = ?", job.TenantID, job.PublicID, domain.JobStatusRunning).Updates(updates)
	if result.Error != nil {
		return mapError(result.Error)
	}
	if result.RowsAffected != 1 {
		return application.ErrConflict
	}
	return nil
}

func (repository *Repository) CancelJob(ctx context.Context, tenantID, jobID string, now time.Time) error {
	result := repository.database.WithContext(ctx).Model(&asyncJobModel{}).Where("tenant_id = ? AND public_id = ? AND status IN ?", tenantID, jobID, []string{domain.JobStatusPending, domain.JobStatusFailed, domain.JobStatusDead}).Updates(map[string]any{"status": domain.JobStatusCancelled, "completed_at": now.UTC(), "locked_by": nil, "locked_at": nil})
	if result.Error != nil {
		return mapError(result.Error)
	}
	if result.RowsAffected != 1 {
		return application.ErrConflict
	}
	return nil
}

func (repository *Repository) RetryJob(ctx context.Context, tenantID, jobID string, now time.Time) error {
	result := repository.database.WithContext(ctx).Model(&asyncJobModel{}).Where("tenant_id = ? AND public_id = ? AND status IN ?", tenantID, jobID, []string{domain.JobStatusFailed, domain.JobStatusDead}).Updates(map[string]any{"status": domain.JobStatusPending, "available_at": now.UTC(), "locked_by": nil, "locked_at": nil, "attempts": 0, "completed_at": nil, "last_error_code": nil, "last_error_message": nil})
	if result.Error != nil {
		return mapError(result.Error)
	}
	if result.RowsAffected != 1 {
		return application.ErrConflict
	}
	return nil
}

func (repository *Repository) CreateRerun(ctx context.Context, job domain.Job) (domain.Job, error) {
	return repository.CreateJob(ctx, job)
}

func toFile(model fileObjectModel) domain.File {
	return domain.File{ID: model.ID, TenantID: model.TenantID, ApplicationID: model.ApplicationID, OriginalName: model.OriginalName, FileExtension: dereference(model.FileExtension), MediaType: model.MediaType, Classification: model.Classification, OwnerUserID: dereference(model.OwnerUserID), CurrentVersionNo: model.CurrentVersionNo, CurrentVersionID: dereference(model.CurrentVersionID), Status: model.Status, Version: model.Version, CreatedAt: model.CreatedAt, UpdatedAt: model.UpdatedAt}
}

func toVersion(model fileVersionModel) domain.FileVersion {
	return domain.FileVersion{ID: model.ID, FileID: model.FileID, VersionNo: model.VersionNo, StorageRelativePath: model.StorageRelativePath, SizeBytes: model.SizeBytes, SHA256: append([]byte(nil), model.SHA256...), MediaType: model.MediaType, OriginalName: model.OriginalName, UploaderUserID: dereference(model.UploaderUserID), UploadRequestID: dereference(model.UploadRequestID), Status: model.Status, CreatedAt: model.CreatedAt}
}

func toJob(model asyncJobModel) domain.Job {
	return domain.Job{ID: model.ID, PublicID: dereference(model.PublicID), TenantID: model.TenantID, ApplicationID: dereference(model.ApplicationID), JobType: model.JobType, AggregateType: dereference(model.AggregateType), AggregateID: dereference(model.AggregateID), Payload: append([]byte(nil), model.Payload...), Status: model.Status, Priority: model.Priority, AvailableAt: model.AvailableAt, LockedBy: dereference(model.LockedBy), LockedAt: model.LockedAt, Attempts: model.Attempts, MaxAttempts: model.MaxAttempts, LastErrorCode: dereference(model.LastErrorCode), LastErrorMessage: dereference(model.LastErrorMessage), ResultFileID: dereference(model.ResultFileID), CreatedAt: model.CreatedAt, CompletedAt: model.CompletedAt}
}

func toJobModel(job domain.Job) asyncJobModel {
	return asyncJobModel{PublicID: optional(job.PublicID), TenantID: job.TenantID, ApplicationID: optional(job.ApplicationID), JobType: job.JobType, AggregateType: optional(job.AggregateType), AggregateID: optional(job.AggregateID), Payload: append([]byte(nil), job.Payload...), Status: job.Status, Priority: job.Priority, AvailableAt: job.AvailableAt.UTC(), MaxAttempts: job.MaxAttempts, CreatedAt: job.CreatedAt.UTC()}
}

func optional(value string) *string {
	value = strings.TrimSpace(value)
	if value == "" {
		return nil
	}
	return &value
}

func dereference(value *string) string {
	if value == nil {
		return ""
	}
	return *value
}

func mapError(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return application.ErrNotFound
	}
	return err
}
