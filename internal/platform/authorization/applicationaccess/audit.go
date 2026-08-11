package applicationaccess

import (
	"context"
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
	// Authorization mutations have already committed when this hook runs. Audit persistence is
	// deliberately best-effort so a temporary audit outage cannot turn a successful authorization
	// change into a misleading failed response.
	_ = s.audit.RecordApplicationAccessAudit(ctx, event)
}

func sameValidity(left, right *time.Time) bool {
	if left == nil || right == nil {
		return left == nil && right == nil
	}
	return left.Equal(*right)
}

func sameStringSlice(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}
