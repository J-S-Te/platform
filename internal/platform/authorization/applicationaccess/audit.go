package applicationaccess

import (
	"context"
	"log/slog"
	"time"
)

// AuditEvent 描述一次已成功的应用授权操作业务事件。事件由 applicationaccess
// 服务负责产生；如何落库、落到哪里由 bootstrap 适配器决定。
type AuditEvent struct {
	TenantID        string
	ApplicationID   string
	ApplicationCode string
	OperatorID      string
	OperatorName    string
	SubjectID       string
	Action          string
	ResourceType    string
	ResourceID      string
	Result          string
	RiskLevel       string
	Summary         string
	OccurredAt      time.Time
	Metadata        map[string]any
	Changes         []AuditFieldChange
}

// AuditFieldChange 捕获一次业务字段变更的 before/after 值，且保证不泄露密钥类内容。
type AuditFieldChange struct {
	Field  string
	Before any
	After  any
}

// AuditRecorder 为可选依赖，使授权服务在未安装平台审计管线的进程
// （如聚焦测试、维护工具）中依然可用。
type AuditRecorder interface {
	RecordApplicationAccessAudit(context.Context, AuditEvent) error
}

func (s *Service) recordAudit(ctx context.Context, event AuditEvent) {
	if s.audit == nil {
		return
	}
	if event.OccurredAt.IsZero() {
		event.OccurredAt = s.clock.Now().UTC()
	}
	// 本钩子执行时授权变更已提交。审计持久化保持尽力而为，避免审计管道临时故障
	// 把一次成功的授权变更误报为失败——但错误绝不允许再被空白标识符静默丢弃（安全审查 SEC-D4a）：
	// 摄取失败必须输出带可告警字段的结构化 error 日志，供运维检测并回填缺失的业务审计记录；
	// 通用审计轨迹（AuditTrail 中间件）在同一路由始终落库，构成失败时的兜底记录。
	if err := s.audit.RecordApplicationAccessAudit(ctx, event); err != nil {
		slog.Error("record application access audit",
			"error", err,
			"tenant_id", event.TenantID,
			"application_code", event.ApplicationCode,
			"action", event.Action,
			"operator_id", event.OperatorID,
			"resource_id", event.ResourceID,
			"result", event.Result)
	}
}

func sameValidity(left, right *time.Time) bool {
	if left == nil || right == nil {
		return left == nil && right == nil
	}
	return left.Equal(*right)
}
