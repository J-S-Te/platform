package bootstrap

import (
	"context"
	"strings"

	auditapplication "github.com/J-S-Te/Basic-Platform/backend/internal/platform/audit/application"
	"github.com/J-S-Te/Basic-Platform/backend/internal/shared/config"
)

// governanceAuditAdapter records background retention actions through the same append-only audit
// ingestion path used by the rest of the platform. It is retained for the worker even though the
// interactive audit-operations management module is no longer exposed.
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
