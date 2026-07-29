package bootstrap

import (
	"context"
	"strings"
	"time"

	auditapplication "github.com/J-S-Te/Basic-Platform/backend/internal/platform/audit/application"
	applicationaccess "github.com/J-S-Te/Basic-Platform/backend/internal/platform/authorization/applicationaccess"
	"github.com/J-S-Te/Basic-Platform/backend/internal/shared/config"
	"github.com/J-S-Te/Basic-Platform/backend/internal/shared/ulid"
)

// applicationAccessAuditAdapter maps generic application authorization events to the platform's
// append-only audit application. The configured platform application is the audit event source;
// the target application and subject are retained as resource metadata.
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
	for key, value := range event.Metadata {
		metadata[key] = value
	}
	metadata["target_application_id"] = event.ApplicationID
	metadata["target_application_code"] = event.ApplicationCode
	if strings.TrimSpace(event.SubjectID) != "" {
		metadata["subject_user_id"] = event.SubjectID
	}

	actorType := "SYSTEM"
	if strings.TrimSpace(event.OperatorID) != "" {
		actorType = "USER"
	}
	_, err = adapter.service.Ingest(ctx, event.TenantID, auditapplication.EventInput{
		EventID:         eventID,
		ApplicationCode: adapter.config.ApplicationCode,
		EnvironmentCode: adapter.config.EnvironmentCode,
		ActorType:       actorType,
		ActorID:         event.OperatorID,
		OccurredAt:      now,
		Action:          event.Action,
		ResourceType:    event.ResourceType,
		ResourceID:      event.ResourceID,
		Result:          event.Result,
		RiskLevel:       event.RiskLevel,
		Classification:  "INTERNAL",
		Summary:         event.Summary,
		Metadata:        metadata,
		EventCategory:   "PLATFORM_AUTHORIZATION",
		EventType:       event.Action,
	})
	return err
}
