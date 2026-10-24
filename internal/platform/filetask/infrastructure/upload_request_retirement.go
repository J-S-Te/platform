package infrastructure

import (
	"context"
	"errors"
	"time"

	"github.com/J-S-Te/Basic-Platform/internal/platform/filetask/application"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

var errUploadRequestRetired = errors.New("upload request is retired")

type uploadRequestGuard struct {
	TenantID             string `gorm:"primaryKey"`
	ApplicationID        string `gorm:"primaryKey"`
	RequestID            string `gorm:"primaryKey"`
	Retired              bool
	CreatedAt, UpdatedAt time.Time
}

func (uploadRequestGuard) TableName() string { return "file_upload_request_guard" }

// ReserveUpload 与退役操作都持有此唯一请求行锁直至提交。
// 即使请求尚无会话或文件，退役操作也会与其串行执行。
func lockUploadRequest(tx *gorm.DB, tenantID, appID, requestID string) (uploadRequestGuard, error) {
	now := time.Now().UTC()
	guard := uploadRequestGuard{TenantID: tenantID, ApplicationID: appID, RequestID: requestID, CreatedAt: now, UpdatedAt: now}
	if err := tx.Clauses(clause.OnConflict{DoNothing: true}).Create(&guard).Error; err != nil {
		return guard, err
	}
	err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where("tenant_id = ? AND application_id = ? AND request_id = ?", tenantID, appID, requestID).Take(&guard).Error
	return guard, err
}

func (repository *Repository) RetireUploadRequest(ctx context.Context, tenantID, appID, requestID string) (string, string, error) {
	if tenantID == "" || appID == "" || requestID == "" {
		return "", "", application.ErrForbidden
	}
	var fileID, status string
	err := repository.database.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if _, err := lockUploadRequest(tx, tenantID, appID, requestID); err != nil {
			return err
		}
		if err := tx.Model(&uploadRequestGuard{}).Where("tenant_id = ? AND application_id = ? AND request_id = ?", tenantID, appID, requestID).Updates(map[string]any{"retired": true, "updated_at": time.Now().UTC()}).Error; err != nil {
			return err
		}
		// 退休只确认稳定会话身份，不声明字节可用。受控清理可把文件/版本
		// 推进到 DELETED/REMOVED，但不能因此阻止提交请求墓碑。
		var session fileUploadSessionModel
		err := tx.Where("tenant_id = ? AND application_id = ? AND request_id = ?", tenantID, appID, requestID).Take(&session).Error
		if errors.Is(err, gorm.ErrRecordNotFound) {
			fileID, status = "", "CANCELLED"
			return nil
		}
		if err != nil {
			return err
		}
		if session.FileID == nil || session.VersionID == nil || *session.FileID == "" || *session.VersionID == "" {
			return application.ErrConflict
		}
		switch session.Status {
		case "WRITING", "READY", "FAILED", "REJECTED":
		default:
			return application.ErrConflict
		}
		// 元数据可以不可读，但归属链仍必须完整：不能借损坏的会话指针
		// 将其他租户/应用的文件身份返回给调用方。
		var object fileObjectModel
		if err := tx.Select("id").Where("tenant_id = ? AND application_id = ? AND id = ?", tenantID, appID, *session.FileID).Take(&object).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return application.ErrConflict
			}
			return err
		}
		var version fileVersionModel
		if err := tx.Select("id").Where("id = ? AND file_id = ?", *session.VersionID, *session.FileID).Take(&version).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return application.ErrConflict
			}
			return err
		}
		fileID, status = *session.FileID, session.Status
		return nil
	})
	return fileID, status, mapError(err)
}
