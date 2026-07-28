package bootstrap

import (
	"context"
	"time"

	auditapplication "github.com/J-S-Te/Basic-Platform/backend/internal/platform/audit/application"
	"github.com/J-S-Te/Basic-Platform/backend/internal/shared/config"
	"github.com/J-S-Te/Basic-Platform/backend/internal/shared/ulid"
)

// contractAccessAuditAdapter records protected SYS-004 catalog mismatches through the
// platform's append-only audit ingestion path. It is intentionally independent of the
// removed audit-operations UI modules.
type contractAccessAuditAdapter struct {
	service *auditapplication.Service
	config  config.AuditConfig
	ids     ulid.Generator
}

func (adapter *contractAccessAuditAdapter) RecordContractRoleIntegrityFailure(
	ctx context.Context,
	tenantID string,
	applicationID string,
	expectedHash string,
	actual string,
) error {
	now := time.Now().UTC()
	eventID, err := adapter.ids.New(now)
	if err != nil {
		return err
	}
	_, err = adapter.service.Ingest(ctx, tenantID, auditapplication.EventInput{
		EventID:         eventID,
		ApplicationCode: adapter.config.ApplicationCode,
		EnvironmentCode: adapter.config.EnvironmentCode,
		ActorType:       "SYSTEM",
		OccurredAt:      now,
		Action:          "authorization.contract_role_config.integrity_failed",
		ResourceType:    "authorization_role_catalog",
		ResourceID:      applicationID,
		Result:          "DENIED",
		RiskLevel:       "CRITICAL",
		Classification:  "CONFIDENTIAL",
		Summary:         "合同系统预置角色或权限配置完整性校验失败，已拒绝签发令牌",
		Metadata: map[string]any{
			"application_code": "contract_management",
			"expected_hash":    expectedHash,
			"actual":           actual,
		},
		EventCategory: "SECURITY",
		EventType:     "CONTRACT_ROLE_CONFIG_INTEGRITY_FAILURE",
	})
	return err
}
