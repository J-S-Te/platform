// Package application contains the operator-facing, tenant-scoped recovery
// contract for Keycloak authorization projections.  It deliberately does not
// expose the worker queue itself: callers can inspect durable failures and
// request one safe replay, but cannot alter an event's tenant, subject or
// target application.
package application

import (
	"context"
	"errors"
	"strings"
	"time"
)

var (
	ErrOperationsValidation = errors.New("invalid Keycloak projection operation")
	ErrProjectionNotFound   = errors.New("Keycloak projection event not found")
	ErrProjectionConflict   = errors.New("Keycloak projection event cannot be replayed")
)

const (
	defaultFailurePageSize = 20
	maxFailurePageSize     = 100
)

// FailurePageRequest limits the management view to the tenant and optional
// application/environment filters.  It never accepts an arbitrary SQL sort
// field or an identity selector from the browser.
type FailurePageRequest struct {
	Page, PageSize               int
	ApplicationCode, Environment string
}

type FailedProjection struct {
	EventID         string    `json:"event_id"`
	IdentityID      string    `json:"identity_id"`
	ApplicationID   string    `json:"application_id"`
	EnvironmentID   string    `json:"environment_id,omitempty"`
	ApplicationCode string    `json:"application_code"`
	Environment     string    `json:"environment,omitempty"`
	EventType       string    `json:"event_type"`
	Attempts        uint      `json:"attempts"`
	FailedAt        time.Time `json:"failed_at"`
	ErrorCode       string    `json:"error_code,omitempty"`
	ErrorMessage    string    `json:"error_message,omitempty"`
	BlocksCutover   bool      `json:"blocks_cutover"`
}

type FailurePageResult struct {
	Items    []FailedProjection `json:"items"`
	Page     int                `json:"page"`
	PageSize int                `json:"page_size"`
	Total    int64              `json:"total"`
}

// AlertStatus is a non-sensitive operational signal: FAILED events are
// durable dead letters and therefore block Keycloak issuer cutover for their
// concrete environment.  No Client secret, token or outbox payload is part of
// this representation.
type AlertStatus struct {
	Severity                 string     `json:"severity"`
	State                    string     `json:"state"`
	Summary                  string     `json:"summary"`
	FailedCount              int64      `json:"failed_count"`
	AffectedEnvironmentCount int64      `json:"affected_environment_count"`
	OldestFailedAt           *time.Time `json:"oldest_failed_at,omitempty"`
}

type ReplayInput struct {
	TenantID, EventID, OperatorID, Confirmation, Reason string
}

type ReplayResult struct {
	EventID        string    `json:"event_id"`
	Replayed       bool      `json:"replayed"`
	AlreadyPending bool      `json:"already_pending"`
	AvailableAt    time.Time `json:"available_at"`
}

type OperationsStore interface {
	ListFailedProjections(context.Context, string, FailurePageRequest) (FailurePageResult, error)
	GetProjectionAlertStatus(context.Context, string) (AlertStatus, error)
	ReplayFailedProjection(context.Context, ReplayInput, time.Time) (ReplayResult, error)
}

type Operations struct {
	store OperationsStore
	clock Clock
}

func NewOperations(store OperationsStore, clocks ...Clock) (*Operations, error) {
	if store == nil || len(clocks) > 1 {
		return nil, errors.New("Keycloak projection operations dependencies are invalid")
	}
	clock := Clock(systemClock{})
	if len(clocks) == 1 {
		if clocks[0] == nil {
			return nil, errors.New("Keycloak projection operations clock must not be nil")
		}
		clock = clocks[0]
	}
	return &Operations{store: store, clock: clock}, nil
}

func (service *Operations) ListFailed(ctx context.Context, tenantID string, query FailurePageRequest) (FailurePageResult, error) {
	if strings.TrimSpace(tenantID) == "" {
		return FailurePageResult{}, ErrOperationsValidation
	}
	query = normalizeFailurePageQuery(query)
	return service.store.ListFailedProjections(ctx, strings.TrimSpace(tenantID), query)
}

func (service *Operations) AlertStatus(ctx context.Context, tenantID string) (AlertStatus, error) {
	if strings.TrimSpace(tenantID) == "" {
		return AlertStatus{}, ErrOperationsValidation
	}
	return service.store.GetProjectionAlertStatus(ctx, strings.TrimSpace(tenantID))
}

// Replay only changes FAILED -> PENDING.  The store performs the conditional
// transition under a row lock; a repeated POST sees PENDING/RUNNING and returns
// an idempotent success without enqueueing a duplicate event.
func (service *Operations) Replay(ctx context.Context, input ReplayInput) (ReplayResult, error) {
	input.TenantID = strings.TrimSpace(input.TenantID)
	input.EventID = strings.TrimSpace(input.EventID)
	input.OperatorID = strings.TrimSpace(input.OperatorID)
	input.Confirmation = strings.TrimSpace(input.Confirmation)
	input.Reason = strings.TrimSpace(input.Reason)
	if input.TenantID == "" || input.EventID == "" || input.OperatorID == "" || input.Confirmation != input.EventID || len(input.Reason) < 6 || len(input.Reason) > 500 {
		return ReplayResult{}, ErrOperationsValidation
	}
	return service.store.ReplayFailedProjection(ctx, input, service.clock.Now().UTC())
}

func normalizeFailurePageQuery(query FailurePageRequest) FailurePageRequest {
	if query.Page < 1 {
		query.Page = 1
	}
	if query.PageSize < 1 {
		query.PageSize = defaultFailurePageSize
	}
	if query.PageSize > maxFailurePageSize {
		query.PageSize = maxFailurePageSize
	}
	query.ApplicationCode = strings.TrimSpace(query.ApplicationCode)
	query.Environment = strings.ToLower(strings.TrimSpace(query.Environment))
	return query
}
