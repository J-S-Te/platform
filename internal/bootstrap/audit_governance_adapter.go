package bootstrap

import (
	"context"
	"strings"

	auditapplication "github.com/J-S-Te/Basic-Platform/internal/platform/audit/application"
	"github.com/J-S-Te/Basic-Platform/internal/shared/config"
)

// governanceAuditAdapter 让后台留存动作复用平台追加式审计入口。即使交互式治理页面未暴露，
// Worker 的清理行为仍必须留下与其他平台事件一致的来源、结果和风险记录。
type governanceAuditAdapter struct {
	service *auditapplication.Service
	config  config.AuditConfig
}

func (adapter *governanceAuditAdapter) RecordGovernanceAudit(ctx context.Context, record auditapplication.AuditRecord) error {
	applicationCode := strings.TrimSpace(record.ApplicationCode)
	if applicationCode == "" {
		applicationCode = adapter.config.ApplicationCode
	}
	environmentCode := strings.TrimSpace(record.EnvironmentCode)
	if environmentCode == "" {
		environmentCode = adapter.config.EnvironmentCode
	}
	_, err := adapter.service.Ingest(ctx, record.TenantID, auditapplication.EventInput{
		EventID:         record.EventID,
		ApplicationCode: applicationCode,
		EnvironmentCode: environmentCode,
		ActorType:       "USER",
		ActorID:         record.ActorID,
		ActorName:       record.ActorName,
		OccurredAt:      record.OccurredAt,
		Action:          record.Action,
		ResourceType:    record.ResourceType,
		ResourceID:      record.ResourceID,
		Result:          record.Result,
		RiskLevel:       record.RiskLevel,
		Classification:  "INTERNAL",
		Summary:         record.Summary,
		Metadata:        record.Metadata,
		EventCategory:   "PLATFORM_GOVERNANCE",
		EventType:       record.Action,
	})
	return err
}
