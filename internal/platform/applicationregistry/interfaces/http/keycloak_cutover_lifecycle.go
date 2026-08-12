package http

import (
	"context"
	"time"
)

// KeycloakCutoverLifecycle is the durable migration state for exactly one
// application environment.  It is intentionally separate from deployment
// health: a running service does not make an authentication cutover safe.
// No secret, token or Client credential is part of this projection.
type KeycloakCutoverLifecycle struct {
	Status               string     `json:"status"`
	ObservationStartedAt *time.Time `json:"observation_started_at,omitempty"`
	ObservationEndsAt    *time.Time `json:"observation_ends_at,omitempty"`
	SwitchedAt           *time.Time `json:"switched_at,omitempty"`
	RollbackDeadlineAt   *time.Time `json:"rollback_deadline_at,omitempty"`
	RolledBackAt         *time.Time `json:"rolled_back_at,omitempty"`
}

// KeycloakCutoverTimelineEvent is append-only evidence used by the console.
type KeycloakCutoverTimelineEvent struct {
	ID         string    `json:"id,omitempty"`
	EventType  string    `json:"event_type"`
	Summary    string    `json:"summary"`
	ActorID    string    `json:"actor_id,omitempty"`
	OccurredAt time.Time `json:"occurred_at"`
}

// keycloakCutoverLifecycleStore is intentionally a narrow control-plane
// contract.  It lets the HTTP boundary enforce an observation window without
// coupling application directory management to a storage implementation.
type keycloakCutoverLifecycleStore interface {
	GetKeycloakCutoverLifecycle(context.Context, string, string, string) (KeycloakCutoverLifecycle, error)
	ListKeycloakCutoverTimeline(context.Context, string, string, string, int) ([]KeycloakCutoverTimelineEvent, error)
	StartKeycloakObservation(context.Context, string, string, string, string, time.Duration) (KeycloakCutoverLifecycle, error)
	CanKeycloakCutover(context.Context, string, string, string) error
	CanKeycloakRollback(context.Context, string, string, string) error
	ConfirmKeycloakCutover(context.Context, string, string, string, string, time.Duration) (KeycloakCutoverLifecycle, error)
	RecordKeycloakRollback(context.Context, string, string, string, string) (KeycloakCutoverLifecycle, error)
}
