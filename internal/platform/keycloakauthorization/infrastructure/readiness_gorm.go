package infrastructure

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	registryhttp "github.com/J-S-Te/Basic-Platform/internal/platform/applicationregistry/interfaces/http"
	platformulid "github.com/J-S-Te/Basic-Platform/internal/shared/ulid"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// SwitchReadinessStore is the authoritative, durable Keycloak cutover gate.
// It intentionally returns incomplete evidence as blocked rather than trying
// to infer a successful browser login from an Admin API operation.
type SwitchReadinessStore struct{ database *gorm.DB }

func NewSwitchReadinessStore(database *gorm.DB) (*SwitchReadinessStore, error) {
	if database == nil {
		return nil, errors.New("Keycloak switch readiness database must not be nil")
	}
	return &SwitchReadinessStore{database: database}, nil
}

type readinessScope struct {
	ApplicationID string `gorm:"column:application_id"`
	EnvironmentID string `gorm:"column:environment_id"`
}

type readinessRow struct {
	ClientReady             bool `gorm:"column:client_ready"`
	RoleCatalogSynced       bool `gorm:"column:role_catalog_synced"`
	UserProjectionCompleted bool `gorm:"column:user_projection_completed"`
	BrokerLoginVerified     bool `gorm:"column:broker_login_verified"`
}

func (store *SwitchReadinessStore) InspectKeycloakSwitchReadiness(ctx context.Context, tenantID, applicationCode, environment string) (registryhttp.KeycloakSwitchReadiness, error) {
	// readiness 同时读取四项持久化门禁和 outbox 未完成数；队列未清空时强制撤销用户投影门禁。
	scope, err := store.scope(ctx, tenantID, applicationCode, environment)
	if err != nil {
		return registryhttp.KeycloakSwitchReadiness{}, err
	}
	var row readinessRow
	err = store.database.WithContext(ctx).Table("keycloak_switch_readiness").
		Where("tenant_id = ? AND application_id = ? AND environment_id = ?", strings.TrimSpace(tenantID), scope.ApplicationID, scope.EnvironmentID).
		Take(&row).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return blockedReadiness(), nil
	}
	if err != nil {
		return registryhttp.KeycloakSwitchReadiness{}, fmt.Errorf("load Keycloak switch readiness: %w", err)
	}
	var outstanding int64
	if err := store.database.WithContext(ctx).Table("keycloak_authorization_outbox").
		Where("tenant_id = ? AND application_id = ? AND environment_id = ? AND status IN ?", strings.TrimSpace(tenantID), scope.ApplicationID, scope.EnvironmentID, []string{"PENDING", "RUNNING", "FAILED"}).
		Count(&outstanding).Error; err != nil {
		return registryhttp.KeycloakSwitchReadiness{}, fmt.Errorf("count outstanding Keycloak projections: %w", err)
	}
	if outstanding > 0 {
		row.UserProjectionCompleted = false
	}
	return readinessFromRow(row), nil
}

// MarkKeycloakClientAndRoleCatalogSynced is called only after both Keycloak
// Admin operations and the local Client mapping have succeeded.
func (store *SwitchReadinessStore) MarkKeycloakClientAndRoleCatalogSynced(ctx context.Context, tenantID, applicationID, environmentID string) error {
	return store.upsert(ctx, tenantID, applicationID, environmentID, map[string]any{
		"client_ready": true, "role_catalog_synced": true,
	})
}

// MarkUserProjectionCompleted is deliberately explicit: callers must not mark
// the user gate from a queued/scheduled projection.  It is available for the
// projection coordinator once it has durably established completion.
func (store *SwitchReadinessStore) MarkUserProjectionCompleted(ctx context.Context, tenantID, applicationID, environmentID string) error {
	return store.upsert(ctx, tenantID, applicationID, environmentID, map[string]any{"user_projection_completed": true})
}

// RecordBrokerLoginVerification validates the persisted Client mapping before
// atomically updating the current gate and appending immutable audit evidence.
func (store *SwitchReadinessStore) RecordBrokerLoginVerification(ctx context.Context, input registryhttp.KeycloakBrokerLoginVerification) error {
	// 只有当前租户/应用/环境的已同步 Client 且带验签会话证据，才能开启最后一道门禁。
	input = normalizeVerification(input)
	if input.TenantID == "" || input.ApplicationID == "" || input.EnvironmentID == "" || input.IdentityID == "" || input.Issuer == "" || input.ClientID == "" || input.VerifiedByID == "" {
		return errors.New("incomplete Keycloak broker login verification")
	}
	return store.database.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var mapping struct {
			ClientID string `gorm:"column:keycloak_client_id"`
		}
		if err := tx.Table("keycloak_application_client_mapping").Select("keycloak_client_id").
			Where("tenant_id = ? AND application_id = ? AND environment_id = ? AND keycloak_client_id = ? AND status = ?", input.TenantID, input.ApplicationID, input.EnvironmentID, input.ClientID, "SYNCED").Take(&mapping).Error; err != nil {
			return fmt.Errorf("verify Keycloak Client mapping: %w", err)
		}
		now := time.Now().UTC()
		if err := upsertReadiness(tx, input.TenantID, input.ApplicationID, input.EnvironmentID, map[string]any{
			"broker_login_verified": true, "broker_verified_identity_id": input.IdentityID,
			"broker_verified_issuer": input.Issuer, "broker_verified_client_id": input.ClientID,
			"broker_verified_by_id": input.VerifiedByID, "broker_verified_session_id": nullableString(input.SessionID), "broker_verified_at": now,
		}, now); err != nil {
			return err
		}
		eventID, err := platformulid.New(now)
		if err != nil {
			return fmt.Errorf("generate Keycloak broker verification audit ID: %w", err)
		}
		return tx.Table("keycloak_broker_login_verification").Create(map[string]any{
			"id": eventID, "tenant_id": input.TenantID, "application_id": input.ApplicationID, "environment_id": input.EnvironmentID,
			"identity_id": input.IdentityID, "issuer": input.Issuer, "keycloak_client_id": input.ClientID,
			"verified_by_id": input.VerifiedByID, "session_id": nullableString(input.SessionID), "verified_at": now,
		}).Error
	})
}

func (store *SwitchReadinessStore) scope(ctx context.Context, tenantID, applicationCode, environment string) (readinessScope, error) {
	var scope readinessScope
	err := store.database.WithContext(ctx).Table("platform_application AS application").
		Select("application.id AS application_id, environment.id AS environment_id").
		Joins("JOIN platform_application_environment AS environment ON environment.tenant_id = application.tenant_id AND environment.application_id = application.id").
		Where("application.tenant_id = ? AND application.code = ? AND environment.environment = ?", strings.TrimSpace(tenantID), strings.TrimSpace(applicationCode), strings.ToLower(strings.TrimSpace(environment))).
		Take(&scope).Error
	if err != nil {
		return readinessScope{}, fmt.Errorf("resolve Keycloak readiness scope: %w", err)
	}
	return scope, nil
}

func (store *SwitchReadinessStore) upsert(ctx context.Context, tenantID, applicationID, environmentID string, values map[string]any) error {
	return upsertReadiness(store.database.WithContext(ctx), strings.TrimSpace(tenantID), strings.TrimSpace(applicationID), strings.TrimSpace(environmentID), values, time.Now().UTC())
}

func upsertReadiness(database *gorm.DB, tenantID, applicationID, environmentID string, values map[string]any, now time.Time) error {
	values["updated_at"] = now
	create := map[string]any{"tenant_id": tenantID, "application_id": applicationID, "environment_id": environmentID, "client_ready": false, "role_catalog_synced": false, "user_projection_completed": false, "broker_login_verified": false, "created_at": now, "updated_at": now}
	for key, value := range values {
		create[key] = value
	}
	return database.Table("keycloak_switch_readiness").Clauses(clause.OnConflict{
		Columns: []clause.Column{{Name: "tenant_id"}, {Name: "application_id"}, {Name: "environment_id"}}, DoUpdates: clause.Assignments(values),
	}).Create(create).Error
}

func normalizeVerification(value registryhttp.KeycloakBrokerLoginVerification) registryhttp.KeycloakBrokerLoginVerification {
	value.TenantID, value.ApplicationID, value.EnvironmentID = strings.TrimSpace(value.TenantID), strings.TrimSpace(value.ApplicationID), strings.TrimSpace(value.EnvironmentID)
	value.IdentityID, value.ClientID, value.VerifiedByID, value.SessionID = strings.TrimSpace(value.IdentityID), strings.TrimSpace(value.ClientID), strings.TrimSpace(value.VerifiedByID), strings.TrimSpace(value.SessionID)
	value.Issuer = strings.TrimRight(strings.TrimSpace(value.Issuer), "/")
	return value
}

func nullableString(value string) any {
	if value == "" {
		return nil
	}
	return value
}

func readinessFromRow(row readinessRow) registryhttp.KeycloakSwitchReadiness {
	gates := []registryhttp.KeycloakSwitchGate{
		{Key: "client_ready", Label: "Client 已就绪", Passed: row.ClientReady, Detail: "Realm Client 已由服务器同步。", NextAction: "执行“同步 Keycloak”。"},
		{Key: "role_catalog_synced", Label: "角色目录已同步", Passed: row.RoleCatalogSynced, Detail: "角色目录已由服务器同步。", NextAction: "同步 Keycloak 角色目录。"},
		{Key: "user_projection_completed", Label: "用户投影已完成", Passed: row.UserProjectionCompleted, Detail: "用户投影完成状态由服务器持久化。", NextAction: "完成该环境的用户投影。"},
		{Key: "broker_login_verified", Label: "Broker 登录验证已通过", Passed: row.BrokerLoginVerified, Detail: "Broker 登录由当前认证会话回传并保存审计证据。", NextAction: "使用目标应用完成一次 Broker 登录验证。"},
	}
	ready := true
	for _, gate := range gates {
		if !gate.Passed {
			ready = false
			break
		}
	}
	return registryhttp.KeycloakSwitchReadiness{Gates: gates, SwitchReady: ready, NextAction: "四项门禁均需由服务器验证后才能切换 Issuer。"}
}

func blockedReadiness() registryhttp.KeycloakSwitchReadiness { return readinessFromRow(readinessRow{}) }

var _ interface {
	InspectKeycloakSwitchReadiness(context.Context, string, string, string) (registryhttp.KeycloakSwitchReadiness, error)
} = (*SwitchReadinessStore)(nil)
