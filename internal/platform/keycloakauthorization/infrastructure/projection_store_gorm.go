package infrastructure

import (
	"context"
	"errors"
	"strings"
	"time"

	projectionapplication "github.com/J-S-Te/Basic-Platform/internal/platform/keycloakauthorization/application"
	projectionworker "github.com/J-S-Te/Basic-Platform/internal/platform/keycloakauthorization/worker"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// ProjectionStore 持久化 Keycloak 授权投影的结果。虽然当前 worker 事件是应用级
// 的，environment_id 仍被刻意建模：未来扇出 worker 可以直接填充，无需改变本
// 适配器的存储契约。
type ProjectionStore struct{ database *gorm.DB }

// NewProjectionStore 构造投影结果存储；database 为空时返回错误。
func NewProjectionStore(database *gorm.DB) (*ProjectionStore, error) {
	if database == nil {
		return nil, errors.New("Keycloak authorization projection database must not be nil")
	}
	return &ProjectionStore{database: database}, nil
}

// MarkSynchronized 按复合业务键幂等记录一次成功的投影。
func (store *ProjectionStore) MarkSynchronized(ctx context.Context, snapshot projectionapplication.Snapshot, synchronizedAt time.Time) error {
	// 同步成功状态按复合业务键幂等 upsert；last_synced_at 保存快照加载前的
	// 对账水位，避免外部调用期间跨过的时间边界被误认为已投影。
	now := synchronizedAt.UTC()
	return store.database.WithContext(ctx).Table("keycloak_authorization_projection").Clauses(clause.OnConflict{
		Columns: []clause.Column{{Name: "tenant_id"}, {Name: "identity_id"}, {Name: "application_id"}, {Name: "environment_id"}},
		DoUpdates: clause.Assignments(map[string]any{
			"application_code":       strings.TrimSpace(snapshot.ApplicationCode),
			"keycloak_client_id":     strings.TrimSpace(snapshot.KeycloakClientID),
			"authorization_revision": snapshot.AuthorizationRevision,
			"role_config_hash":       strings.TrimSpace(snapshot.RoleConfigHash),
			"user_enabled":           snapshot.UserEnabled,
			"status":                 "SYNCED",
			"last_synced_at":         now,
			"last_error_code":        nil,
			"last_error_message":     nil,
			"updated_at":             now,
		}),
	}).Create(map[string]any{
		"tenant_id": strings.TrimSpace(snapshot.TenantID), "identity_id": strings.TrimSpace(snapshot.IdentityID),
		"application_id": strings.TrimSpace(snapshot.ApplicationID), "environment_id": strings.TrimSpace(snapshot.EnvironmentID), "application_code": strings.TrimSpace(snapshot.ApplicationCode),
		"keycloak_client_id": strings.TrimSpace(snapshot.KeycloakClientID), "authorization_revision": snapshot.AuthorizationRevision,
		"role_config_hash": strings.TrimSpace(snapshot.RoleConfigHash), "user_enabled": snapshot.UserEnabled, "status": "SYNCED", "last_synced_at": now,
		"created_at": now, "updated_at": now,
	}).Error
}

// MarkFailed 按复合业务键 upsert 最后一次投影失败的诊断状态；即使尚无成功投影行
// 也会创建记录，不丢失唯一的失败证据。
func (store *ProjectionStore) MarkFailed(ctx context.Context, event projectionworker.Event, code, message string, failedAt time.Time) error {
	// 失败也要落库，使 outbox 重试之外仍保留最后一次投影诊断状态。
	now := failedAt.UTC()
	// 源事件失败时可能尚无成功的投影行。用 upsert 记录该失败，
	// 避免丢失唯一的诊断信号。
	return store.database.WithContext(ctx).Table("keycloak_authorization_projection").Clauses(clause.OnConflict{
		Columns: []clause.Column{{Name: "tenant_id"}, {Name: "identity_id"}, {Name: "application_id"}, {Name: "environment_id"}},
		DoUpdates: clause.Assignments(map[string]any{
			"status": "FAILED", "last_error_code": strings.TrimSpace(code),
			"last_error_message": trimProjectionError(message), "updated_at": now,
		}),
	}).Create(map[string]any{
		"tenant_id": strings.TrimSpace(event.TenantID), "identity_id": strings.TrimSpace(event.IdentityID),
		"application_id": strings.TrimSpace(event.ApplicationID), "environment_id": strings.TrimSpace(event.EnvironmentID),
		// 源事件失败时这些字段不可得；由 MarkSynchronized 在映射与快照
		// 加载成功后填充。
		"application_code": "", "keycloak_client_id": "", "authorization_revision": 0, "role_config_hash": "",
		"status": "FAILED", "last_error_code": strings.TrimSpace(code), "last_error_message": trimProjectionError(message),
		"created_at": now, "updated_at": now,
	}).Error
}

func trimProjectionError(message string) string {
	message = strings.TrimSpace(message)
	if len(message) > 1000 {
		return message[:1000]
	}
	return message
}

var _ projectionapplication.ProjectionStore = (*ProjectionStore)(nil)
