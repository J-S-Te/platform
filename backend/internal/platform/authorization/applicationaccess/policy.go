package applicationaccess

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

const maxAuthorizationPolicyEffectiveRoles = 65535

// CatalogPolicyInput 来自经过认证的应用目录同步负载，约束所有来源合并后的有效角色数；
// 零值明确表示应用不限制角色数量，而不是“未配置即拒绝”。
type CatalogPolicyInput struct {
	MaxEffectiveRoles int `json:"max_effective_roles,omitempty"`
}

// ApplicationAuthorizationPolicy is the persisted, application-owned policy resolved
// by authorization code. Its provenance is exposed so callers can distinguish a
// synchronized catalog constraint from a platform-side assignment decision.
type ApplicationAuthorizationPolicy struct {
	ApplicationID     string     `json:"application_id"`
	MaxEffectiveRoles int        `json:"max_effective_roles"`
	SourceType        string     `json:"source_type"`
	SourceIdentifier  string     `json:"source_identifier"`
	CatalogVersion    string     `json:"catalog_version"`
	CatalogHash       string     `json:"catalog_hash"`
	LastSyncedAt      *time.Time `json:"last_synced_at,omitempty"`
	LastSyncedBy      string     `json:"last_synced_by,omitempty"`
}

type applicationAuthorizationPolicyRow struct {
	TenantID          string     `gorm:"column:tenant_id"`
	ApplicationID     string     `gorm:"column:application_id"`
	MaxEffectiveRoles int        `gorm:"column:max_effective_roles"`
	SourceType        string     `gorm:"column:source_type"`
	SourceIdentifier  string     `gorm:"column:source_identifier"`
	CatalogVersion    string     `gorm:"column:catalog_version"`
	CatalogHash       string     `gorm:"column:catalog_hash"`
	LastSyncedAt      *time.Time `gorm:"column:last_synced_at"`
	LastSyncedBy      string     `gorm:"column:last_synced_by"`
}

// ResolveApplicationAuthorizationPolicy 以已解析的应用 ID 读取目录策略，防止同名或编码
// 别名混淆授权目标。缺少策略行按兼容默认值处理：max_effective_roles=0，不限制角色数。
// 只有正数限制且去重后的有效角色超过限制时，授权路径才应拒绝。
func (s *Service) ResolveApplicationAuthorizationPolicy(ctx context.Context, tenantID, applicationID string) (ApplicationAuthorizationPolicy, error) {
	policy := ApplicationAuthorizationPolicy{
		ApplicationID:     strings.TrimSpace(applicationID),
		MaxEffectiveRoles: 0,
	}
	if strings.TrimSpace(tenantID) == "" || policy.ApplicationID == "" {
		return ApplicationAuthorizationPolicy{}, validation("tenant_id and application_id are required")
	}

	var row applicationAuthorizationPolicyRow
	err := s.db.WithContext(ctx).Table("authz_application_authorization_policy").
		Where("tenant_id = ? AND application_id = ?", strings.TrimSpace(tenantID), policy.ApplicationID).
		Take(&row).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return policy, nil
	}
	if err != nil {
		return ApplicationAuthorizationPolicy{}, fmt.Errorf("load application authorization policy: %w", err)
	}
	return applicationAuthorizationPolicyFromRow(row), nil
}

func normalizeCatalogPolicy(input CatalogPolicyInput) (CatalogPolicyInput, error) {
	if input.MaxEffectiveRoles < 0 || input.MaxEffectiveRoles > maxAuthorizationPolicyEffectiveRoles {
		return CatalogPolicyInput{}, validation(fmt.Sprintf("max_effective_roles must be between 0 and %d", maxAuthorizationPolicyEffectiveRoles))
	}
	return input, nil
}

func applicationAuthorizationPolicyFromRow(row applicationAuthorizationPolicyRow) ApplicationAuthorizationPolicy {
	return ApplicationAuthorizationPolicy{
		ApplicationID:     row.ApplicationID,
		MaxEffectiveRoles: row.MaxEffectiveRoles,
		SourceType:        row.SourceType,
		SourceIdentifier:  row.SourceIdentifier,
		CatalogVersion:    row.CatalogVersion,
		CatalogHash:       row.CatalogHash,
		LastSyncedAt:      row.LastSyncedAt,
		LastSyncedBy:      row.LastSyncedBy,
	}
}

// upsertCatalogPolicy 在每次成功目录同步时覆盖完整策略。新目录省略限制会显式重置为 0，
// 防止上一目录版本的旧约束在目录升级后残留。
func (s *Service) upsertCatalogPolicy(tx *gorm.DB, tenantID, applicationID, operatorID string, now time.Time, input CatalogInput) error {
	values := map[string]any{
		"tenant_id":           tenantID,
		"application_id":      applicationID,
		"max_effective_roles": input.Policy.MaxEffectiveRoles,
		"source_type":         input.SourceType,
		"source_identifier":   input.SourceIdentifier,
		"catalog_version":     input.CatalogVersion,
		"catalog_hash":        input.Checksum,
		"last_synced_at":      now,
		"last_synced_by":      operatorID,
		"created_at":          now,
		"created_by":          operatorID,
		"updated_at":          now,
		"updated_by":          operatorID,
	}
	if err := tx.Table("authz_application_authorization_policy").Clauses(clause.OnConflict{
		Columns: []clause.Column{{Name: "tenant_id"}, {Name: "application_id"}},
		DoUpdates: clause.AssignmentColumns([]string{
			"max_effective_roles", "source_type", "source_identifier", "catalog_version", "catalog_hash",
			"last_synced_at", "last_synced_by", "updated_at", "updated_by",
		}),
	}).Create(values).Error; err != nil {
		return fmt.Errorf("save application authorization policy: %w", err)
	}
	return nil
}
