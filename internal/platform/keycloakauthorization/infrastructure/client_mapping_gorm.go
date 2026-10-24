package infrastructure

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

	registryhttp "github.com/J-S-Te/Basic-Platform/internal/platform/applicationregistry/interfaces/http"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// ClientMappingStore 持久化租户/应用/环境到 Keycloak realm 与 Client 的映射，
// 并维护配置哈希与切换门禁的联动失效。
type ClientMappingStore struct{ database *gorm.DB }

// keycloakProjectionConfigurationVersion 标识 Keycloak Client mapping 背后由平台
// 管理的投影契约。稳定身份属性或投影语义的任何变化都必须递增该值，使此前完成的
// 用户投影与 Broker 证据不可被复用。
// v5：独立 identity_id mapper 与启动期 Admin API 对账纳入投影契约后，强制所有
// 既有 Client mapping 完整重投影一次，防止数据库中的历史就绪状态掩盖 mappers
// 实际已过期的 Realm Client。
const keycloakProjectionConfigurationVersion = "stable-identity-projection-v5"

type persistedClientMapping struct {
	Realm             string `gorm:"column:realm"`
	ClientID          string `gorm:"column:keycloak_client_id"`
	ConfigurationHash string `gorm:"column:configuration_hash"`
	Status            string `gorm:"column:status"`
}

// StoredKeycloakClientMapping 是 worker 启动时重新应用 Realm Client 所需的
// 非机密注册数据。持久化的 Client ID 保持权威，兼容映射绝不会被悄悄改名。
type StoredKeycloakClientMapping struct {
	TenantID        string `gorm:"column:tenant_id"`
	ApplicationID   string `gorm:"column:application_id"`
	EnvironmentID   string `gorm:"column:environment_id"`
	ApplicationName string `gorm:"column:application_name"`
	BaseURL         string `gorm:"column:base_url"`
	PathPrefix      string `gorm:"column:path_prefix"`
	Realm           string `gorm:"column:realm"`
	ClientID        string `gorm:"column:keycloak_client_id"`
}

type managedClientConfiguration struct {
	BaseURL              string `gorm:"column:base_url"`
	PathPrefix           string `gorm:"column:path_prefix"`
	CatalogHash          string `gorm:"column:catalog_hash"`
	ClaimsRoleConfigHash string `gorm:"column:claims_role_config_hash"`
}

// NewClientMappingStore 构造 Client 映射存储；database 为空时返回错误。
func NewClientMappingStore(database *gorm.DB) (*ClientMappingStore, error) {
	if database == nil {
		return nil, errors.New("Keycloak Client mapping database must not be nil")
	}
	return &ClientMappingStore{database: database}, nil
}

// ListStoredKeycloakClientMappings 返回全部已同步映射及其当前浏览器传输配置。
// 调用方必须先成功对账真实 Keycloak Client 与角色，才能再次保存映射。
func (store *ClientMappingStore) ListStoredKeycloakClientMappings(ctx context.Context) ([]StoredKeycloakClientMapping, error) {
	if store == nil || store.database == nil {
		return nil, errors.New("Keycloak Client mapping database must not be nil")
	}
	var mappings []StoredKeycloakClientMapping
	if err := syncedKeycloakClientMappingsQuery(store.database.WithContext(ctx)).Find(&mappings).Error; err != nil {
		return nil, fmt.Errorf("load synchronized Keycloak Client mappings: %w", err)
	}
	return mappings, nil
}

func syncedKeycloakClientMappingsQuery(database *gorm.DB) *gorm.DB {
	return database.Table("keycloak_application_client_mapping AS mapping").
		Select(`mapping.tenant_id, mapping.application_id, mapping.environment_id,
			application.name AS application_name, environment.base_url, environment.path_prefix,
			mapping.realm, mapping.keycloak_client_id`).
		Joins("JOIN platform_application AS application ON application.tenant_id = mapping.tenant_id AND application.id = mapping.application_id").
		Joins("JOIN platform_application_environment AS environment ON environment.tenant_id = mapping.tenant_id AND environment.application_id = mapping.application_id AND environment.id = mapping.environment_id").
		Where("mapping.status = ?", "SYNCED").
		Order("mapping.tenant_id ASC, mapping.application_id ASC, mapping.environment_id ASC")
}

// SaveKeycloakClientMapping 在事务内保存映射并重算配置哈希；realm/Client ID 或
// 托管配置变化时会撤销四道切换门禁、重置投影并重新入队全部用户的对账投影。
func (store *ClientMappingStore) SaveKeycloakClientMapping(ctx context.Context, tenantID, applicationID, environmentID, realm, clientID string) error {
	if store == nil || store.database == nil {
		return errors.New("Keycloak Client mapping database must not be nil")
	}
	tenantID = strings.TrimSpace(tenantID)
	applicationID = strings.TrimSpace(applicationID)
	environmentID = strings.TrimSpace(environmentID)
	realm = strings.TrimSpace(realm)
	clientID = strings.TrimSpace(clientID)
	if tenantID == "" || applicationID == "" || environmentID == "" || realm == "" || clientID == "" {
		return errors.New("Keycloak Client mapping scope, realm and Client ID are required")
	}
	now := time.Now().UTC()
	return store.database.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		managedConfiguration, err := loadManagedClientConfiguration(tx, tenantID, applicationID, environmentID)
		if err != nil {
			return fmt.Errorf("load platform-managed Keycloak Client configuration: %w", err)
		}
		configurationHash := keycloakClientConfigurationHash(
			realm, clientID, keycloakProjectionConfigurationVersion,
			managedConfiguration.BaseURL, managedConfiguration.PathPrefix,
			managedConfiguration.CatalogHash, managedConfiguration.ClaimsRoleConfigHash,
		)
		var previous persistedClientMapping
		err = tx.Table("keycloak_application_client_mapping").
			Select("realm, keycloak_client_id, configuration_hash, status").
			Clauses(clause.Locking{Strength: "UPDATE"}).
			Where("tenant_id = ? AND application_id = ? AND environment_id = ?", tenantID, applicationID, environmentID).
			Take(&previous).Error
		mappingExists := err == nil
		if err != nil && !errors.Is(err, gorm.ErrRecordNotFound) {
			return fmt.Errorf("load existing Keycloak Client mapping: %w", err)
		}

		if err := tx.Table("keycloak_application_client_mapping").Clauses(clause.OnConflict{
			Columns: []clause.Column{{Name: "tenant_id"}, {Name: "application_id"}, {Name: "environment_id"}},
			DoUpdates: clause.Assignments(map[string]any{
				"realm": realm, "keycloak_client_id": clientID, "configuration_hash": configurationHash,
				"status": "SYNCED", "last_synced_at": now, "updated_at": now,
			}),
		}).Create(map[string]any{
			"tenant_id": tenantID, "application_id": applicationID, "environment_id": environmentID,
			"realm": realm, "keycloak_client_id": clientID, "configuration_hash": configurationHash,
			"status": "SYNCED", "last_synced_at": now, "created_at": now, "updated_at": now,
		}).Error; err != nil {
			return fmt.Errorf("save Keycloak Client mapping: %w", err)
		}

		if !shouldInvalidateKeycloakClientConfiguration(mappingExists, previous, realm, clientID, configurationHash) {
			return nil
		}
		return invalidateKeycloakClientConfiguration(tx, tenantID, applicationID, environmentID, clientID, configurationHash, now)
	})
}

func loadManagedClientConfiguration(database *gorm.DB, tenantID, applicationID, environmentID string) (managedClientConfiguration, error) {
	var configuration managedClientConfiguration
	err := database.Table("platform_application_environment AS environment").
		Select(`COALESCE(environment.base_url, '') AS base_url,
			COALESCE(environment.path_prefix, '') AS path_prefix,
			COALESCE(catalog.catalog_hash, '') AS catalog_hash,
			COALESCE(catalog.claims_role_config_hash, '') AS claims_role_config_hash`).
		Joins("LEFT JOIN authz_authorization_catalog AS catalog ON catalog.tenant_id = environment.tenant_id AND catalog.application_id = environment.application_id").
		Where("environment.tenant_id = ? AND environment.application_id = ? AND environment.id = ?", tenantID, applicationID, environmentID).
		Take(&configuration).Error
	return configuration, err
}

func keycloakClientConfigurationHash(realm, clientID, version string, managedValues ...string) string {
	values := []string{realm, clientID, version}
	values = append(values, managedValues...)
	for index := range values {
		values[index] = strings.TrimSpace(values[index])
	}
	digest := sha256.Sum256([]byte(strings.Join(values, "\x00")))
	return hex.EncodeToString(digest[:])
}

func keycloakClientConfigurationChanged(exists bool, previous persistedClientMapping, realm, clientID, configurationHash string) bool {
	if !exists {
		return true
	}
	return strings.TrimSpace(previous.Realm) != strings.TrimSpace(realm) ||
		strings.TrimSpace(previous.ClientID) != strings.TrimSpace(clientID) ||
		strings.TrimSpace(previous.ConfigurationHash) != strings.TrimSpace(configurationHash)
}

func shouldInvalidateKeycloakClientConfiguration(exists bool, previous persistedClientMapping, realm, clientID, configurationHash string) bool {
	if !exists {
		return true
	}
	if !strings.EqualFold(strings.TrimSpace(previous.Status), "SYNCED") {
		return true
	}
	return keycloakClientConfigurationChanged(exists, previous, realm, clientID, configurationHash)
}

func invalidateKeycloakClientConfiguration(tx *gorm.DB, tenantID, applicationID, environmentID, clientID, configurationHash string, now time.Time) error {
	readiness := map[string]any{
		"client_ready": false, "role_catalog_synced": false, "user_projection_completed": false,
		"broker_login_verified": false, "client_configuration_hash": configurationHash,
		"broker_verified_configuration_hash": nil, "broker_verified_identity_id": nil,
		"broker_verified_issuer": nil, "broker_verified_client_id": nil, "broker_verified_by_id": nil,
		"broker_verified_session_id": nil, "broker_verified_at": nil,
	}
	if err := upsertReadiness(tx, tenantID, applicationID, environmentID, readiness, now); err != nil {
		return fmt.Errorf("invalidate Keycloak switch readiness: %w", err)
	}
	if err := tx.Table("keycloak_authorization_reconcile_backfill").
		Where("tenant_id = ? AND application_id = ? AND environment_id = ?", tenantID, applicationID, environmentID).
		Delete(nil).Error; err != nil {
		return fmt.Errorf("reset Keycloak authorization backfill ledger: %w", err)
	}
	if err := tx.Table("keycloak_authorization_projection").
		Where("tenant_id = ? AND application_id = ? AND environment_id = ?", tenantID, applicationID, environmentID).
		Updates(map[string]any{
			"keycloak_client_id": clientID, "status": "PENDING", "last_synced_at": nil,
			"last_error_code": nil, "last_error_message": nil, "updated_at": now,
		}).Error; err != nil {
		return fmt.Errorf("invalidate Keycloak authorization projections: %w", err)
	}
	var identityIDs []string
	if err := allKeycloakConfigurationUsersQuery(tx, tenantID).Pluck("id", &identityIDs).Error; err != nil {
		return fmt.Errorf("load Keycloak configuration reconcile users: %w", err)
	}
	for _, identityID := range identityIDs {
		if err := enqueueInitialKeycloakReconcile(tx, tenantID, strings.TrimSpace(identityID), applicationID, environmentID, now); err != nil {
			return fmt.Errorf("enqueue Keycloak configuration reconcile: %w", err)
		}
	}
	return nil
}

func allKeycloakConfigurationUsersQuery(database *gorm.DB, tenantID string) *gorm.DB {
	// 包含停用与业务删除的身份，使配置迁移也会停用其 Keycloak 账号并清理过期的
	// 详细授权属性。ProjectionSource 刻意以空授予快照处理它们。
	return database.Table("iam_user").Where("tenant_id = ?", strings.TrimSpace(tenantID)).Order("id ASC")
}

// GetKeycloakClientMapping 向应用认证 UI 暴露一份安全的持久化映射摘要。
// 该表从不存储 Client secret，返回值中也绝不包含。
func (store *ClientMappingStore) GetKeycloakClientMapping(ctx context.Context, tenantID, applicationID, environmentID string) (registryhttp.KeycloakClientMapping, error) {
	var row struct {
		Realm        string     `gorm:"column:realm"`
		ClientID     string     `gorm:"column:keycloak_client_id"`
		Status       string     `gorm:"column:status"`
		LastSyncedAt *time.Time `gorm:"column:last_synced_at"`
	}
	err := store.database.WithContext(ctx).Table("keycloak_application_client_mapping").
		Select("realm, keycloak_client_id, status, last_synced_at").
		Where("tenant_id = ? AND application_id = ? AND environment_id = ?", strings.TrimSpace(tenantID), strings.TrimSpace(applicationID), strings.TrimSpace(environmentID)).
		Take(&row).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return registryhttp.KeycloakClientMapping{Status: "NOT_CONFIGURED"}, nil
	}
	if err != nil {
		return registryhttp.KeycloakClientMapping{}, err
	}
	return registryhttp.KeycloakClientMapping{Realm: strings.TrimSpace(row.Realm), ClientID: strings.TrimSpace(row.ClientID), Status: strings.TrimSpace(row.Status), LastSyncedAt: row.LastSyncedAt, Exists: true}, nil
}

// ResolveEffectiveKeycloakClient 保证平台环境标签规范化后既有子系统配置仍可用。
// 持久化映射是权威值；唯一例外是同一应用/环境下显式注册过的遗留浏览器 OAuth
// client 会被保留，避免运行中的子系统被悄悄强制更换 client_id。Service client
// 不在保留之列。不做任何 prod/dev 字符串替换或跨环境查找。
func (store *ClientMappingStore) ResolveEffectiveKeycloakClient(ctx context.Context, tenantID, applicationID, environmentID, canonicalClientID string) (registryhttp.KeycloakClientResolution, error) {
	canonicalClientID = strings.TrimSpace(canonicalClientID)
	resolution := registryhttp.KeycloakClientResolution{ClientID: canonicalClientID, CanonicalClientID: canonicalClientID, Source: "canonical"}

	mapping, err := store.GetKeycloakClientMapping(ctx, tenantID, applicationID, environmentID)
	if err != nil {
		return resolution, err
	}
	mappedClientID := strings.TrimSpace(mapping.ClientID)

	var clients []struct {
		ClientID   string `gorm:"column:client_id"`
		ClientType string `gorm:"column:client_type"`
		Status     string `gorm:"column:status"`
	}
	err = store.database.WithContext(ctx).Table("platform_oauth_client").
		Select("client_id, client_type, status").
		Where("tenant_id = ? AND application_id = ? AND environment_id = ? AND status = ? AND client_type IN ?", strings.TrimSpace(tenantID), strings.TrimSpace(applicationID), strings.TrimSpace(environmentID), "ACTIVE", []string{"public", "confidential"}).
		Find(&clients).Error
	if err != nil {
		return resolution, err
	}

	legacy := make([]string, 0, len(clients))
	for _, client := range clients {
		id := strings.TrimSpace(client.ClientID)
		if id == "" || strings.EqualFold(id, canonicalClientID) || isKeycloakServiceClientID(id) {
			continue
		}
		legacy = append(legacy, id)
	}
	if len(legacy) > 1 {
		return resolution, errors.New("multiple legacy browser OAuth clients are registered for the application environment")
	}
	if len(legacy) == 1 {
		return registryhttp.KeycloakClientResolution{ClientID: legacy[0], CanonicalClientID: canonicalClientID, Source: "legacy_oauth_client", LegacyCompatibility: true}, nil
	}
	if mappedClientID != "" && strings.EqualFold(mapping.Status, "SYNCED") {
		return registryhttp.KeycloakClientResolution{ClientID: mappedClientID, CanonicalClientID: canonicalClientID, Source: "persisted_mapping", LegacyCompatibility: !strings.EqualFold(mappedClientID, canonicalClientID)}, nil
	}
	return resolution, nil
}

func isKeycloakServiceClientID(clientID string) bool {
	clientID = strings.ToLower(strings.TrimSpace(clientID))
	for _, suffix := range []string{"-catalog-publisher", "-audit-publisher", "-summary-read", "-opportunity-intake"} {
		if strings.HasSuffix(clientID, suffix) {
			return true
		}
	}
	return false
}
