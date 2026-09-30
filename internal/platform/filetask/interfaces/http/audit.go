package filetaskhttp

import (
	"context"
	"log/slog"

	"gorm.io/gorm"
)

// writeFileAccessAudit 落一条 file_access_audit 网关访问审计记录，供 v2 会话路径与
// 仍在路由表中的 v1 上传/下载共同复用（AUD-2026-022）。审计写入失败不阻断主流程，
// 但错误绝不允许被空白标识符静默丢弃（AUD-2026-021，对齐平台 SEC-D4a 口径）：必须
// 输出带可告警字段的结构化 error 日志，供运维检测并回填缺失的审计记录。
func writeFileAccessAudit(ctx context.Context, db *gorm.DB, logger *slog.Logger, record accessAudit) {
	if err := db.WithContext(ctx).Create(&record).Error; err != nil {
		logger.Error("file access audit write failed",
			"error", err,
			"tenant_id", record.TenantID,
			"application_id", record.ApplicationID,
			"file_id", record.FileID,
			"action", record.Action,
			"result", record.Result)
	}
}
