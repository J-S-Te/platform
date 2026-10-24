package positiongrant

import (
	"fmt"
	"time"

	"gorm.io/gorm"
)

func generatedBindingMatches(binding bindingProjection, item templateRoleModel, positionID string) bool {
	return binding.ApplicationID == item.ApplicationID && binding.RoleID == item.RoleID && binding.SubjectType == subjectPosition && binding.SubjectID == positionID && binding.ScopeType == item.ScopeType && binding.ScopeID == item.ScopeID
}

// 曾经可编辑的模板绑定可能已转移到其他主体。把原主体也加入待物化队列，使修复来源的
// 同时撤销其在 Keycloak 上的过期投影。所有写操作都保留在调用方事务内。
func (s *Service) enqueueDisplacedSubject(tx *gorm.DB, tenantID string, binding bindingProjection, now time.Time) error {
	var environmentIDs []string
	if err := tx.Table("keycloak_application_client_mapping").Where("tenant_id=? AND application_id=? AND status=?", tenantID, binding.ApplicationID, "SYNCED").Pluck("environment_id", &environmentIDs).Error; err != nil {
		return fmt.Errorf("load displaced binding projection targets: %w", err)
	}
	if len(environmentIDs) == 0 {
		return nil
	}
	var users []string
	switch binding.SubjectType {
	case "USER":
		users = []string{binding.SubjectID}
	case "ACCOUNT":
		if err := tx.Table("iam_account").Where("tenant_id=? AND id=?", tenantID, binding.SubjectID).Pluck("user_id", &users).Error; err != nil {
			return fmt.Errorf("load displaced account owner: %w", err)
		}
	case "ORG_UNIT", subjectPosition:
		column := "org_unit_id"
		if binding.SubjectType == subjectPosition {
			column = "position_id"
		}
		if err := tx.Table("iam_membership").Distinct("user_id").Where("tenant_id=? AND "+column+"=? AND status=? AND inherit_authorization=?", tenantID, binding.SubjectID, activeStatus, true).Where("(valid_from IS NULL OR valid_from<=?) AND (valid_until IS NULL OR valid_until>?)", now, now).Pluck("user_id", &users).Error; err != nil {
			return fmt.Errorf("load displaced binding members: %w", err)
		}
	}
	for _, userID := range stringSetKeys(mapFromStrings(users)) {
		if userID == "" {
			continue
		}
		for _, envID := range stringSetKeys(mapFromStrings(environmentIDs)) {
			id, err := s.ids.New(now)
			if err != nil {
				return err
			}
			if err := tx.Table("keycloak_authorization_outbox").Create(map[string]any{"id": id, "tenant_id": tenantID, "identity_id": userID, "application_id": binding.ApplicationID, "environment_id": envID, "event_type": "AUTHORIZATION_CHANGED", "authorization_revision": 0, "status": "PENDING", "available_at": now, "attempts": 0, "created_at": now}).Error; err != nil {
				return fmt.Errorf("enqueue displaced subject authorization: %w", err)
			}
		}
	}
	return nil
}
