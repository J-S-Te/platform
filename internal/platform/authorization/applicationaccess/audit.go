package applicationaccess

import (
	"context"
	"log/slog"
	"time"
)

// AuditEvent describes a successful application authorization operation. The applicationaccess
// service owns the business event; the bootstrap adapter decides how and where it is persisted.
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

// AuditFieldChange captures a secret-safe before/after business change.
type AuditFieldChange struct {
	Field  string
	Before any
	After  any
}

// AuditRecorder is optional so the authorization service remains usable in processes that do not
// install the platform audit pipeline (for example, focused tests or maintenance tools).
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
	// Authorization mutations have already committed when this hook runs. Audit persistence stays
	// best-effort so a temporary audit outage cannot turn a successful authorization change into a
	// misleading failed response——但错误绝不允许再被空白标识符静默丢弃（安全审查 SEC-D4a）：
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
