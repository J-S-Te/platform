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

// ProjectionStore persists the outcome of Keycloak authorization projection.
// environment_id is intentionally represented even though the current worker
// event is application-scoped: a future fan-out worker can populate it without
// changing this adapter's storage contract.
type ProjectionStore struct{ database *gorm.DB }

func NewProjectionStore(database *gorm.DB) (*ProjectionStore, error) {
	if database == nil {
		return nil, errors.New("Keycloak authorization projection database must not be nil")
	}
	return &ProjectionStore{database: database}, nil
}

func (store *ProjectionStore) MarkSynchronized(ctx context.Context, snapshot projectionapplication.Snapshot, synchronizedAt time.Time) error {
	// 同步成功状态按复合业务键幂等 upsert，记录的是平台快照已落到 Keycloak 的修订。
	now := synchronizedAt.UTC()
	return store.database.WithContext(ctx).Table("keycloak_authorization_projection").Clauses(clause.OnConflict{
		Columns: []clause.Column{{Name: "tenant_id"}, {Name: "identity_id"}, {Name: "application_id"}, {Name: "environment_id"}},
		DoUpdates: clause.Assignments(map[string]any{
			"application_code":       strings.TrimSpace(snapshot.ApplicationCode),
			"keycloak_client_id":     strings.TrimSpace(snapshot.KeycloakClientID),
			"authorization_revision": snapshot.AuthorizationRevision,
			"role_config_hash":       strings.TrimSpace(snapshot.RoleConfigHash),
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
		"role_config_hash": strings.TrimSpace(snapshot.RoleConfigHash), "status": "SYNCED", "last_synced_at": now,
		"created_at": now, "updated_at": now,
	}).Error
}

func (store *ProjectionStore) MarkFailed(ctx context.Context, event projectionworker.Event, code, message string, failedAt time.Time) error {
	// 失败也要落库，使 outbox 重试之外仍保留最后一次投影诊断状态。
	now := failedAt.UTC()
	// A source failure can happen before a successful projection row exists.  An
	// upsert records that failure instead of losing the only diagnostic signal.
	return store.database.WithContext(ctx).Table("keycloak_authorization_projection").Clauses(clause.OnConflict{
		Columns: []clause.Column{{Name: "tenant_id"}, {Name: "identity_id"}, {Name: "application_id"}, {Name: "environment_id"}},
		DoUpdates: clause.Assignments(map[string]any{
			"status": "FAILED", "last_error_code": strings.TrimSpace(code),
			"last_error_message": trimProjectionError(message), "updated_at": now,
		}),
	}).Create(map[string]any{
		"tenant_id": strings.TrimSpace(event.TenantID), "identity_id": strings.TrimSpace(event.IdentityID),
		"application_id": strings.TrimSpace(event.ApplicationID), "environment_id": strings.TrimSpace(event.EnvironmentID),
		// These fields are not available on a failed source event. They are
		// populated by MarkSynchronized after the mapping and snapshot load.
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
