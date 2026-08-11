package infrastructure

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/J-S-Te/Basic-Platform/internal/shared/ulid"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

type keycloakBackfillLegacyEvent struct {
	ID, IdentityID, EventType string
}

type keycloakBackfillLegacyScope struct {
	TenantID, ApplicationID string
}

// BackfillKeycloakAuthorization creates the initial FULL_RECONCILE work only
// after a mapping has been persisted.  All writes, including the idempotency
// ledger, share one transaction so an interrupted request can safely retry.
func (store *ClientMappingStore) BackfillKeycloakAuthorization(ctx context.Context, tenantID, applicationID, environmentID string) error {
	if store == nil || store.database == nil {
		return errors.New("Keycloak authorization backfill database must not be nil")
	}
	tenantID, applicationID, environmentID = strings.TrimSpace(tenantID), strings.TrimSpace(applicationID), strings.TrimSpace(environmentID)
	if tenantID == "" || applicationID == "" || environmentID == "" {
		return errors.New("Keycloak authorization backfill tenant, application and environment are required")
	}
	now := time.Now().UTC()
	return store.database.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var identityIDs []string
		if err := activeKeycloakBackfillUsersQuery(tx, tenantID).Pluck("id", &identityIDs).Error; err != nil {
			return fmt.Errorf("load active Keycloak backfill users: %w", err)
		}
		for _, identityID := range identityIDs {
			if err := enqueueInitialKeycloakReconcile(tx, tenantID, strings.TrimSpace(identityID), applicationID, environmentID, now); err != nil {
				return err
			}
		}
		return expandLegacyKeycloakOutbox(tx, tenantID, applicationID, now)
	})
}

// ExpandLegacyKeycloakAuthorizationOutbox expands every pending or running
// pre-environment outbox event into one event per synchronized Client mapping.
// It is safe to invoke at worker startup and on every worker loop: the
// expansion ledger makes each source-event/environment pair idempotent.
//
// Legacy events without a synchronized mapping deliberately remain pending or
// running. A later invocation will expand them once a Client mapping exists.
func (store *ClientMappingStore) ExpandLegacyKeycloakAuthorizationOutbox(ctx context.Context) error {
	if store == nil || store.database == nil {
		return errors.New("Keycloak authorization legacy expansion database must not be nil")
	}
	now := time.Now().UTC()
	return store.database.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var scopes []keycloakBackfillLegacyScope
		if err := pendingLegacyKeycloakOutboxScopesQuery(tx).Find(&scopes).Error; err != nil {
			return fmt.Errorf("load legacy Keycloak outbox scopes: %w", err)
		}
		for _, scope := range scopes {
			if err := expandLegacyKeycloakOutbox(tx, scope.TenantID, scope.ApplicationID, now); err != nil {
				return fmt.Errorf("expand legacy Keycloak outbox for tenant %s application %s: %w", scope.TenantID, scope.ApplicationID, err)
			}
		}
		return nil
	})
}

func activeKeycloakBackfillUsersQuery(database *gorm.DB, tenantID string) *gorm.DB {
	return database.Table("iam_user").Where("tenant_id = ? AND status = ?", strings.TrimSpace(tenantID), "ACTIVE").Order("id ASC")
}

func synchronizedKeycloakBackfillTargetsQuery(database *gorm.DB, tenantID, applicationID string) *gorm.DB {
	return database.Table("keycloak_application_client_mapping").Select("environment_id").
		Where("tenant_id = ? AND application_id = ? AND status = ?", strings.TrimSpace(tenantID), strings.TrimSpace(applicationID), "SYNCED").Order("environment_id ASC")
}

func pendingLegacyKeycloakOutboxQuery(database *gorm.DB, tenantID, applicationID string) *gorm.DB {
	return database.Table("keycloak_authorization_outbox").Select("id, identity_id, event_type").
		Where("tenant_id = ? AND application_id = ? AND environment_id IS NULL", strings.TrimSpace(tenantID), strings.TrimSpace(applicationID)).
		Where("status IN ?", []string{"PENDING", "RUNNING"}).Order("created_at ASC, id ASC")
}

func pendingLegacyKeycloakOutboxScopesQuery(database *gorm.DB) *gorm.DB {
	return database.Table("keycloak_authorization_outbox").Select("tenant_id, application_id").
		Where("environment_id IS NULL").Where("status IN ?", []string{"PENDING", "RUNNING"}).
		Group("tenant_id, application_id").Order("tenant_id ASC, application_id ASC")
}

func enqueueInitialKeycloakReconcile(tx *gorm.DB, tenantID, identityID, applicationID, environmentID string, now time.Time) error {
	if identityID == "" {
		return nil
	}
	eventID, err := ulid.New(now)
	if err != nil {
		return fmt.Errorf("generate Keycloak reconciliation outbox ID: %w", err)
	}
	inserted, err := insertKeycloakBackfillLedger(tx, "keycloak_authorization_reconcile_backfill", map[string]any{
		"tenant_id": tenantID, "identity_id": identityID, "application_id": applicationID, "environment_id": environmentID,
		"outbox_event_id": eventID, "created_at": now,
	})
	if err != nil || !inserted {
		return err
	}
	if err := createKeycloakBackfillOutbox(tx, eventID, tenantID, identityID, applicationID, environmentID, "FULL_RECONCILE", now); err != nil {
		return fmt.Errorf("enqueue initial Keycloak reconciliation: %w", err)
	}
	return nil
}

func expandLegacyKeycloakOutbox(tx *gorm.DB, tenantID, applicationID string, now time.Time) error {
	var environmentIDs []string
	if err := synchronizedKeycloakBackfillTargetsQuery(tx, tenantID, applicationID).Pluck("environment_id", &environmentIDs).Error; err != nil {
		return fmt.Errorf("load Keycloak expansion targets: %w", err)
	}
	if len(environmentIDs) == 0 {
		return nil
	}
	var legacyEvents []keycloakBackfillLegacyEvent
	if err := pendingLegacyKeycloakOutboxQuery(tx, tenantID, applicationID).Find(&legacyEvents).Error; err != nil {
		return fmt.Errorf("load legacy Keycloak outbox events: %w", err)
	}
	for _, legacy := range legacyEvents {
		for _, environmentID := range environmentIDs {
			eventID, err := ulid.New(now)
			if err != nil {
				return fmt.Errorf("generate expanded Keycloak outbox ID: %w", err)
			}
			inserted, err := insertKeycloakBackfillLedger(tx, "keycloak_authorization_outbox_expansion", map[string]any{
				"source_outbox_event_id": legacy.ID, "environment_id": environmentID, "outbox_event_id": eventID, "created_at": now,
			})
			if err != nil {
				return fmt.Errorf("record legacy Keycloak outbox expansion: %w", err)
			}
			if inserted {
				if err := createKeycloakBackfillOutbox(tx, eventID, tenantID, legacy.IdentityID, applicationID, environmentID, legacy.EventType, now); err != nil {
					return fmt.Errorf("enqueue expanded Keycloak outbox event: %w", err)
				}
			}
		}
		// Once every currently synchronized target has a durable child event, the
		// NULL-environment source must no longer be claimed and retried forever.
		if err := tx.Table("keycloak_authorization_outbox").Where("id = ? AND environment_id IS NULL AND status IN ?", legacy.ID, []string{"PENDING", "RUNNING"}).Updates(map[string]any{
			"status": "SUCCEEDED", "completed_at": now, "locked_by": nil, "locked_at": nil,
		}).Error; err != nil {
			return fmt.Errorf("complete expanded legacy Keycloak outbox event: %w", err)
		}
	}
	return nil
}

func insertKeycloakBackfillLedger(tx *gorm.DB, table string, values map[string]any) (bool, error) {
	// MySQL rejects an empty ON DUPLICATE KEY UPDATE clause. Keep the insert
	// idempotent with a valid no-op assignment instead of relying on GORM's
	// dialect-specific DoNothing rendering.
	result := tx.Table(table).Clauses(clause.OnConflict{
		DoUpdates: clause.Assignments(map[string]any{"created_at": gorm.Expr("created_at")}),
	}).Create(values)
	if result.Error != nil {
		return false, result.Error
	}
	return result.RowsAffected == 1, nil
}

func createKeycloakBackfillOutbox(tx *gorm.DB, eventID, tenantID, identityID, applicationID, environmentID, eventType string, now time.Time) error {
	return tx.Table("keycloak_authorization_outbox").Create(map[string]any{
		"id": eventID, "tenant_id": tenantID, "identity_id": identityID, "application_id": applicationID, "environment_id": environmentID,
		"event_type": eventType, "authorization_revision": 0, "status": "PENDING", "available_at": now, "attempts": 0, "created_at": now,
	}).Error
}
