package applicationaccess

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// CatalogInput is the application-owned authorization manifest submitted to the platform.
// The platform stores the catalog in generic authorization tables; it does not assign business
// meaning to permission codes.
type CatalogInput struct {
	CatalogVersion string `json:"catalog_version"`
	Checksum       string `json:"checksum"`
	// SourceType and SourceIdentifier are provenance fields managed by the HTTP
	// transport from a verified application bearer; request values must not be trusted.
	SourceType       string             `json:"source_type"`
	SourceIdentifier string             `json:"source_identifier"`
	Permissions      []PermissionInput  `json:"permissions"`
	Roles            []CatalogRoleInput `json:"roles"`
	Policy           CatalogPolicyInput `json:"policy,omitempty"`
	// ClaimsRoleConfigHash is application-owned opaque Claims compatibility
	// metadata. The platform stores and forwards it, but does not derive it from
	// application-specific roles or permissions.
	ClaimsRoleConfigHash string `json:"claims_role_config_hash,omitempty"`
}

type PermissionInput struct {
	Code         string `json:"code"`
	Name         string `json:"name"`
	Action       string `json:"action"`
	ResourceCode string `json:"resource_code"`
	ResourceName string `json:"resource_name,omitempty"`
	RiskLevel    string `json:"risk_level,omitempty"`
}

type CatalogRoleInput struct {
	Code        string   `json:"code"`
	Name        string   `json:"name"`
	Description string   `json:"description,omitempty"`
	Permissions []string `json:"permissions"`
}

type CatalogView struct {
	ApplicationID        string                         `json:"application_id"`
	ApplicationCode      string                         `json:"application_code"`
	CatalogVersion       string                         `json:"catalog_version"`
	Checksum             string                         `json:"checksum"`
	SourceType           string                         `json:"source_type"`
	SourceIdentifier     string                         `json:"source_identifier"`
	SyncStatus           string                         `json:"sync_status"`
	LastSyncedAt         *time.Time                     `json:"last_synced_at,omitempty"`
	LastSyncedBy         string                         `json:"last_synced_by,omitempty"`
	ErrorMessage         string                         `json:"error_message,omitempty"`
	Policy               ApplicationAuthorizationPolicy `json:"policy"`
	ClaimsRoleConfigHash string                         `json:"claims_role_config_hash,omitempty"`
	Permissions          []CatalogPermissionView        `json:"permissions"`
	Roles                []CatalogRoleView              `json:"roles"`
}

type CatalogPermissionView struct {
	Code         string `json:"code"`
	Name         string `json:"name"`
	Action       string `json:"action"`
	ResourceCode string `json:"resource_code"`
	RiskLevel    string `json:"risk_level"`
	Status       string `json:"status"`
}

type CatalogRoleView struct {
	Code        string   `json:"code"`
	Name        string   `json:"name"`
	Description string   `json:"description,omitempty"`
	Status      string   `json:"status"`
	Permissions []string `json:"permissions"`
}

type catalogMetadataRow struct {
	TenantID             string     `gorm:"column:tenant_id"`
	ApplicationID        string     `gorm:"column:application_id"`
	CatalogVersion       string     `gorm:"column:catalog_version"`
	CatalogHash          string     `gorm:"column:catalog_hash"`
	ClaimsRoleConfigHash string     `gorm:"column:claims_role_config_hash"`
	SourceType           string     `gorm:"column:source_type"`
	SourceIdentifier     string     `gorm:"column:source_identifier"`
	SyncStatus           string     `gorm:"column:sync_status"`
	LastSyncedAt         *time.Time `gorm:"column:last_synced_at"`
	LastSyncedBy         string     `gorm:"column:last_synced_by"`
	ErrorMessage         string     `gorm:"column:error_message"`
}

type catalogPermissionRow struct {
	ID           string `gorm:"column:id"`
	Code         string `gorm:"column:code"`
	Name         string `gorm:"column:name"`
	Action       string `gorm:"column:action"`
	ResourceCode string `gorm:"column:resource_code"`
	RiskLevel    string `gorm:"column:risk_level"`
	Status       string `gorm:"column:status"`
}

type catalogRoleRow struct {
	ID          string `gorm:"column:id"`
	Code        string `gorm:"column:code"`
	Name        string `gorm:"column:name"`
	Description string `gorm:"column:description"`
	Status      string `gorm:"column:status"`
}

// GetCatalog returns the latest synchronized catalog and its platform-side status.
func (s *Service) GetCatalog(ctx context.Context, tenantID, applicationID string) (CatalogView, error) {
	app, err := s.findApplicationByID(ctx, tenantID, applicationID)
	if err != nil {
		return CatalogView{}, err
	}
	var metadata catalogMetadataRow
	err = s.db.WithContext(ctx).Table("authz_authorization_catalog").Where("tenant_id = ? AND application_id = ?", tenantID, app.ID).Take(&metadata).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		metadata = catalogMetadataRow{TenantID: tenantID, ApplicationID: app.ID, SyncStatus: "NOT_SYNCED"}
	} else if err != nil {
		return CatalogView{}, fmt.Errorf("load authorization catalog metadata: %w", err)
	}
	return s.catalogView(ctx, app, metadata)
}

// SyncCatalog validates and atomically replaces the application-owned role catalog.
// Every application, including contract_management, uses this application-owned mirror;
// contract-specific role-assignment and token-issuance guardrails live in Service access logic.
func (s *Service) SyncCatalog(ctx context.Context, tenantID, applicationID, operatorID string, input CatalogInput) (CatalogView, error) {
	app, err := s.findApplicationByID(ctx, tenantID, applicationID)
	if err != nil {
		return CatalogView{}, err
	}
	submittedInput := input
	input, checksum, err := normalizeCatalog(input)
	if err != nil {
		s.recordCatalogFailure(ctx, tenantID, app, operatorID, submittedInput, err)
		return CatalogView{}, err
	}
	if input.Checksum != "" {
		provided := strings.TrimSpace(strings.TrimPrefix(input.Checksum, "sha256:"))
		if !strings.EqualFold(provided, checksum) {
			validationErr := validation("checksum does not match catalog content")
			s.recordCatalogFailure(ctx, tenantID, app, operatorID, input, validationErr)
			return CatalogView{}, validationErr
		}
	}
	input.Checksum = "sha256:" + checksum
	if strings.TrimSpace(input.SourceType) == "" {
		input.SourceType = "API"
	}
	if strings.TrimSpace(input.SourceIdentifier) == "" {
		input.SourceIdentifier = app.Code
	}
	now := s.clock.Now().UTC()
	var existingMetadata catalogMetadataRow
	metadataErr := s.db.WithContext(ctx).Table("authz_authorization_catalog").Where("tenant_id = ? AND application_id = ?", tenantID, app.ID).Take(&existingMetadata).Error
	if metadataErr == nil && existingMetadata.SyncStatus == "SYNCED" && existingMetadata.CatalogVersion == input.CatalogVersion && strings.EqualFold(existingMetadata.CatalogHash, input.Checksum) {
		// 同版本同摘要视为幂等确认，不重写角色关系或推进 revision，避免应用重启后
		// 周期同步制造无意义的令牌失效和审计噪声。
		view, viewErr := s.catalogView(ctx, app, existingMetadata)
		if viewErr != nil {
			return CatalogView{}, viewErr
		}
		s.recordAudit(ctx, AuditEvent{
			TenantID: tenantID, ApplicationID: app.ID, ApplicationCode: app.Code, OperatorID: operatorID,
			Action: "authorization.application_catalog.synced", ResourceType: "authorization_catalog", ResourceID: app.ID,
			Result: "SUCCESS", RiskLevel: "HIGH", Summary: "应用授权目录已同步", OccurredAt: now,
			Metadata: map[string]any{"catalog_version": input.CatalogVersion, "checksum": input.Checksum, "source_type": input.SourceType, "source_identifier": input.SourceIdentifier, "idempotent": true},
		})
		return view, nil
	}
	if metadataErr != nil && !errors.Is(metadataErr, gorm.ErrRecordNotFound) {
		metadataLoadErr := fmt.Errorf("load existing authorization catalog metadata: %w", metadataErr)
		s.recordCatalogFailure(ctx, tenantID, app, operatorID, input, metadataLoadErr)
		return CatalogView{}, metadataLoadErr
	}
	if err := s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		// 目录、角色权限关系、应用策略、同步元数据和 revision 必须作为一个版本发布。
		// 任一环节失败都会回滚，令牌签发始终只能看到上一份完整目录或新目录。
		permissionIDs := make(map[string]string, len(input.Permissions))
		for _, item := range input.Permissions {
			resourceID, err := s.upsertCatalogResource(tx, tenantID, app.ID, operatorID, now, item)
			if err != nil {
				return err
			}
			permissionID, err := s.upsertCatalogPermission(tx, tenantID, app.ID, operatorID, now, item, resourceID)
			if err != nil {
				return err
			}
			permissionIDs[item.Code] = permissionID
		}
		roleCodes := make([]string, 0, len(input.Roles))
		for _, item := range input.Roles {
			roleID, err := s.upsertCatalogRole(tx, tenantID, app.ID, operatorID, now, item)
			if err != nil {
				return err
			}
			roleCodes = append(roleCodes, item.Code)
			if err := tx.Table("authz_role_permission").Where("role_id = ?", roleID).Delete(nil).Error; err != nil {
				return fmt.Errorf("replace role permissions for %s: %w", item.Code, err)
			}
			for _, code := range item.Permissions {
				permissionID, ok := permissionIDs[code]
				if !ok {
					return validation("role references unknown permission: " + code)
				}
				if err := tx.Table("authz_role_permission").Create(map[string]any{"role_id": roleID, "permission_id": permissionID, "effect": "ALLOW", "created_at": now, "created_by": operatorID}).Error; err != nil {
					return fmt.Errorf("map role %s permission %s: %w", item.Code, code, err)
				}
			}
		}
		if len(roleCodes) == 0 {
			roleCodes = []string{"__no_catalog_role__"}
		}
		if err := tx.Table("authz_role").Where("tenant_id = ? AND application_id = ? AND role_type <> ? AND code NOT IN ?", tenantID, app.ID, "COMPATIBILITY", roleCodes).Updates(map[string]any{"status": disabledStatus, "version": gorm.Expr("version + 1"), "updated_at": now, "updated_by": operatorID}).Error; err != nil {
			return fmt.Errorf("disable removed catalog roles: %w", err)
		}
		// 不物理删除目录中移除的角色/权限：历史绑定和审计仍需可追溯；置为 DISABLED
		// 后它们会立即退出授权查询，并可在后续同步中按稳定编码恢复。
		permissionCodes := make([]string, 0, len(input.Permissions))
		for _, item := range input.Permissions {
			permissionCodes = append(permissionCodes, item.Code)
		}
		if len(permissionCodes) == 0 {
			permissionCodes = []string{"__no_catalog_permission__"}
		}
		if err := tx.Table("authz_permission").Where("tenant_id = ? AND application_id = ? AND code NOT IN ?", tenantID, app.ID, permissionCodes).Updates(map[string]any{"status": disabledStatus, "version": gorm.Expr("version + 1"), "updated_at": now, "updated_by": operatorID}).Error; err != nil {
			return fmt.Errorf("disable removed catalog permissions: %w", err)
		}
		if err := s.upsertCatalogPolicy(tx, tenantID, app.ID, operatorID, now, input); err != nil {
			return err
		}
		metadata := map[string]any{"tenant_id": tenantID, "application_id": app.ID, "catalog_version": input.CatalogVersion, "catalog_hash": input.Checksum, "claims_role_config_hash": input.ClaimsRoleConfigHash, "source_type": input.SourceType, "source_identifier": input.SourceIdentifier, "sync_status": "SYNCED", "last_synced_at": now, "last_synced_by": operatorID, "error_message": nil, "created_at": now, "created_by": operatorID, "updated_at": now, "updated_by": operatorID}
		if err := tx.Table("authz_authorization_catalog").Clauses(gormOnConflictCatalog()).Create(metadata).Error; err != nil {
			return fmt.Errorf("save authorization catalog metadata: %w", err)
		}
		return bumpRevision(tx, tenantID, app.ID, now, "authorization catalog synchronized")
	}); err != nil {
		s.recordCatalogFailure(ctx, tenantID, app, operatorID, input, err)
		return CatalogView{}, err
	}
	view, err := s.GetCatalog(ctx, tenantID, app.ID)
	if err != nil {
		return CatalogView{}, err
	}
	s.recordAudit(ctx, AuditEvent{
		TenantID: tenantID, ApplicationID: app.ID, ApplicationCode: app.Code, OperatorID: operatorID,
		Action: "authorization.application_catalog.synced", ResourceType: "authorization_catalog", ResourceID: app.ID,
		Result: "SUCCESS", RiskLevel: "HIGH", Summary: "应用授权目录已同步", OccurredAt: now,
		Metadata: map[string]any{"catalog_version": input.CatalogVersion, "checksum": input.Checksum, "source_type": input.SourceType, "source_identifier": input.SourceIdentifier, "idempotent": false},
	})
	return view, nil
}

// recordCatalogFailure stores the latest failed synchronization attempt without replacing the
// last known good catalog. It is deliberately best-effort so the original validation or database
// error remains the response returned to the caller.
func (s *Service) recordCatalogFailure(ctx context.Context, tenantID string, app applicationRow, operatorID string, input CatalogInput, syncErr error) {
	message := "authorization catalog synchronization failed"
	if syncErr != nil {
		message = syncErr.Error()
	}
	if len(message) > 2000 {
		message = message[:2000]
	}
	now := s.clock.Now().UTC()
	updates := map[string]any{
		"sync_status":    "FAILED",
		"last_synced_at": now,
		"last_synced_by": operatorID,
		"error_message":  message,
		"updated_at":     now,
		"updated_by":     operatorID,
	}
	result := s.db.WithContext(ctx).Table("authz_authorization_catalog").Where("tenant_id = ? AND application_id = ?", tenantID, app.ID).Updates(updates)
	if result.Error == nil && result.RowsAffected == 0 {
		catalogVersion := strings.TrimSpace(input.CatalogVersion)
		catalogHash := strings.TrimSpace(input.Checksum)
		sourceType := strings.TrimSpace(input.SourceType)
		if sourceType == "" {
			sourceType = "API"
		}
		sourceIdentifier := strings.TrimSpace(input.SourceIdentifier)
		if sourceIdentifier == "" {
			sourceIdentifier = app.Code
		}
		metadata := map[string]any{
			"tenant_id": tenantID, "application_id": app.ID,
			"catalog_version": catalogVersion, "catalog_hash": catalogHash,
			"source_type": sourceType, "source_identifier": sourceIdentifier,
			"sync_status": "FAILED", "last_synced_at": now, "last_synced_by": operatorID,
			"error_message": message, "created_at": now, "created_by": operatorID,
			"updated_at": now, "updated_by": operatorID,
		}
		result = s.db.WithContext(ctx).Table("authz_authorization_catalog").Clauses(gormOnConflictCatalog()).Create(metadata)
	}
	metadataRecorded := result.Error == nil
	s.recordAudit(ctx, AuditEvent{
		TenantID: tenantID, ApplicationID: app.ID, ApplicationCode: app.Code, OperatorID: operatorID,
		Action: "authorization.application_catalog.sync_failed", ResourceType: "authorization_catalog", ResourceID: app.ID,
		Result: "FAILURE", RiskLevel: "HIGH", Summary: "应用授权目录同步失败", OccurredAt: now,
		Metadata: map[string]any{
			"catalog_version":   strings.TrimSpace(input.CatalogVersion),
			"source_type":       strings.TrimSpace(input.SourceType),
			"source_identifier": strings.TrimSpace(input.SourceIdentifier),
			"error":             message,
			"metadata_recorded": metadataRecorded,
		},
	})
}

func normalizeCatalog(input CatalogInput) (CatalogInput, string, error) {
	input.CatalogVersion = strings.TrimSpace(input.CatalogVersion)
	if input.CatalogVersion == "" {
		return CatalogInput{}, "", validation("catalog_version is required")
	}
	permissions := make(map[string]PermissionInput, len(input.Permissions))
	// 先规范化并构造确定性内容，再计算摘要；输入顺序和重复项不能改变同一目录的
	// 身份，也不能让角色引用一个未在本次目录声明的权限。
	resourceActions := make(map[string]struct{}, len(input.Permissions))
	for _, item := range input.Permissions {
		item.Code, item.Name, item.Action, item.ResourceCode = strings.TrimSpace(item.Code), strings.TrimSpace(item.Name), strings.TrimSpace(item.Action), strings.TrimSpace(item.ResourceCode)
		item.RiskLevel = strings.ToUpper(strings.TrimSpace(item.RiskLevel))
		if item.RiskLevel == "" {
			item.RiskLevel = "LOW"
		}
		item.RiskLevel = strings.ToUpper(strings.TrimSpace(item.RiskLevel))
		if item.Code == "" || item.Name == "" || item.Action == "" || item.ResourceCode == "" {
			return CatalogInput{}, "", validation("permission code, name, action and resource_code are required")
		}
		if _, exists := permissions[item.Code]; exists {
			return CatalogInput{}, "", validation("duplicate permission code: " + item.Code)
		}
		if item.RiskLevel != "LOW" && item.RiskLevel != "MEDIUM" && item.RiskLevel != "HIGH" && item.RiskLevel != "CRITICAL" {
			return CatalogInput{}, "", validation("risk_level must be LOW, MEDIUM, HIGH or CRITICAL")
		}
		resourceAction := item.ResourceCode + "\x00" + strings.ToLower(item.Action)
		if _, exists := resourceActions[resourceAction]; exists {
			return CatalogInput{}, "", validation("duplicate resource/action: " + item.ResourceCode + "/" + item.Action)
		}
		resourceActions[resourceAction] = struct{}{}
		permissions[item.Code] = item
	}
	input.ClaimsRoleConfigHash = strings.TrimSpace(input.ClaimsRoleConfigHash)
	if len(input.ClaimsRoleConfigHash) > 128 {
		return CatalogInput{}, "", validation("claims_role_config_hash must be at most 128 characters")
	}
	for _, value := range input.ClaimsRoleConfigHash {
		if !((value >= 'a' && value <= 'z') || (value >= 'A' && value <= 'Z') || (value >= '0' && value <= '9') || value == ':' || value == '.' || value == '_' || value == '-') {
			return CatalogInput{}, "", validation("claims_role_config_hash contains unsupported characters")
		}
	}
	policy, err := normalizeCatalogPolicy(input.Policy)
	if err != nil {
		return CatalogInput{}, "", err
	}
	input.Policy = policy
	input.Permissions = input.Permissions[:0]
	for _, item := range permissions {
		input.Permissions = append(input.Permissions, item)
	}
	sort.Slice(input.Permissions, func(i, j int) bool { return input.Permissions[i].Code < input.Permissions[j].Code })
	roles := make(map[string]CatalogRoleInput, len(input.Roles))
	for _, item := range input.Roles {
		item.Code, item.Name = strings.TrimSpace(item.Code), strings.TrimSpace(item.Name)
		item.Permissions = sortedUnique(item.Permissions)
		if item.Code == "" || item.Name == "" {
			return CatalogInput{}, "", validation("role code and name are required")
		}
		if _, exists := roles[item.Code]; exists {
			return CatalogInput{}, "", validation("duplicate role code: " + item.Code)
		}
		for _, permission := range item.Permissions {
			if _, ok := permissions[permission]; !ok {
				return CatalogInput{}, "", validation("role references unknown permission: " + permission)
			}
		}
		roles[item.Code] = item
	}
	input.Roles = input.Roles[:0]
	for _, item := range roles {
		input.Roles = append(input.Roles, item)
	}
	sort.Slice(input.Roles, func(i, j int) bool { return input.Roles[i].Code < input.Roles[j].Code })
	canonical := struct {
		Version     string             `json:"catalog_version"`
		Permissions []PermissionInput  `json:"permissions"`
		Roles       []CatalogRoleInput `json:"roles"`
		Policy      CatalogPolicyInput `json:"policy"`
	}{input.CatalogVersion, input.Permissions, input.Roles, input.Policy}
	encoded, _ := json.Marshal(canonical)
	sum := sha256.Sum256(encoded)
	return input, hex.EncodeToString(sum[:]), nil
}

func gormOnConflictCatalog() clause.OnConflict {
	return clause.OnConflict{Columns: []clause.Column{{Name: "tenant_id"}, {Name: "application_id"}}, DoUpdates: clause.AssignmentColumns([]string{"catalog_version", "catalog_hash", "claims_role_config_hash", "source_type", "source_identifier", "sync_status", "last_synced_at", "last_synced_by", "error_message", "updated_at", "updated_by"})}
}

func (s *Service) findApplicationByID(ctx context.Context, tenantID, id string) (applicationRow, error) {
	var row applicationRow
	err := s.db.WithContext(ctx).Where("tenant_id = ? AND id = ? AND status = ?", strings.TrimSpace(tenantID), strings.TrimSpace(id), activeStatus).Take(&row).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return applicationRow{}, ErrNotFound
	}
	if err != nil {
		return applicationRow{}, fmt.Errorf("load application: %w", err)
	}
	return row, nil
}

func (s *Service) upsertCatalogResource(tx *gorm.DB, tenantID, appID, operatorID string, now time.Time, item PermissionInput) (string, error) {
	var row struct {
		ID string `gorm:"column:id"`
	}
	err := tx.Table("authz_resource").Select("id").Where("tenant_id = ? AND application_id = ? AND code = ?", tenantID, appID, item.ResourceCode).Take(&row).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		id, e := s.ids.New(now)
		if e != nil {
			return "", e
		}
		name := item.ResourceName
		if name == "" {
			name = item.ResourceCode
		}
		err = tx.Table("authz_resource").Create(map[string]any{"id": id, "tenant_id": tenantID, "application_id": appID, "code": item.ResourceCode, "name": name, "resource_type": "BUSINESS", "status": activeStatus, "version": 1, "created_at": now, "created_by": operatorID, "updated_at": now, "updated_by": operatorID}).Error
		if err != nil {
			return "", fmt.Errorf("create resource %s: %w", item.ResourceCode, err)
		}
		return id, nil
	}
	if err != nil {
		return "", err
	}
	if err := tx.Table("authz_resource").Where("id = ?", row.ID).Updates(map[string]any{"status": activeStatus, "version": gorm.Expr("version + 1"), "updated_at": now, "updated_by": operatorID}).Error; err != nil {
		return "", err
	}
	return row.ID, nil
}

func (s *Service) upsertCatalogPermission(tx *gorm.DB, tenantID, appID, operatorID string, now time.Time, item PermissionInput, resourceID string) (string, error) {
	var row struct {
		ID string `gorm:"column:id"`
	}
	err := catalogPermissionByCode(tx, tenantID, appID, item.Code).Take(&row).Error
	// Permission codes can be renamed between catalog versions. The database
	// also enforces one permission per resource/action, so reconcile the legacy
	// row instead of attempting an insert that can never satisfy that unique
	// constraint.
	if errors.Is(err, gorm.ErrRecordNotFound) {
		err = catalogPermissionByResourceAction(tx, tenantID, appID, resourceID, item.Action).Take(&row).Error
	}
	values := map[string]any{"tenant_id": tenantID, "application_id": appID, "resource_id": resourceID, "code": item.Code, "action": item.Action, "name": item.Name, "risk_level": item.RiskLevel, "status": activeStatus, "version": 1, "updated_at": now, "updated_by": operatorID}
	if errors.Is(err, gorm.ErrRecordNotFound) {
		id, e := s.ids.New(now)
		if e != nil {
			return "", e
		}
		values["id"], values["created_at"], values["created_by"] = id, now, operatorID
		if err := tx.Table("authz_permission").Create(values).Error; err != nil {
			return "", err
		}
		return id, nil
	}
	if err != nil {
		return "", err
	}
	values["version"] = gorm.Expr("version + 1")
	if err := tx.Table("authz_permission").Where("id = ?", row.ID).Updates(values).Error; err != nil {
		return "", err
	}
	return row.ID, nil
}

func catalogPermissionByCode(tx *gorm.DB, tenantID, appID, code string) *gorm.DB {
	return tx.Table("authz_permission").Select("id").Where("tenant_id = ? AND application_id = ? AND code = ?", tenantID, appID, code)
}

func catalogPermissionByResourceAction(tx *gorm.DB, tenantID, appID, resourceID, action string) *gorm.DB {
	return tx.Table("authz_permission").Select("id").Where("tenant_id = ? AND application_id = ? AND resource_id = ? AND action = ?", tenantID, appID, resourceID, action)
}

func (s *Service) upsertCatalogRole(tx *gorm.DB, tenantID, appID, operatorID string, now time.Time, item CatalogRoleInput) (string, error) {
	var row struct {
		ID string `gorm:"column:id"`
	}
	err := tx.Table("authz_role").Select("id").Where("tenant_id = ? AND application_id = ? AND code = ?", tenantID, appID, item.Code).Take(&row).Error
	values := map[string]any{"tenant_id": tenantID, "application_id": appID, "code": item.Code, "name": item.Name, "role_type": "APPLICATION", "description": item.Description, "built_in": 0, "status": activeStatus, "version": 1, "updated_at": now, "updated_by": operatorID}
	if errors.Is(err, gorm.ErrRecordNotFound) {
		id, e := s.ids.New(now)
		if e != nil {
			return "", e
		}
		values["id"], values["created_at"], values["created_by"] = id, now, operatorID
		if err := tx.Table("authz_role").Create(values).Error; err != nil {
			return "", err
		}
		return id, nil
	}
	if err != nil {
		return "", err
	}
	values["version"] = gorm.Expr("version + 1")
	if err := tx.Table("authz_role").Where("id = ?", row.ID).Updates(values).Error; err != nil {
		return "", err
	}
	return row.ID, nil
}

func (s *Service) catalogView(ctx context.Context, app applicationRow, metadata catalogMetadataRow) (CatalogView, error) {
	policy, err := s.ResolveApplicationAuthorizationPolicy(ctx, app.TenantID, app.ID)
	if err != nil {
		return CatalogView{}, err
	}
	var permissions []catalogPermissionRow
	if err := s.db.WithContext(ctx).Table("authz_permission AS p").Select("p.id, p.code, p.name, p.action, r.code AS resource_code, p.risk_level, p.status").Joins("JOIN authz_resource AS r ON r.id = p.resource_id").Where("p.tenant_id = ? AND p.application_id = ?", app.TenantID, app.ID).Order("p.code ASC").Find(&permissions).Error; err != nil {
		return CatalogView{}, err
	}
	var roles []catalogRoleRow
	if err := s.db.WithContext(ctx).Table("authz_role").Where("tenant_id = ? AND application_id = ? AND role_type <> ?", app.TenantID, app.ID, "COMPATIBILITY").Order("code ASC").Find(&roles).Error; err != nil {
		return CatalogView{}, err
	}
	permissionViews := make([]CatalogPermissionView, 0, len(permissions))
	for _, p := range permissions {
		permissionViews = append(permissionViews, CatalogPermissionView{Code: p.Code, Name: p.Name, Action: p.Action, ResourceCode: p.ResourceCode, RiskLevel: p.RiskLevel, Status: p.Status})
	}
	roleViews := make([]CatalogRoleView, 0, len(roles))
	for _, role := range roles {
		var perms []string
		if err := s.db.WithContext(ctx).Table("authz_role_permission AS rp").Select("p.code").Joins("JOIN authz_permission AS p ON p.id = rp.permission_id AND p.status = ?", activeStatus).Where("rp.role_id = ? AND rp.effect = ?", role.ID, "ALLOW").Find(&perms).Error; err != nil {
			return CatalogView{}, err
		}
		roleViews = append(roleViews, CatalogRoleView{Code: role.Code, Name: role.Name, Description: role.Description, Status: role.Status, Permissions: sortedUnique(perms)})
	}
	return CatalogView{ApplicationID: app.ID, ApplicationCode: app.Code, CatalogVersion: metadata.CatalogVersion, Checksum: metadata.CatalogHash, ClaimsRoleConfigHash: metadata.ClaimsRoleConfigHash, SourceType: metadata.SourceType, SourceIdentifier: metadata.SourceIdentifier, SyncStatus: metadata.SyncStatus, LastSyncedAt: metadata.LastSyncedAt, LastSyncedBy: metadata.LastSyncedBy, ErrorMessage: metadata.ErrorMessage, Policy: policy, Permissions: permissionViews, Roles: roleViews}, nil
}

// EnsurePlatformCatalogSynced mirrors the migration-seeded built-in platform roles and
// permissions into the application-owned catalog row when the platform's own subsystem has not
// published a catalog. The platform has no catalog-publisher OAuth client for itself, so this
// bootstrap is the only path that surfaces the built-in data through the application-owned
// catalog mirror. The function is idempotent: a SYNCED row whose catalog_hash matches the
// current built-in data only refreshes last_synced_at/last_synced_by/updated_at/updated_by so
// the catalog row tracks whoever (or whatever) most recently re-acknowledged the built-in data.
//
// The function deliberately avoids SyncCatalog: SyncCatalog is designed for subsystem-published
// manifests and would overwrite the platform's role_type=PLATFORM / built_in=1 metadata with
// role_type=APPLICATION / built_in=0. Mirroring only the catalog row preserves the platform's
// built-in distinctions.
func (s *Service) EnsurePlatformCatalogSynced(ctx context.Context, tenantID, operatorID string) (CatalogView, error) {
	app, err := s.findApplicationByCode(ctx, tenantID, PlatformApplicationCode)
	if err != nil {
		return CatalogView{}, err
	}
	hash, err := s.computePlatformCatalogHash(ctx, app.TenantID, app.ID)
	if err != nil {
		return CatalogView{}, err
	}
	var existing catalogMetadataRow
	lookupErr := s.db.WithContext(ctx).Table("authz_authorization_catalog").
		Where("tenant_id = ? AND application_id = ?", tenantID, app.ID).
		Take(&existing).Error
	if lookupErr != nil && !errors.Is(lookupErr, gorm.ErrRecordNotFound) {
		return CatalogView{}, fmt.Errorf("load existing platform catalog: %w", lookupErr)
	}
	now := s.clock.Now().UTC()
	updates := map[string]any{
		"tenant_id":               tenantID,
		"application_id":          app.ID,
		"catalog_version":         PlatformCatalogVersion,
		"catalog_hash":            hash,
		"claims_role_config_hash": "",
		"source_type":             PlatformCatalogSourceType,
		"source_identifier":       PlatformCatalogSourceIdentifier,
		"sync_status":             "SYNCED",
		"last_synced_at":          now,
		"last_synced_by":          operatorID,
		"error_message":           nil,
		"updated_at":              now,
		"updated_by":              operatorID,
	}
	contentChanged := errors.Is(lookupErr, gorm.ErrRecordNotFound) ||
		existing.SyncStatus != "SYNCED" ||
		!strings.EqualFold(existing.CatalogHash, hash)
	if !contentChanged && existing.LastSyncedBy == operatorID {
		// Truly idempotent: the row already reflects the current built-in data and the same
		// operator. Returning the live view is cheap and keeps callers consistent.
		return s.GetCatalog(ctx, tenantID, app.ID)
	}
	var writeErr error
	if errors.Is(lookupErr, gorm.ErrRecordNotFound) {
		updates["created_at"] = now
		updates["created_by"] = operatorID
		writeErr = s.db.WithContext(ctx).Table("authz_authorization_catalog").
			Clauses(clause.OnConflict{Columns: []clause.Column{{Name: "tenant_id"}, {Name: "application_id"}}, DoUpdates: clause.AssignmentColumns([]string{"catalog_version", "catalog_hash", "claims_role_config_hash", "source_type", "source_identifier", "sync_status", "last_synced_at", "last_synced_by", "error_message", "updated_at", "updated_by"})}).
			Create(updates).Error
	} else {
		writeErr = s.db.WithContext(ctx).Table("authz_authorization_catalog").
			Where("tenant_id = ? AND application_id = ?", tenantID, app.ID).
			Updates(updates).Error
	}
	if writeErr != nil {
		return CatalogView{}, fmt.Errorf("persist platform catalog metadata: %w", writeErr)
	}
	s.recordAudit(ctx, AuditEvent{
		TenantID: tenantID, ApplicationID: app.ID, ApplicationCode: app.Code, OperatorID: operatorID,
		Action: "authorization.application_catalog.synced", ResourceType: "authorization_catalog", ResourceID: app.ID,
		Result: "SUCCESS", RiskLevel: "HIGH", Summary: "基础能力平台授权目录已同步（内置）", OccurredAt: now,
		Metadata: map[string]any{
			"catalog_version":   PlatformCatalogVersion,
			"checksum":          hash,
			"source_type":       PlatformCatalogSourceType,
			"source_identifier": PlatformCatalogSourceIdentifier,
			"bootstrap":         true,
			"content_changed":   contentChanged,
		},
	})
	return s.GetCatalog(ctx, tenantID, app.ID)
}

// computePlatformCatalogHash returns a stable sha256 digest of the platform's currently
// active role and permission codes. Re-syncing is skipped while the hash is unchanged, so an
// idempotent API restart does not churn the catalog row or the audit log.
func (s *Service) computePlatformCatalogHash(ctx context.Context, tenantID, appID string) (string, error) {
	var roleCodes []string
	if err := s.db.WithContext(ctx).Table("authz_role").
		Where("tenant_id = ? AND application_id = ? AND status = ?", tenantID, appID, activeStatus).
		Order("code ASC").Pluck("code", &roleCodes).Error; err != nil {
		return "", fmt.Errorf("load platform role codes: %w", err)
	}
	var permissionCodes []string
	if err := s.db.WithContext(ctx).Table("authz_permission").
		Where("tenant_id = ? AND application_id = ? AND status = ?", tenantID, appID, activeStatus).
		Order("code ASC").Pluck("code", &permissionCodes).Error; err != nil {
		return "", fmt.Errorf("load platform permission codes: %w", err)
	}
	canonical := struct {
		Roles       []string `json:"roles"`
		Permissions []string `json:"permissions"`
	}{roleCodes, permissionCodes}
	encoded, err := json.Marshal(canonical)
	if err != nil {
		return "", fmt.Errorf("marshal platform catalog payload: %w", err)
	}
	sum := sha256.Sum256(encoded)
	return "sha256:" + hex.EncodeToString(sum[:]), nil
}

func (s *Service) findApplicationByCode(ctx context.Context, tenantID, code string) (applicationRow, error) {
	var row applicationRow
	err := s.db.WithContext(ctx).Where("tenant_id = ? AND code = ? AND status = ?", strings.TrimSpace(tenantID), strings.TrimSpace(code), activeStatus).Take(&row).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return applicationRow{}, ErrNotFound
	}
	if err != nil {
		return applicationRow{}, fmt.Errorf("load application by code: %w", err)
	}
	return row, nil
}
