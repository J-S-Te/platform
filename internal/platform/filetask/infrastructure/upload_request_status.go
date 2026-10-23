package infrastructure

import (
	"context"
	"errors"

	"github.com/J-S-Te/Basic-Platform/internal/platform/filetask/application"
	"gorm.io/gorm"
)

// LookupUploadRequest 使用同一快照和全部三个归属条件。
// 文件或版本缺失、损坏时必须明确报冲突，不能据此认定
// 之前已提交的请求不存在并允许使用新键替换。
func (repository *Repository) LookupUploadRequest(ctx context.Context, tenantID, applicationID, requestID string) (string, string, error) {
	if tenantID == "" || applicationID == "" || requestID == "" {
		return "", "", application.ErrForbidden
	}
	var row struct{ FileID, SessionStatus, FileStatus, VersionStatus string }
	result := repository.database.WithContext(ctx).Table("file_upload_session AS s").Select("s.file_id, s.status AS session_status, f.status AS file_status, v.status AS version_status").
		Joins("LEFT JOIN file_object AS f ON f.id = s.file_id AND f.tenant_id = s.tenant_id AND f.application_id = s.application_id").
		Joins("LEFT JOIN file_version AS v ON v.id = s.version_id AND v.file_id = s.file_id").
		Where("s.tenant_id = ? AND s.application_id = ? AND s.request_id = ?", tenantID, applicationID, requestID).Limit(1).Scan(&row)
	if result.Error != nil {
		return "", "", mapError(result.Error)
	}
	if result.RowsAffected == 0 {
		var guard uploadRequestGuard
		err := repository.database.WithContext(ctx).Where("tenant_id = ? AND application_id = ? AND request_id = ? AND retired = ?", tenantID, applicationID, requestID, true).Take(&guard).Error
		if err == nil {
			return "", "CANCELLED", nil
		}
		if !errors.Is(err, gorm.ErrRecordNotFound) {
			return "", "", mapError(err)
		}
		return "", "", application.ErrNotFound
	}
	if row.FileID == "" || row.FileStatus == "" || row.VersionStatus == "" {
		return "", "", application.ErrConflict
	}
	switch {
	case row.FileStatus == "REJECTED" && row.VersionStatus == "REJECTED" && row.SessionStatus == "FAILED":
		return row.FileID, "REJECTED", nil
	case row.FileStatus == "FAILED" && row.VersionStatus == "FAILED" && row.SessionStatus == "FAILED":
		return row.FileID, "FAILED", nil
	case row.FileStatus == "READY" && row.VersionStatus == "READY" && row.SessionStatus == "READY":
		return row.FileID, "READY", nil
	case row.SessionStatus == "WRITING" && (row.FileStatus == "PENDING_UPLOAD" || row.FileStatus == "VALIDATING") && (row.VersionStatus == "PENDING_UPLOAD" || row.VersionStatus == "VALIDATING"):
		return row.FileID, "WRITING", nil
	default:
		return "", "", application.ErrConflict
	}
}
