// Package integrationexample provides a copyable reference implementation for another Go service
// that publishes audit events through a local transactional outbox.
//
// The package intentionally contains no platform-internal imports. A business system can copy this
// directory, implement Store with GORM/MySQL, and keep the business write plus OutboxRecord insert
// in the same database transaction. It must never place application tokens, client secrets, or raw
// password values in Payload or ErrorMessage.
package integrationexample

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

const (
	// AuditIngestPath is the application-authenticated batch endpoint exposed by Basic Platform.
	AuditIngestPath = "/api/v1/integration/audit/events:batch"
	maxBatchSize    = 100
)

// Event is the safe, documented payload sent to the Basic Platform audit-ingestion endpoint.
type Event struct {
	EventID         string         `json:"event_id"`
	ApplicationCode string         `json:"application_code"`
	EnvironmentCode string         `json:"environment_code"`
	ActorType       string         `json:"actor_type,omitempty"`
	ActorID         string         `json:"actor_id,omitempty"`
	ActorName       string         `json:"actor_name,omitempty"`
	OccurredAt      time.Time      `json:"occurred_at"`
	Action          string         `json:"action"`
	ResourceType    string         `json:"resource_type"`
	ResourceID      string         `json:"resource_id,omitempty"`
	ResourceName    string         `json:"resource_name,omitempty"`
	BusinessID      string         `json:"business_id,omitempty"`
	RequestID       string         `json:"request_id,omitempty"`
	TraceID         string         `json:"trace_id,omitempty"`
	CorrelationID   string         `json:"correlation_id,omitempty"`
	Result          string         `json:"result"`
	ReasonCode      string         `json:"reason_code,omitempty"`
	RiskLevel       string         `json:"risk_level,omitempty"`
	Classification  string         `json:"classification,omitempty"`
	Summary         string         `json:"summary,omitempty"`
	Metadata        map[string]any `json:"metadata,omitempty"`
}

// OutboxRecord is the minimum MySQL row an integrated service should persist in the same GORM
// transaction as its business mutation. Payload is the serialized Event only, after the business
// service has redacted sensitive fields.
type OutboxRecord struct {
	ID          string
	EventID     string
	Payload     []byte
	Attempts    uint
	AvailableAt time.Time
}

// Receipt is the receiver acknowledgement for a submitted event. Both ACCEPTED and DUPLICATE are
// terminal successes because Basic Platform deduplicates by the application and event ID boundary.
type Receipt struct {
	EventID string `json:"event_id"`
	Status  string `json:"status"`
}

// Store is implemented by the integrated service using GORM/MySQL. Claim must use a row lock or
// optimistic claim and only return rows that are due. MarkRetry chooses exponential backoff and,
// after the service policy's maximum attempts, moves the record to its own local dead-letter state.
type Store interface {
	Claim(ctx context.Context, workerID string, limit int, staleBefore time.Time) ([]OutboxRecord, error)
	MarkDelivered(ctx context.Context, recordID string, deliveredAt time.Time) error
	MarkRetry(ctx context.Context, recordID, errorCode string, retryAt time.Time) error
	MarkDeadLetter(ctx context.Context, recordID, errorCode string, failedAt time.Time) error
}

// Sender sends a validated batch with an application Bearer token. Implementations must not log
// the authorization header, raw HTTP response body, or serialized event payload.
type Sender interface {
	Send(ctx context.Context, events []Event) ([]Receipt, error)
}

// Clock makes delivery scheduling deterministic in tests and callers that already centralize time.
type Clock interface{ Now() time.Time }

type systemClock struct{}

func (systemClock) Now() time.Time { return time.Now().UTC() }

// Worker delivers one MySQL-claimed batch at a time. No Redis, broker, or browser storage is used.
type Worker struct {
	store       Store
	sender      Sender
	clock       Clock
	workerID    string
	maxAttempts uint
}

// NewWorker builds an outbox worker with a bounded batch and explicit retry policy.
func NewWorker(store Store, sender Sender, workerID string, maxAttempts uint) (*Worker, error) {
	if store == nil || sender == nil || workerID == "" || maxAttempts == 0 {
		return nil, errors.New("audit outbox worker configuration is invalid")
	}
	return &Worker{store: store, sender: sender, clock: systemClock{}, workerID: workerID, maxAttempts: maxAttempts}, nil
}

// DeliverOnce claims up to 100 due rows and records the outcome per event. A malformed local row
// cannot block the rest of the batch; it is moved to the local dead-letter state with a stable code.
func (worker *Worker) DeliverOnce(ctx context.Context) (int, error) {
	now := worker.clock.Now().UTC()
	records, err := worker.store.Claim(ctx, worker.workerID, maxBatchSize, now.Add(-5*time.Minute))
	if err != nil || len(records) == 0 {
		return len(records), err
	}

	events := make([]Event, 0, len(records))
	recordByEventID := make(map[string]OutboxRecord, len(records))
	for _, record := range records {
		var event Event
		if err := json.Unmarshal(record.Payload, &event); err != nil || event.EventID == "" || event.EventID != record.EventID {
			if markErr := worker.store.MarkDeadLetter(ctx, record.ID, "AUDIT_OUTBOX_PAYLOAD_INVALID", now); markErr != nil {
				return len(records), fmt.Errorf("mark invalid audit outbox record dead-letter: %w", markErr)
			}
			continue
		}
		events = append(events, event)
		recordByEventID[event.EventID] = record
	}
	if len(events) == 0 {
		return len(records), nil
	}

	receipts, err := worker.sender.Send(ctx, events)
	if err != nil {
		return len(records), worker.scheduleAll(ctx, recordByEventID, "AUDIT_INGEST_UNAVAILABLE", now)
	}

	receiptByEventID := make(map[string]Receipt, len(receipts))
	for _, receipt := range receipts {
		receiptByEventID[receipt.EventID] = receipt
	}
	for eventID, record := range recordByEventID {
		receipt, ok := receiptByEventID[eventID]
		if !ok {
			if err := worker.schedule(ctx, record, "AUDIT_INGEST_RECEIPT_MISSING", now); err != nil {
				return len(records), err
			}
			continue
		}
		if receipt.Status == "ACCEPTED" || receipt.Status == "DUPLICATE" {
			if err := worker.store.MarkDelivered(ctx, record.ID, now); err != nil {
				return len(records), fmt.Errorf("mark audit outbox delivered: %w", err)
			}
			continue
		}
		if err := worker.schedule(ctx, record, "AUDIT_INGEST_REJECTED", now); err != nil {
			return len(records), err
		}
	}
	return len(records), nil
}

func (worker *Worker) scheduleAll(ctx context.Context, records map[string]OutboxRecord, code string, now time.Time) error {
	for _, record := range records {
		if err := worker.schedule(ctx, record, code, now); err != nil {
			return err
		}
	}
	return nil
}

func (worker *Worker) schedule(ctx context.Context, record OutboxRecord, code string, now time.Time) error {
	if record.Attempts+1 >= worker.maxAttempts {
		if err := worker.store.MarkDeadLetter(ctx, record.ID, code, now); err != nil {
			return fmt.Errorf("mark audit outbox dead-letter: %w", err)
		}
		return nil
	}
	if err := worker.store.MarkRetry(ctx, record.ID, code, now.Add(retryDelay(record.Attempts))); err != nil {
		return fmt.Errorf("schedule audit outbox retry: %w", err)
	}
	return nil
}

func retryDelay(attempt uint) time.Duration {
	if attempt > 5 {
		attempt = 5
	}
	return time.Second * time.Duration(1<<attempt)
}
