package infrastructure

import (
	"bytes"
	"context"
	"crypto/sha256"
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
	UploadRequestHash                                                []byte
	CreatedAt                                                        time.Time
}

func (fileVersionModel) TableName() string { return "file_version" }

type fileUploadSessionModel struct {
	ID                      uint64
	TenantID, ApplicationID string
	RequestID               string
	RequestHash             []byte
	FileID, VersionID       *string
	Status                  string
	CreatedAt, UpdatedAt    time.Time
}

func (fileUploadSessionModel) TableName() string { return "file_upload_session" }

type fileBindingModel struct {
	ID, TenantID, ApplicationID, FileID string
	ResourceType, ResourceID            string
	BindingType, DisplayName, Status    string
	SortOrder                           int
	CreatedAt                           time.Time
	CreatedBy                           *string
}

func (fileBindingModel) TableName() string { return "file_binding" }

type asyncJobModel struct {
	ID                                             uint64
	PublicID                                       *string
	ParentJobID                                    *uint64
	TenantID                                       string
	ApplicationID, AggregateType, AggregateID      *string
	IdempotencyKey                                 *string
	JobType                                        string
	Payload                                        []byte
	RequestHash                                    []byte
	RequestID, TraceID, CorrelationID, BusinessRef *string
	Status                                         string
	Priority                                       int
	AvailableAt                                    time.Time
	LockedBy                                       *string
	LockedAt                                       *time.Time
	LastAttemptAt                                  *time.Time
	Attempts, MaxAttempts                          uint
	RetryCount                                     uint
	LastErrorCode, LastErrorMessage, ResultFileID  *string
	CreatedAt                                      time.Time
	CompletedAt                                    *time.Time
	LastSucceededAt                                *time.Time
}

func (asyncJobModel) TableName() string { return "async_job" }

func (repository *Repository) CreateWriting(ctx context.Context, file domain.File, version domain.FileVersion) error {
	return repository.database.WithContext(ctx).Transaction(func(transaction *gorm.DB) error {
		// 独立网关库不复制平台应用目录；应用身份已经由上游 Bearer
		// Token 验证，数据库只保存稳定 application_id 供租户隔离和审计查询。
		if strings.TrimSpace(file.TenantID) == "" || strings.TrimSpace(file.ApplicationID) == "" {
			return application.ErrForbidden
		}
		fileModel := fileObjectModel{ID: file.ID, TenantID: file.TenantID, ApplicationID: file.ApplicationID, OriginalName: file.OriginalName, FileExtension: optional(file.FileExtension), MediaType: file.MediaType, Classification: file.Classification, OwnerUserID: optional(file.OwnerUserID), CurrentVersionNo: file.CurrentVersionNo, CurrentVersionID: optional(file.CurrentVersionID), Status: file.Status, Version: file.Version, CreatedAt: file.CreatedAt.UTC(), UpdatedAt: file.UpdatedAt.UTC(), CreatedBy: optional(file.OwnerUserID), UpdatedBy: optional(file.OwnerUserID)}
		if err := transaction.Create(&fileModel).Error; err != nil {
			return mapError(err)
		}
		versionModel := fileVersionModel{ID: version.ID, FileID: version.FileID, VersionNo: version.VersionNo, StorageRelativePath: version.StorageRelativePath, MediaType: version.MediaType, OriginalName: version.OriginalName, UploaderUserID: optional(version.UploaderUserID), UploadRequestID: optional(version.UploadRequestID), UploadRequestHash: version.UploadRequestHash, Status: version.Status, CreatedAt: version.CreatedAt.UTC()}
		if err := transaction.Create(&versionModel).Error; err != nil {
			return mapError(err)
		}
		return nil
	})
}

// ReserveUpload 以 file_upload_session 的唯一键作为并发仲裁点，并在同一事务中创建
// 文件、版本和上传会话。唯一键竞争失败后只复用摘要一致且已经 READY 的结果；WRITING、
// FAILED 或摘要不同均返回冲突，调用方不会收到虚假的上传成功。
func (repository *Repository) ReserveUpload(ctx context.Context, file domain.File, version domain.FileVersion) (domain.StoredFile, bool, error) {
	requestID := strings.TrimSpace(version.UploadRequestID)
	if requestID == "" || len(version.UploadRequestHash) != sha256.Size {
		return domain.StoredFile{}, false, application.ErrValidation
	}
	if strings.TrimSpace(file.TenantID) == "" || strings.TrimSpace(file.ApplicationID) == "" {
		return domain.StoredFile{}, false, application.ErrForbidden
	}

	created := domain.StoredFile{File: file, Version: version}
	now := file.CreatedAt.UTC()
	err := repository.database.WithContext(ctx).Transaction(func(transaction *gorm.DB) error {
		session := fileUploadSessionModel{
			TenantID: file.TenantID, ApplicationID: file.ApplicationID, RequestID: requestID,
			RequestHash: append([]byte(nil), version.UploadRequestHash...), Status: "WRITING",
			CreatedAt: now, UpdatedAt: now,
		}
		// 先抢占唯一请求号。并发事务会在该 INSERT 上由 MySQL 唯一键串行化；后续任一
		// 元数据写入失败都会回滚会话，不会留下指向不存在文件的预留记录。
		if err := transaction.Create(&session).Error; err != nil {
			return err
		}
		fileModel := fileObjectModel{ID: file.ID, TenantID: file.TenantID, ApplicationID: file.ApplicationID, OriginalName: file.OriginalName, FileExtension: optional(file.FileExtension), MediaType: file.MediaType, Classification: file.Classification, OwnerUserID: optional(file.OwnerUserID), CurrentVersionNo: file.CurrentVersionNo, CurrentVersionID: optional(file.CurrentVersionID), Status: file.Status, Version: file.Version, CreatedAt: now, UpdatedAt: file.UpdatedAt.UTC(), CreatedBy: optional(file.OwnerUserID), UpdatedBy: optional(file.OwnerUserID)}
		if err := transaction.Create(&fileModel).Error; err != nil {
			return err
		}
		versionModel := fileVersionModel{ID: version.ID, FileID: version.FileID, VersionNo: version.VersionNo, StorageRelativePath: version.StorageRelativePath, MediaType: version.MediaType, OriginalName: version.OriginalName, UploaderUserID: optional(version.UploaderUserID), UploadRequestID: optional(requestID), UploadRequestHash: version.UploadRequestHash, Status: version.Status, CreatedAt: version.CreatedAt.UTC()}
		if err := transaction.Create(&versionModel).Error; err != nil {
			return err
		}
		result := transaction.Model(&fileUploadSessionModel{}).Where("id = ? AND status = ?", session.ID, "WRITING").Updates(map[string]any{"file_id": file.ID, "version_id": version.ID, "updated_at": now})
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected != 1 {
			return application.ErrConflict
		}
		return nil
	})
	if err == nil {
		return created, true, nil
	}

	// 不依赖驱动专用的 duplicate-key 文本判断：只有确实存在同一业务唯一键的会话
	// 才进入重放分支；ULID 冲突或其他数据库错误仍按原错误返回。
	existing, findErr := repository.findUploadSession(ctx, file.TenantID, file.ApplicationID, requestID)
	if findErr != nil {
		return domain.StoredFile{}, false, mapError(err)
	}
	if len(existing.session.RequestHash) != sha256.Size || !bytes.Equal(existing.session.RequestHash, version.UploadRequestHash) {
		return domain.StoredFile{}, false, application.ErrConflict
	}
	if existing.session.Status != "READY" || existing.file.Status != domain.FileStatusReady || existing.version.Status != domain.FileVersionStatusReady {
		return domain.StoredFile{}, false, application.ErrConflict
	}
	return domain.StoredFile{File: toFile(existing.file), Version: toVersion(existing.version)}, false, nil
}

type uploadSessionRecord struct {
	session fileUploadSessionModel
	file    fileObjectModel
	version fileVersionModel
}

func (repository *Repository) findUploadSession(ctx context.Context, tenantID, applicationID, requestID string) (uploadSessionRecord, error) {
	var record uploadSessionRecord
	if err := repository.database.WithContext(ctx).Where("tenant_id = ? AND application_id = ? AND request_id = ?", tenantID, applicationID, requestID).Take(&record.session).Error; err != nil {
		return uploadSessionRecord{}, err
	}
	if record.session.FileID == nil || record.session.VersionID == nil {
		return uploadSessionRecord{}, application.ErrConflict
	}
	if err := repository.database.WithContext(ctx).Where("tenant_id = ? AND application_id = ? AND id = ?", tenantID, applicationID, *record.session.FileID).Take(&record.file).Error; err != nil {
		return uploadSessionRecord{}, err
	}
	if err := repository.database.WithContext(ctx).Where("id = ? AND file_id = ?", *record.session.VersionID, *record.session.FileID).Take(&record.version).Error; err != nil {
		return uploadSessionRecord{}, err
	}
	return record, nil
}

// MarkValidating 在二进制原子落盘后记录大小与摘要，并把文件推进到内容校验阶段。
func (repository *Repository) MarkValidating(ctx context.Context, tenantID, fileID string, size uint64, digest []byte, updatedAt time.Time) error {
	return repository.database.WithContext(ctx).Transaction(func(transaction *gorm.DB) error {
		var object fileObjectModel
		if err := transaction.Clauses(clause.Locking{Strength: "UPDATE"}).Where("tenant_id = ? AND id = ? AND status = ?", tenantID, fileID, domain.FileStatusPendingUpload).Take(&object).Error; err != nil {
			return mapError(err)
		}
		if object.CurrentVersionID == nil {
			return application.ErrConflict
		}
		updateVersion := transaction.Model(&fileVersionModel{}).Where("id = ? AND file_id = ? AND status = ?", *object.CurrentVersionID, fileID, domain.FileVersionStatusPendingUpload).Updates(map[string]any{"size_bytes": size, "sha256": digest, "status": domain.FileVersionStatusValidating})
		if updateVersion.Error != nil {
			return mapError(updateVersion.Error)
		}
		if updateVersion.RowsAffected != 1 {
			return application.ErrConflict
		}
		updateObject := transaction.Model(&fileObjectModel{}).Where("tenant_id = ? AND id = ? AND status = ?", tenantID, fileID, domain.FileStatusPendingUpload).Updates(map[string]any{"status": domain.FileStatusValidating, "updated_at": updatedAt.UTC(), "version": gorm.Expr("version + 1")})
		if updateObject.Error != nil {
			return mapError(updateObject.Error)
		}
		if updateObject.RowsAffected != 1 {
			return application.ErrConflict
		}
		return nil
	})
}

// MarkReady 在结构校验通过后原子开放当前版本下载。
func (repository *Repository) MarkReady(ctx context.Context, tenantID, fileID string, updatedAt time.Time) error {
	return repository.transitionValidationState(ctx, tenantID, fileID, domain.FileStatusReady, domain.FileVersionStatusReady, updatedAt)
}

// MarkRejected 在内容校验失败后关闭文件与版本，拒绝任何下载。
func (repository *Repository) MarkRejected(ctx context.Context, tenantID, fileID string, updatedAt time.Time) error {
	return repository.transitionValidationState(ctx, tenantID, fileID, domain.FileStatusRejected, domain.FileVersionStatusRejected, updatedAt)
}

func (repository *Repository) transitionValidationState(ctx context.Context, tenantID, fileID, fileStatus, versionStatus string, updatedAt time.Time) error {
	return repository.database.WithContext(ctx).Transaction(func(transaction *gorm.DB) error {
		var object fileObjectModel
		if err := transaction.Clauses(clause.Locking{Strength: "UPDATE"}).Where("tenant_id = ? AND id = ? AND status = ?", tenantID, fileID, domain.FileStatusValidating).Take(&object).Error; err != nil {
			return mapError(err)
		}
		if object.CurrentVersionID == nil {
			return application.ErrConflict
		}
		versionResult := transaction.Model(&fileVersionModel{}).Where("id = ? AND file_id = ? AND status = ?", *object.CurrentVersionID, fileID, domain.FileVersionStatusValidating).Update("status", versionStatus)
		if versionResult.Error != nil || versionResult.RowsAffected != 1 {
			if versionResult.Error != nil {
				return mapError(versionResult.Error)
			}
			return application.ErrConflict
		}
		objectResult := transaction.Model(&fileObjectModel{}).Where("tenant_id = ? AND id = ? AND status = ?", tenantID, fileID, domain.FileStatusValidating).Updates(map[string]any{"status": fileStatus, "updated_at": updatedAt.UTC(), "version": gorm.Expr("version + 1")})
		if objectResult.Error != nil || objectResult.RowsAffected != 1 {
			if objectResult.Error != nil {
				return mapError(objectResult.Error)
			}
			return application.ErrConflict
		}
		sessionStatus := "FAILED"
		if fileStatus == domain.FileStatusReady {
			sessionStatus = "READY"
		}
		// 没有 request_id 的上传不存在会话，RowsAffected=0 属于正常情况；存在会话时
		// 只允许从 WRITING 单向完成，失败记录不能被后续重放重新开放。
		if err := transaction.Model(&fileUploadSessionModel{}).Where("tenant_id = ? AND file_id = ? AND status = ?", tenantID, fileID, "WRITING").Updates(map[string]any{"status": sessionStatus, "updated_at": updatedAt.UTC()}).Error; err != nil {
			return mapError(err)
		}
		return nil
	})
}

func (repository *Repository) MarkFailed(ctx context.Context, tenantID, fileID string, updatedAt time.Time) error {
	return repository.database.WithContext(ctx).Transaction(func(transaction *gorm.DB) error {
		var object fileObjectModel
		if err := transaction.Clauses(clause.Locking{Strength: "UPDATE"}).Where("tenant_id = ? AND id = ? AND status IN ?", tenantID, fileID, []string{domain.FileStatusPendingUpload, domain.FileStatusValidating}).Take(&object).Error; err != nil {
			return mapError(err)
		}
		if object.CurrentVersionID != nil {
			if err := transaction.Model(&fileVersionModel{}).Where("id = ? AND file_id = ?", *object.CurrentVersionID, fileID).Updates(map[string]any{"status": domain.FileVersionStatusFailed}).Error; err != nil {
				return mapError(err)
			}
		}
		if err := transaction.Model(&fileObjectModel{}).Where("tenant_id = ? AND id = ?", tenantID, fileID).Updates(map[string]any{"status": domain.FileStatusFailed, "updated_at": updatedAt.UTC(), "version": gorm.Expr("version + 1")}).Error; err != nil {
			return mapError(err)
		}
		return mapError(transaction.Model(&fileUploadSessionModel{}).Where("tenant_id = ? AND file_id = ? AND status = ?", tenantID, fileID, "WRITING").Updates(map[string]any{"status": "FAILED", "updated_at": updatedAt.UTC()}).Error)
	})
}

func (repository *Repository) GetAvailable(ctx context.Context, tenantID, fileID string) (domain.StoredFile, error) {
	var object fileObjectModel
	if err := repository.database.WithContext(ctx).Where("tenant_id = ? AND id = ? AND status IN ?", tenantID, fileID, []string{domain.FileStatusReady, "AVAILABLE", "ACTIVE"}).Take(&object).Error; err != nil {
		return domain.StoredFile{}, mapError(err)
	}
	if object.CurrentVersionID == nil {
		return domain.StoredFile{}, application.ErrNotFound
	}
	var version fileVersionModel
	if err := repository.database.WithContext(ctx).Where("id = ? AND file_id = ? AND status IN ?", *object.CurrentVersionID, fileID, []string{domain.FileVersionStatusReady, "AVAILABLE", "ACTIVE"}).Take(&version).Error; err != nil {
		return domain.StoredFile{}, mapError(err)
	}
	return domain.StoredFile{File: toFile(object), Version: toVersion(version)}, nil
}

// CreateBinding 在同一事务内锁定 READY 文件并校验租户、应用归属；唯一键冲突时查询并
// 返回现有 ACTIVE 绑定，使调用方可安全重放相同的业务绑定请求。
func (repository *Repository) CreateBinding(ctx context.Context, binding domain.FileBinding) (domain.FileBinding, error) {
	var result domain.FileBinding
	err := repository.database.WithContext(ctx).Transaction(func(transaction *gorm.DB) error {
		var object fileObjectModel
		if err := transaction.Clauses(clause.Locking{Strength: "SHARE"}).Where("tenant_id = ? AND application_id = ? AND id = ? AND status = ?", binding.TenantID, binding.ApplicationID, binding.FileID, domain.FileStatusReady).Take(&object).Error; err != nil {
			return mapError(err)
		}
		var existing fileBindingModel
		err := transaction.Where("application_id = ? AND resource_type = ? AND resource_id = ? AND file_id = ? AND binding_type = ?", binding.ApplicationID, binding.ResourceType, binding.ResourceID, binding.FileID, binding.BindingType).Take(&existing).Error
		if err == nil {
			if existing.TenantID != binding.TenantID || existing.Status != "ACTIVE" {
				return application.ErrConflict
			}
			result = toBinding(existing)
			return nil
		}
		if !errors.Is(err, gorm.ErrRecordNotFound) {
			return mapError(err)
		}
		model := fileBindingModel{ID: binding.ID, TenantID: binding.TenantID, ApplicationID: binding.ApplicationID, FileID: binding.FileID, ResourceType: binding.ResourceType, ResourceID: binding.ResourceID, BindingType: binding.BindingType, DisplayName: binding.DisplayName, SortOrder: binding.SortOrder, Status: "ACTIVE", CreatedAt: binding.CreatedAt.UTC(), CreatedBy: optional(binding.CreatedBy)}
		if err := transaction.Create(&model).Error; err != nil {
			return mapError(err)
		}
		result = toBinding(model)
		return nil
	})
	return result, mapError(err)
}

// DeactivateBinding 保留绑定记录并将 ACTIVE 原子推进为 DISABLED，避免删除后失去关联审计。
func (repository *Repository) DeactivateBinding(ctx context.Context, tenantID, applicationID, fileID, bindingID string, updatedAt time.Time) error {
	_ = updatedAt // 既有 file_binding 尚无 updated_at；状态变更时间将在后续审计事件中记录。
	result := repository.database.WithContext(ctx).Model(&fileBindingModel{}).Where("tenant_id = ? AND application_id = ? AND file_id = ? AND id = ? AND status = ?", tenantID, applicationID, fileID, bindingID, "ACTIVE").Update("status", "DISABLED")
	if result.Error != nil {
		return mapError(result.Error)
	}
	if result.RowsAffected != 1 {
		return application.ErrConflict
	}
	return nil
}

// HasActiveBinding 精确校验租户、文件、应用和业务资源，防止只知道 file_id 即跨资源下载。
func (repository *Repository) HasActiveBinding(ctx context.Context, tenantID, fileID, applicationID, resourceType, resourceID string) (bool, error) {
	var count int64
	err := repository.database.WithContext(ctx).Model(&fileBindingModel{}).Where("tenant_id = ? AND file_id = ? AND application_id = ? AND resource_type = ? AND resource_id = ? AND status = ?", tenantID, fileID, applicationID, resourceType, resourceID, "ACTIVE").Count(&count).Error
	return count > 0, mapError(err)
}

// ListRecoveryCandidates 返回更新时间早于截止点的未完成文件及当前版本。它不领取长事务锁；
// 后续状态迁移都带旧状态 CAS，因此并发对账最多产生可识别冲突，不会重复开放文件。
func (repository *Repository) ListRecoveryCandidates(ctx context.Context, tenantID string, cutoff time.Time, limit int) ([]domain.StoredFile, error) {
	var objects []fileObjectModel
	if err := repository.database.WithContext(ctx).Where("tenant_id = ? AND status IN ? AND updated_at < ? AND current_version_id IS NOT NULL", tenantID, []string{domain.FileStatusPendingUpload, domain.FileStatusValidating}, cutoff.UTC()).Order("updated_at ASC, id ASC").Limit(limit).Find(&objects).Error; err != nil {
		return nil, mapError(err)
	}
	items := make([]domain.StoredFile, 0, len(objects))
	for _, object := range objects {
		var version fileVersionModel
		if err := repository.database.WithContext(ctx).Where("id = ? AND file_id = ?", *object.CurrentVersionID, object.ID).Take(&version).Error; err != nil {
			return nil, mapError(err)
		}
		items = append(items, domain.StoredFile{File: toFile(object), Version: toVersion(version)})
	}
	return items, nil
}

// ClaimExpiredUnbound 用 FOR UPDATE SKIP LOCKED 领取一条旧且无绑定的文件，再推进到 DELETING。
// 慢速磁盘删除在短事务提交后执行，因此多个清理实例不会互相等待，也不会同时删除同一文件。
func (repository *Repository) ClaimExpiredUnbound(ctx context.Context, tenantID string, cutoff time.Time) (domain.StoredFile, bool, error) {
	var result domain.StoredFile
	err := repository.database.WithContext(ctx).Transaction(func(transaction *gorm.DB) error {
		var object fileObjectModel
		err := transaction.Clauses(clause.Locking{Strength: "UPDATE", Options: "SKIP LOCKED"}).Where("tenant_id = ? AND status IN ? AND created_at < ? AND current_version_id IS NOT NULL", tenantID, []string{domain.FileStatusReady, "AVAILABLE", "ACTIVE"}, cutoff.UTC()).Where("NOT EXISTS (SELECT 1 FROM file_binding binding WHERE binding.file_id = file_object.id AND binding.status = ?)", "ACTIVE").Order("created_at ASC, id ASC").Take(&object).Error
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil
		}
		if err != nil {
			return err
		}
		var version fileVersionModel
		if err := transaction.Where("id = ? AND file_id = ? AND status IN ?", *object.CurrentVersionID, object.ID, []string{domain.FileVersionStatusReady, "AVAILABLE", "ACTIVE"}).Take(&version).Error; err != nil {
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
	if restoreStatus != domain.FileStatusReady && restoreStatus != "AVAILABLE" && restoreStatus != "ACTIVE" {
		restoreStatus = domain.FileStatusReady
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
	if job.IdempotencyKey == "" {
		if err := repository.database.WithContext(ctx).Create(&model).Error; err != nil {
			return domain.Job{}, mapError(err)
		}
		return toJob(model), nil
	}
	result := repository.database.WithContext(ctx).Clauses(clause.OnConflict{DoNothing: true}).Create(&model)
	if result.Error != nil {
		return domain.Job{}, mapError(result.Error)
	}
	if result.RowsAffected == 1 {
		return toJob(model), nil
	}
	var existing asyncJobModel
	database := repository.database.WithContext(ctx).Where("tenant_id = ? AND job_type = ? AND idempotency_key = ?", job.TenantID, job.JobType, job.IdempotencyKey)
	if job.ApplicationID == "" {
		database = database.Where("application_id IS NULL")
	} else {
		database = database.Where("application_id = ?", job.ApplicationID)
	}
	err := database.Take(&existing).Error
	if err != nil {
		return domain.Job{}, application.ErrConflict
	}
	if len(existing.RequestHash) != sha256.Size || !bytes.Equal(existing.RequestHash, job.RequestHash) {
		return domain.Job{}, application.ErrConflict
	}
	return toJob(existing), nil
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
	if err := repository.attachParentPublicIDs(repository.database.WithContext(ctx), items); err != nil {
		return domain.PageResult[domain.Job]{}, err
	}
	return domain.PageResult[domain.Job]{Items: items, Page: query.Page, PageSize: query.PageSize, Total: total}, nil
}

func (repository *Repository) GetJob(ctx context.Context, tenantID, jobID string) (domain.Job, error) {
	var model asyncJobModel
	if err := repository.database.WithContext(ctx).Where("tenant_id = ? AND public_id = ?", tenantID, jobID).Take(&model).Error; err != nil {
		return domain.Job{}, mapError(err)
	}
	jobs := []domain.Job{toJob(model)}
	if err := repository.attachParentPublicIDs(repository.database.WithContext(ctx), jobs); err != nil {
		return domain.Job{}, err
	}
	return jobs[0], nil
}

func (repository *Repository) ClaimJob(ctx context.Context, workerID string, allowedTypes []string, now, staleBefore time.Time) (domain.Job, bool, error) {
	// PENDING 到期任务和锁已过期的 RUNNING 任务都可被领取；行锁只覆盖选择与状态更新，
	// worker 的耗时业务处理不占用数据库锁。
	var claimed domain.Job
	err := repository.database.WithContext(ctx).Transaction(func(transaction *gorm.DB) error {
		// A stale worker cannot retain a job forever. Existing audit exports use the same recovery
		// approach; generic workers only requeue the types that they were explicitly registered for.
		if err := transaction.Model(&asyncJobModel{}).Where("job_type IN ? AND status = ? AND locked_at < ?", allowedTypes, domain.JobStatusRunning, staleBefore.UTC()).Updates(map[string]any{"status": domain.JobStatusPending, "locked_by": nil, "locked_at": nil, "retry_count": gorm.Expr("retry_count + 1")}).Error; err != nil {
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
		update := transaction.Model(&asyncJobModel{}).Where("id = ? AND status = ?", model.ID, domain.JobStatusPending).Updates(map[string]any{"status": domain.JobStatusRunning, "locked_by": workerID, "locked_at": now.UTC(), "last_attempt_at": now.UTC(), "attempts": gorm.Expr("attempts + 1")})
		if update.Error != nil {
			return update.Error
		}
		if update.RowsAffected != 1 {
			return application.ErrConflict
		}
		model.Status, model.LockedBy = domain.JobStatusRunning, &workerID
		model.LockedAt = &now
		model.LastAttemptAt = &now
		model.Attempts++
		claimed = toJob(model)
		return nil
	})
	if err != nil {
		return domain.Job{}, false, mapError(err)
	}
	return claimed, claimed.PublicID != "", nil
}

func (repository *Repository) CompleteJob(ctx context.Context, job domain.Job, completedAt time.Time) error {
	// 完成操作同时核对 worker lease。旧 Worker 即使在租约过期后恢复，也不能覆盖已经
	// 被新 Worker 领取并执行的同一任务。
	result := repository.database.WithContext(ctx).Model(&asyncJobModel{}).Where("tenant_id = ? AND public_id = ? AND status = ? AND locked_by = ?", job.TenantID, job.PublicID, domain.JobStatusRunning, job.LockedBy).Updates(map[string]any{"status": domain.JobStatusSucceeded, "completed_at": completedAt.UTC(), "last_succeeded_at": completedAt.UTC(), "locked_by": nil, "locked_at": nil, "last_error_code": nil, "last_error_message": nil})
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
		updates["retry_count"] = gorm.Expr("retry_count + 1")
	}
	result := repository.database.WithContext(ctx).Model(&asyncJobModel{}).Where("tenant_id = ? AND public_id = ? AND status = ? AND locked_by = ?", job.TenantID, job.PublicID, domain.JobStatusRunning, job.LockedBy).Updates(updates)
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
	result := repository.database.WithContext(ctx).Model(&asyncJobModel{}).Where("tenant_id = ? AND public_id = ? AND status IN ?", tenantID, jobID, []string{domain.JobStatusFailed, domain.JobStatusDead}).Updates(map[string]any{"status": domain.JobStatusPending, "available_at": now.UTC(), "locked_by": nil, "locked_at": nil, "attempts": 0, "retry_count": gorm.Expr("retry_count + 1"), "completed_at": nil, "last_error_code": nil, "last_error_message": nil})
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
	return domain.FileVersion{ID: model.ID, FileID: model.FileID, VersionNo: model.VersionNo, StorageRelativePath: model.StorageRelativePath, SizeBytes: model.SizeBytes, SHA256: append([]byte(nil), model.SHA256...), MediaType: model.MediaType, OriginalName: model.OriginalName, UploaderUserID: dereference(model.UploaderUserID), UploadRequestID: dereference(model.UploadRequestID), UploadRequestHash: append([]byte(nil), model.UploadRequestHash...), Status: model.Status, CreatedAt: model.CreatedAt}
}

func toBinding(model fileBindingModel) domain.FileBinding {
	return domain.FileBinding{ID: model.ID, TenantID: model.TenantID, ApplicationID: model.ApplicationID, FileID: model.FileID, ResourceType: model.ResourceType, ResourceID: model.ResourceID, BindingType: model.BindingType, DisplayName: model.DisplayName, SortOrder: model.SortOrder, Status: model.Status, CreatedBy: dereference(model.CreatedBy), CreatedAt: model.CreatedAt}
}

func toJob(model asyncJobModel) domain.Job {
	return domain.Job{ID: model.ID, PublicID: dereference(model.PublicID), ParentJobID: dereferenceUint64(model.ParentJobID), TenantID: model.TenantID, ApplicationID: dereference(model.ApplicationID), JobType: model.JobType, AggregateType: dereference(model.AggregateType), AggregateID: dereference(model.AggregateID), IdempotencyKey: dereference(model.IdempotencyKey), Payload: append([]byte(nil), model.Payload...), RequestHash: append([]byte(nil), model.RequestHash...), RequestID: dereference(model.RequestID), TraceID: dereference(model.TraceID), CorrelationID: dereference(model.CorrelationID), BusinessRef: dereference(model.BusinessRef), Status: model.Status, Priority: model.Priority, AvailableAt: model.AvailableAt, LockedBy: dereference(model.LockedBy), LockedAt: model.LockedAt, LastAttemptAt: model.LastAttemptAt, Attempts: model.Attempts, RetryCount: model.RetryCount, MaxAttempts: model.MaxAttempts, LastErrorCode: dereference(model.LastErrorCode), LastErrorMessage: dereference(model.LastErrorMessage), ResultFileID: dereference(model.ResultFileID), CreatedAt: model.CreatedAt, CompletedAt: model.CompletedAt, LastSucceededAt: model.LastSucceededAt}
}

func toJobModel(job domain.Job) asyncJobModel {
	return asyncJobModel{PublicID: optional(job.PublicID), ParentJobID: optionalUint64(job.ParentJobID), TenantID: job.TenantID, ApplicationID: optional(job.ApplicationID), JobType: job.JobType, AggregateType: optional(job.AggregateType), AggregateID: optional(job.AggregateID), IdempotencyKey: optional(job.IdempotencyKey), Payload: append([]byte(nil), job.Payload...), RequestHash: append([]byte(nil), job.RequestHash...), RequestID: optional(job.RequestID), TraceID: optional(job.TraceID), CorrelationID: optional(job.CorrelationID), BusinessRef: optional(job.BusinessRef), Status: job.Status, Priority: job.Priority, AvailableAt: job.AvailableAt.UTC(), LockedBy: optional(job.LockedBy), LockedAt: job.LockedAt, LastAttemptAt: job.LastAttemptAt, Attempts: job.Attempts, RetryCount: job.RetryCount, MaxAttempts: job.MaxAttempts, LastErrorCode: optional(job.LastErrorCode), LastErrorMessage: optional(job.LastErrorMessage), ResultFileID: optional(job.ResultFileID), CreatedAt: job.CreatedAt.UTC(), CompletedAt: job.CompletedAt, LastSucceededAt: job.LastSucceededAt}
}

func (repository *Repository) attachParentPublicIDs(database *gorm.DB, jobs []domain.Job) error {
	parentIDs := make([]uint64, 0, len(jobs))
	for _, job := range jobs {
		if job.ParentJobID != 0 {
			parentIDs = append(parentIDs, job.ParentJobID)
		}
	}
	if len(parentIDs) == 0 {
		return nil
	}
	var parents []asyncJobModel
	if err := database.Select("id, public_id").Where("id IN ?", parentIDs).Find(&parents).Error; err != nil {
		return mapError(err)
	}
	publicIDs := make(map[uint64]string, len(parents))
	for _, parent := range parents {
		publicIDs[parent.ID] = dereference(parent.PublicID)
	}
	for index := range jobs {
		jobs[index].ParentPublicID = publicIDs[jobs[index].ParentJobID]
	}
	return nil
}

func optionalUint64(value uint64) *uint64 {
	if value == 0 {
		return nil
	}
	return &value
}

func dereferenceUint64(value *uint64) uint64 {
	if value == nil {
		return 0
	}
	return *value
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
