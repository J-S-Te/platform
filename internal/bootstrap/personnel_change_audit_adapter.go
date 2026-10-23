package bootstrap

import (
	"context"
	"errors"
	"strings"
	"time"

	auditapplication "github.com/J-S-Te/Basic-Platform/internal/platform/audit/application"
	identityapplication "github.com/J-S-Te/Basic-Platform/internal/platform/identity/application"
	"github.com/J-S-Te/Basic-Platform/internal/shared/config"
	"github.com/J-S-Te/Basic-Platform/internal/shared/ulid"
)

// personnelChangeAuditAdapter 把身份模块的超管例外审批事件映射到平台审计接收端。
// 它只做字段翻译，不改变审批判定；例外是否成立始终由身份应用服务决定。
type personnelChangeAuditAdapter struct {
	service *auditapplication.Service
	config  config.AuditConfig
	ids     ulid.Generator
}

// RecordApprovalException 把一次超管例外审批写入平台审计管线；适配器未配置
// 时返回错误，不静默丢弃审计事件。
func (adapter *personnelChangeAuditAdapter) RecordApprovalException(ctx context.Context, event identityapplication.PersonnelChangeAuditEvent) error {
	if adapter == nil || adapter.service == nil {
		return errors.New("personnel change audit adapter is not configured")
	}
	eventID, err := personnelChangeAuditEventID(adapter.ids, event)
	if err != nil {
		return err
	}
	_, err = adapter.service.Ingest(ctx, event.TenantID, auditapplication.EventInput{
		EventID:         eventID,
		ApplicationCode: adapter.config.ApplicationCode,
		EnvironmentCode: adapter.config.EnvironmentCode,
		ActorType:       "USER",
		ActorID:         event.OperatorID,
		OccurredAt:      event.OccurredAt,
		Action:          "APPROVE",
		ResourceType:    "PERSONNEL_CHANGE",
		ResourceID:      event.RequestID,
		Result:          "SUCCESS",
		ReasonCode:      event.RuleHit,
		RiskLevel:       "HIGH",
		Classification:  "INTERNAL",
		Summary:         "超管例外审批人员异动（审批人与申请人或异动本人相同）",
		EventCategory:   "AUTHORIZATION_CHANGE",
		EventType:       "PERSONNEL_CHANGE_APPROVAL_EXCEPTION",
		Metadata: map[string]any{
			"rule_hit":           event.RuleHit,
			"exception_reason":   event.Reason,
			"to_status":          event.ToStatus,
			"submitted_by":       event.SubmittedBy,
			"subject_user_id":    event.SubjectUserID,
			"approval_reference": event.ApprovalReference,
		},
	})
	return err
}

// personnelChangeAuditEventID 复用 outbox 写入时生成的稳定事件 ID；缺失时（例如旧调用方）
// 才生成新 ID。重试必须使用同一 event_id，审计接收端才能按键幂等去重而不是重复记账。
func personnelChangeAuditEventID(ids ulid.Generator, event identityapplication.PersonnelChangeAuditEvent) (string, error) {
	if eventID := strings.TrimSpace(event.EventID); eventID != "" {
		return eventID, nil
	}
	return ids.New(time.Now().UTC())
}
