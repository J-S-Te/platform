package bootstrap

import (
	"context"
	"strings"
	"time"

	auditapplication "github.com/J-S-Te/Basic-Platform/internal/platform/audit/application"
	auditdomain "github.com/J-S-Te/Basic-Platform/internal/platform/audit/domain"
	applicationaccess "github.com/J-S-Te/Basic-Platform/internal/platform/authorization/applicationaccess"
	"github.com/J-S-Te/Basic-Platform/internal/shared/config"
	"github.com/J-S-Te/Basic-Platform/internal/shared/ulid"
)

// applicationAccessAuditAdapter 把通用应用授权事件映射到平台追加式审计模型。事件来源固定为
// 平台自身，目标应用和被授权用户保留在资源元数据中，避免把“执行者系统”和“授权目标”混淆。
type applicationAccessAuditAdapter struct {
	service *auditapplication.Service
	config  config.AuditConfig
	ids     ulid.Generator
}

func (adapter *applicationAccessAuditAdapter) RecordApplicationAccessAudit(
	ctx context.Context,
	event applicationaccess.AuditEvent,
) error {
	now := event.OccurredAt.UTC()
	if now.IsZero() {
		now = time.Now().UTC()
	}
	eventID, err := adapter.ids.New(now)
	if err != nil {
		return err
	}

	metadata := make(map[string]any, len(event.Metadata)+4)
	// 复制调用方 map，适配器补充审计字段时不能反向修改业务事件或引发并发 map 写入。
	for key, value := range event.Metadata {
		metadata[key] = value
	}
	metadata["target_application_id"] = event.ApplicationID
	metadata["target_application_code"] = event.ApplicationCode
	if strings.TrimSpace(event.SubjectID) != "" {
		metadata["subject_user_id"] = event.SubjectID
	}

	actorType := "SYSTEM"
	// 空 OperatorID 表示后台补偿或系统任务；有操作者时才记录为 USER，不能伪造占位用户。
	if strings.TrimSpace(event.OperatorID) != "" {
		actorType = "USER"
	}
	changes := make([]auditdomain.FieldChange, 0, len(event.Changes))
	for _, change := range event.Changes {
		changes = append(changes, auditdomain.FieldChange{Field: change.Field, Before: change.Before, After: change.After})
	}
	_, err = adapter.service.Ingest(ctx, event.TenantID, auditapplication.EventInput{
		EventID:         eventID,
		ApplicationCode: adapter.config.ApplicationCode,
		EnvironmentCode: adapter.config.EnvironmentCode,
		ActorType:       actorType,
		ActorID:         event.OperatorID,
		ActorName:       event.OperatorName,
		OccurredAt:      now,
		Action:          event.Action,
		ResourceType:    event.ResourceType,
		ResourceID:      event.ResourceID,
		Result:          event.Result,
		RiskLevel:       event.RiskLevel,
		Classification:  "INTERNAL",
		Summary:         event.Summary,
		Metadata:        metadata,
		Changes:         changes,
		EventCategory:   "PLATFORM_AUTHORIZATION",
		EventType:       event.Action,
	})
	return err
}
