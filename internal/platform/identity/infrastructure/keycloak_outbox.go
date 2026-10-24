package infrastructure

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/J-S-Te/Basic-Platform/internal/shared/ulid"
	"gorm.io/gorm"
)

// keycloakProjectionTarget 有意从 Keycloak 应用映射读取。outbox schema 没有租户级目标，
// worker 只为已映射应用重算快照；把该查询保留在这里，身份写入就不必猜测应用归属或复制
// 投影数据。
type keycloakProjectionTarget struct {
	ApplicationID string `gorm:"column:application_id"`
	EnvironmentID string `gorm:"column:environment_id"`
}

// enqueueKeycloakIdentityEvents 在调用方事务中记录一条重算请求。目标缺失是有意行为：
// 该租户没有已同步的 Keycloak 应用映射；一旦应用完成映射，新身份即被纳入，无需预先存在
// 投影输出行。
func enqueueKeycloakIdentityEvents(transaction *gorm.DB, tenantID string, identityIDs []string, occurredAt time.Time, eventType string) error {
	identityIDs = uniqueIdentityIDs(identityIDs)
	if len(identityIDs) == 0 {
		return nil
	}

	var targets []keycloakProjectionTarget
	if err := keycloakProjectionTargetsQuery(transaction, tenantID).Find(&targets).Error; err != nil {
		return fmt.Errorf("find Keycloak projection targets: %w", err)
	}
	for _, identityID := range identityIDs {
		for _, target := range targets {
			id, err := (ulid.Generator{}).New(occurredAt.UTC())
			if err != nil {
				return fmt.Errorf("generate Keycloak identity outbox ID: %w", err)
			}
			if err := transaction.Table("keycloak_authorization_outbox").Create(map[string]any{
				"id": id, "tenant_id": tenantID, "identity_id": identityID,
				"application_id": target.ApplicationID, "environment_id": target.EnvironmentID, "event_type": eventType,
				"authorization_revision": 0, "status": "PENDING", "available_at": occurredAt.UTC(),
				"attempts": 0, "created_at": occurredAt.UTC(),
			}).Error; err != nil {
				return fmt.Errorf("enqueue Keycloak identity event: %w", err)
			}
		}
	}
	return nil
}

func keycloakProjectionTargetsQuery(transaction *gorm.DB, tenantID string) *gorm.DB {
	return transaction.Table("keycloak_application_client_mapping").
		Select("application_id, environment_id").
		Where("tenant_id = ? AND status = ?", tenantID, "SYNCED")
}

func uniqueIdentityIDs(identityIDs []string) []string {
	seen := make(map[string]struct{}, len(identityIDs))
	for _, identityID := range identityIDs {
		if identityID = strings.TrimSpace(identityID); identityID != "" {
			seen[identityID] = struct{}{}
		}
	}
	result := make([]string, 0, len(seen))
	for identityID := range seen {
		result = append(result, identityID)
	}
	sort.Strings(result)
	return result
}

func membershipIdentityIDs(transaction *gorm.DB, tenantID string, query *gorm.DB) ([]string, error) {
	var identityIDs []string
	if err := query.Where("tenant_id = ? AND status = ?", tenantID, "ACTIVE").
		Distinct().Pluck("user_id", &identityIDs).Error; err != nil {
		return nil, fmt.Errorf("find affected membership identities: %w", err)
	}
	return identityIDs, nil
}
