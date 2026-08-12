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

// CutoverLifecycleStore persists the explicit observation/cutover/rollback
// state.  It is deliberately fail-closed: absent state means NOT_STARTED and
// a caller cannot infer that a prior synchronous Keycloak operation was safe.
type CutoverLifecycleStore struct{ database *gorm.DB }

func NewCutoverLifecycleStore(database *gorm.DB) (*CutoverLifecycleStore, error) {
	if database == nil {
		return nil, errors.New("Keycloak cutover lifecycle database must not be nil")
	}
	return &CutoverLifecycleStore{database: database}, nil
}

type cutoverScope struct {
	ApplicationID string `gorm:"column:application_id"`
	EnvironmentID string `gorm:"column:environment_id"`
}

type cutoverLifecycleRow struct {
	Status               string     `gorm:"column:status"`
	ObservationStartedAt *time.Time `gorm:"column:observation_started_at"`
	ObservationEndsAt    *time.Time `gorm:"column:observation_ends_at"`
	SwitchedAt           *time.Time `gorm:"column:switched_at"`
	RollbackDeadlineAt   *time.Time `gorm:"column:rollback_deadline_at"`
	RolledBackAt         *time.Time `gorm:"column:rolled_back_at"`
}

func (store *CutoverLifecycleStore) GetKeycloakCutoverLifecycle(ctx context.Context, tenantID, applicationCode, environment string) (registryhttp.KeycloakCutoverLifecycle, error) {
	scope, err := store.scope(ctx, tenantID, applicationCode, environment)
	if err != nil {
		return registryhttp.KeycloakCutoverLifecycle{}, err
	}
	var row cutoverLifecycleRow
	err = store.database.WithContext(ctx).Table("keycloak_cutover_lifecycle").
		Where("tenant_id = ? AND application_id = ? AND environment_id = ?", clean(tenantID), scope.ApplicationID, scope.EnvironmentID).
		Take(&row).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return registryhttp.KeycloakCutoverLifecycle{Status: "NOT_STARTED"}, nil
	}
	if err != nil {
		return registryhttp.KeycloakCutoverLifecycle{}, fmt.Errorf("load Keycloak cutover lifecycle: %w", err)
	}
	return toCutoverLifecycle(row), nil
}

func (store *CutoverLifecycleStore) ListKeycloakCutoverTimeline(ctx context.Context, tenantID, applicationCode, environment string, limit int) ([]registryhttp.KeycloakCutoverTimelineEvent, error) {
	scope, err := store.scope(ctx, tenantID, applicationCode, environment)
	if err != nil {
		return nil, err
	}
	if limit < 1 || limit > 100 {
		limit = 50
	}
	var rows []struct {
		ID         string    `gorm:"column:id"`
		EventType  string    `gorm:"column:event_type"`
		ActorID    *string   `gorm:"column:actor_id"`
		Summary    string    `gorm:"column:summary"`
		OccurredAt time.Time `gorm:"column:occurred_at"`
	}
	if err := store.database.WithContext(ctx).Table("keycloak_cutover_timeline_event").
		Select("id, event_type, actor_id, summary, occurred_at").
		Where("tenant_id = ? AND application_id = ? AND environment_id = ?", clean(tenantID), scope.ApplicationID, scope.EnvironmentID).
		Order("occurred_at DESC").Limit(limit).Find(&rows).Error; err != nil {
		return nil, fmt.Errorf("list Keycloak cutover timeline: %w", err)
	}
	items := make([]registryhttp.KeycloakCutoverTimelineEvent, 0, len(rows))
	for _, row := range rows {
		actorID := ""
		if row.ActorID != nil {
			actorID = clean(*row.ActorID)
		}
		items = append(items, registryhttp.KeycloakCutoverTimelineEvent{ID: clean(row.ID), EventType: clean(row.EventType), ActorID: actorID, Summary: clean(row.Summary), OccurredAt: row.OccurredAt.UTC()})
	}
	return items, nil
}

func (store *CutoverLifecycleStore) StartKeycloakObservation(ctx context.Context, tenantID, applicationID, environmentID, actorID string, duration time.Duration) (registryhttp.KeycloakCutoverLifecycle, error) {
	if duration <= 0 {
		return registryhttp.KeycloakCutoverLifecycle{}, errors.New("Keycloak observation duration must be positive")
	}
	return store.transition(ctx, tenantID, applicationID, environmentID, actorID, "OBSERVATION_STARTED", "已开始 Keycloak 七天观察期", func(now time.Time, current cutoverLifecycleRow, exists bool) (cutoverLifecycleRow, error) {
		if exists && (current.Status == "OBSERVING" || current.Status == "READY_TO_SWITCH" || current.Status == "SWITCHED") {
			return cutoverLifecycleRow{}, errors.New("Keycloak observation already exists")
		}
		end := now.Add(duration)
		return cutoverLifecycleRow{Status: "OBSERVING", ObservationStartedAt: &now, ObservationEndsAt: &end}, nil
	})
}

// CanKeycloakCutover is the fail-closed preflight used before the deployment
// Agent changes an issuer.  Confirmation is deliberately recorded only after
// that Agent reports success, so a failed deployment cannot look switched in
// the durable history.
func (store *CutoverLifecycleStore) CanKeycloakCutover(ctx context.Context, tenantID, applicationCode, environment string) error {
	lifecycle, err := store.GetKeycloakCutoverLifecycle(ctx, tenantID, applicationCode, environment)
	if err != nil {
		return err
	}
	if lifecycle.ObservationEndsAt == nil || time.Now().UTC().Before(lifecycle.ObservationEndsAt.UTC()) {
		return errors.New("Keycloak observation window has not completed")
	}
	if lifecycle.Status == "SWITCHED" {
		return errors.New("Keycloak environment is already switched")
	}
	return nil
}

func (store *CutoverLifecycleStore) CanKeycloakRollback(ctx context.Context, tenantID, applicationCode, environment string) error {
	lifecycle, err := store.GetKeycloakCutoverLifecycle(ctx, tenantID, applicationCode, environment)
	if err != nil {
		return err
	}
	if lifecycle.Status != "SWITCHED" {
		return errors.New("Keycloak environment is not switched")
	}
	if lifecycle.RollbackDeadlineAt != nil && time.Now().UTC().After(lifecycle.RollbackDeadlineAt.UTC()) {
		return errors.New("Keycloak rollback window has expired")
	}
	return nil
}

func (store *CutoverLifecycleStore) ConfirmKeycloakCutover(ctx context.Context, tenantID, applicationID, environmentID, actorID string, rollbackWindow time.Duration) (registryhttp.KeycloakCutoverLifecycle, error) {
	if rollbackWindow <= 0 {
		return registryhttp.KeycloakCutoverLifecycle{}, errors.New("Keycloak rollback window must be positive")
	}
	return store.transition(ctx, tenantID, applicationID, environmentID, actorID, "CUTOVER_COMPLETED", "已切换到 Keycloak，回滚窗口已开始", func(now time.Time, current cutoverLifecycleRow, exists bool) (cutoverLifecycleRow, error) {
		if !exists || current.ObservationEndsAt == nil {
			return cutoverLifecycleRow{}, errors.New("Keycloak observation has not started")
		}
		if now.Before(current.ObservationEndsAt.UTC()) {
			return cutoverLifecycleRow{}, errors.New("Keycloak observation window has not completed")
		}
		if current.Status == "SWITCHED" {
			return cutoverLifecycleRow{}, errors.New("Keycloak environment is already switched")
		}
		deadline := now.Add(rollbackWindow)
		return cutoverLifecycleRow{Status: "SWITCHED", ObservationStartedAt: current.ObservationStartedAt, ObservationEndsAt: current.ObservationEndsAt, SwitchedAt: &now, RollbackDeadlineAt: &deadline}, nil
	})
}

func (store *CutoverLifecycleStore) RecordKeycloakRollback(ctx context.Context, tenantID, applicationID, environmentID, actorID string) (registryhttp.KeycloakCutoverLifecycle, error) {
	return store.transition(ctx, tenantID, applicationID, environmentID, actorID, "ROLLBACK_COMPLETED", "已回滚到基础平台 OIDC", func(now time.Time, current cutoverLifecycleRow, exists bool) (cutoverLifecycleRow, error) {
		if !exists || current.Status != "SWITCHED" {
			return cutoverLifecycleRow{}, errors.New("Keycloak environment is not switched")
		}
		if current.RollbackDeadlineAt != nil && now.After(current.RollbackDeadlineAt.UTC()) {
			return cutoverLifecycleRow{}, errors.New("Keycloak rollback window has expired")
		}
		return cutoverLifecycleRow{Status: "ROLLED_BACK", ObservationStartedAt: current.ObservationStartedAt, ObservationEndsAt: current.ObservationEndsAt, SwitchedAt: current.SwitchedAt, RollbackDeadlineAt: current.RollbackDeadlineAt, RolledBackAt: &now}, nil
	})
}

func (store *CutoverLifecycleStore) transition(ctx context.Context, tenantID, applicationID, environmentID, actorID, eventType, summary string, transition func(time.Time, cutoverLifecycleRow, bool) (cutoverLifecycleRow, error)) (registryhttp.KeycloakCutoverLifecycle, error) {
	tenantID, applicationID, environmentID, actorID = clean(tenantID), clean(applicationID), clean(environmentID), clean(actorID)
	if tenantID == "" || applicationID == "" || environmentID == "" || actorID == "" {
		return registryhttp.KeycloakCutoverLifecycle{}, errors.New("incomplete Keycloak cutover scope")
	}
	var result cutoverLifecycleRow
	err := store.database.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var current cutoverLifecycleRow
		err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Table("keycloak_cutover_lifecycle").
			Where("tenant_id = ? AND application_id = ? AND environment_id = ?", tenantID, applicationID, environmentID).Take(&current).Error
		exists := err == nil
		if err != nil && !errors.Is(err, gorm.ErrRecordNotFound) {
			return err
		}
		now := time.Now().UTC()
		next, transitionErr := transition(now, current, exists)
		if transitionErr != nil {
			return transitionErr
		}
		values := map[string]any{"status": next.Status, "observation_started_at": next.ObservationStartedAt, "observation_ends_at": next.ObservationEndsAt, "switched_at": next.SwitchedAt, "rollback_deadline_at": next.RollbackDeadlineAt, "rolled_back_at": next.RolledBackAt, "updated_at": now}
		if exists {
			if err := tx.Table("keycloak_cutover_lifecycle").Where("tenant_id = ? AND application_id = ? AND environment_id = ?", tenantID, applicationID, environmentID).Updates(values).Error; err != nil {
				return err
			}
		} else {
			values["tenant_id"], values["application_id"], values["environment_id"], values["created_at"] = tenantID, applicationID, environmentID, now
			if err := tx.Table("keycloak_cutover_lifecycle").Create(values).Error; err != nil {
				return err
			}
		}
		id, err := platformulid.New(now)
		if err != nil {
			return err
		}
		if err := tx.Table("keycloak_cutover_timeline_event").Create(map[string]any{"id": id, "tenant_id": tenantID, "application_id": applicationID, "environment_id": environmentID, "event_type": eventType, "actor_id": actorID, "summary": summary, "occurred_at": now}).Error; err != nil {
			return err
		}
		result = next
		return nil
	})
	if err != nil {
		return registryhttp.KeycloakCutoverLifecycle{}, fmt.Errorf("transition Keycloak cutover lifecycle: %w", err)
	}
	return toCutoverLifecycle(result), nil
}

func (store *CutoverLifecycleStore) scope(ctx context.Context, tenantID, applicationCode, environment string) (cutoverScope, error) {
	var scope cutoverScope
	err := store.database.WithContext(ctx).Table("platform_application AS application").
		Select("application.id AS application_id, environment.id AS environment_id").
		Joins("JOIN platform_application_environment AS environment ON environment.tenant_id = application.tenant_id AND environment.application_id = application.id").
		Where("application.tenant_id = ? AND application.code = ? AND environment.environment = ?", clean(tenantID), clean(applicationCode), strings.ToLower(clean(environment))).Take(&scope).Error
	if err != nil {
		return cutoverScope{}, fmt.Errorf("resolve Keycloak cutover scope: %w", err)
	}
	return scope, nil
}

func toCutoverLifecycle(row cutoverLifecycleRow) registryhttp.KeycloakCutoverLifecycle {
	status := clean(row.Status)
	// Reaching the deadline is a deterministic, durable fact.  The read model
	// exposes READY_TO_SWITCH immediately so an operator does not need a timer
	// process or a page refresh side effect before the guarded switch request.
	if status == "OBSERVING" && row.ObservationEndsAt != nil && !time.Now().UTC().Before(row.ObservationEndsAt.UTC()) {
		status = "READY_TO_SWITCH"
	}
	return registryhttp.KeycloakCutoverLifecycle{Status: status, ObservationStartedAt: row.ObservationStartedAt, ObservationEndsAt: row.ObservationEndsAt, SwitchedAt: row.SwitchedAt, RollbackDeadlineAt: row.RollbackDeadlineAt, RolledBackAt: row.RolledBackAt}
}

func clean(value string) string { return strings.TrimSpace(value) }

var _ interface {
	GetKeycloakCutoverLifecycle(context.Context, string, string, string) (registryhttp.KeycloakCutoverLifecycle, error)
	ListKeycloakCutoverTimeline(context.Context, string, string, string, int) ([]registryhttp.KeycloakCutoverTimelineEvent, error)
	StartKeycloakObservation(context.Context, string, string, string, string, time.Duration) (registryhttp.KeycloakCutoverLifecycle, error)
	CanKeycloakCutover(context.Context, string, string, string) error
	CanKeycloakRollback(context.Context, string, string, string) error
	ConfirmKeycloakCutover(context.Context, string, string, string, string, time.Duration) (registryhttp.KeycloakCutoverLifecycle, error)
	RecordKeycloakRollback(context.Context, string, string, string, string) (registryhttp.KeycloakCutoverLifecycle, error)
} = (*CutoverLifecycleStore)(nil)
