package filetaskhttp

import (
	"context"
	"log/slog"
	"strings"

	"gorm.io/gorm"
)

// file_access_audit 的列宽（见 migrations/000103_local_gateway_v2.sql）。这些列是
// ascii 字符集：MySQL 严格模式下越界或非 ASCII 值会分别报 1406/1366，审计行被丢弃
// 且既有语义要求不阻断主流程——等于给持 platform:file:* 的机器客户端留下一个
// "用超长请求头让文件操作无审计"的开关。因此写入前必须把每个字段收敛到列宽内。
const (
	auditTenantIDMaxLength              = 26
	auditApplicationIDMaxLength         = 26
	auditFileIDMaxLength                = 26
	auditActorUserIDMaxLength           = 26
	auditAuthenticatedClientIDMaxLength = 128
	auditActionMaxLength                = 32
	auditResultMaxLength                = 16
	auditRequestIDMaxLength             = 128
)

// writeFileAccessAudit 落一条 file_access_audit 网关访问审计记录，供 v2 会话路径与
// 仍在路由表中的 v1 上传/下载共同复用（AUD-2026-022）。审计写入失败不阻断主流程，
// 但错误绝不允许被空白标识符静默丢弃（AUD-2026-021，对齐平台 SEC-D4a 口径）：必须
// 输出带可告警字段的结构化 error 日志，供运维检测并回填缺失的审计记录。
func writeFileAccessAudit(ctx context.Context, db *gorm.DB, logger *slog.Logger, record accessAudit) {
	record = normalizeAccessAudit(record, logger)
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

// normalizeAccessAudit 在 INSERT 前把所有字符串字段收敛到列宽：超长截断、空串保持、
// actor 为空保持 NULL。收敛只影响落库形态，不改变"审计失败不阻断主流程"的语义。
func normalizeAccessAudit(record accessAudit, logger *slog.Logger) accessAudit {
	record.TenantID = clampAuditField(logger, "tenant_id", record.TenantID, auditTenantIDMaxLength)
	record.ApplicationID = clampAuditField(logger, "application_id", record.ApplicationID, auditApplicationIDMaxLength)
	record.FileID = clampAuditField(logger, "file_id", record.FileID, auditFileIDMaxLength)
	if record.ActorUserID != nil {
		actor := clampAuditField(logger, "actor_user_id", *record.ActorUserID, auditActorUserIDMaxLength)
		record.ActorUserID = &actor
	}
	record.AuthenticatedClientID = clampAuditField(logger, "authenticated_client_id", record.AuthenticatedClientID, auditAuthenticatedClientIDMaxLength)
	record.Action = clampAuditField(logger, "action", record.Action, auditActionMaxLength)
	record.Result = clampAuditField(logger, "result", record.Result, auditResultMaxLength)
	if record.RequestID != nil {
		requestID := clampAuditField(logger, "request_id", *record.RequestID, auditRequestIDMaxLength)
		record.RequestID = &requestID
	}
	return record
}

// clampAuditField 把值收敛到 ascii 列宽：先把非 ASCII 与控制字符替换为 '?'，再按
// 字节截断。先 ASCII 化保证截断不会把多字节字符切成无效 UTF-8，也避免 ascii 列
// 因 1366 拒绝整行；ASCII 字符一字节一字符，字节截断等价于按字符宽度截断。
// 发生收敛时只记录字段名与长度，绝不记录原值，避免审计日志本身承载敏感内容。
func clampAuditField(logger *slog.Logger, field, value string, limit int) string {
	if value == "" || limit <= 0 {
		return value
	}
	sanitized := strings.Map(func(r rune) rune {
		if r >= 0x20 && r <= 0x7e {
			return r
		}
		return '?'
	}, value)
	nonASCII := sanitized != value
	truncated := len(sanitized) > limit
	if truncated {
		sanitized = sanitized[:limit]
	}
	if (nonASCII || truncated) && logger != nil {
		logger.Warn("file access audit field normalized before insert",
			"field", field,
			"original_length", len(value),
			"stored_length", len(sanitized),
			"truncated", truncated,
			"non_ascii_replaced", nonASCII)
	}
	return sanitized
}
