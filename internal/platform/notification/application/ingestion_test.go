package application

import (
	"testing"
	"time"

	"github.com/J-S-Te/Basic-Platform/internal/platform/notification/domain"
)

func TestValidateIngestInputAcceptsPlatformEvent(t *testing.T) {
	input := validIngestInput()
	if err := validateIngestInput(input); err != nil {
		t.Fatalf("validate valid ingestion input: %v", err)
	}
}

func TestValidateIngestInputRejectsUnsupportedScopeAndOversizedIdempotency(t *testing.T) {
	input := validIngestInput()
	input.Event.NotificationScope = "LOCAL"
	if err := validateIngestInput(input); err != ErrValidation {
		t.Fatalf("scope error = %v, want validation", err)
	}
	input = validIngestInput()
	input.SourceApplication = "APPLICATION_WITH_A_VERY_LONG_CODE_THAT_MAKES_THE_COMPOSITE_IDEMPOTENCY_KEY_TOO_LARGE_FOR_THE_LEGACY_MESSAGE_COLUMN"
	input.SourceEnvironment = "PRODUCTION"
	input.Event.IdempotencyKey = "EVENT_WITH_A_VERY_LONG_IDEMPOTENCY_KEY_THAT_CANNOT_BE_STORED_WITH_THE_SOURCE_PREFIX_AND_MUST_BE_REJECTED_SAFELY"
	if err := validateIngestInput(input); err != ErrValidation {
		t.Fatalf("composite idempotency error = %v, want validation", err)
	}
}

func validIngestInput() IngestInput {
	return IngestInput{TenantID: "01H00000000000000000000000", SourceApplication: "CRM", SourceEnvironment: "PROD", Event: domain.IngestionEvent{EventID: "EVENT_001", EventType: "OPPORTUNITY_APPROVED", NotificationScope: "CROSS_SYSTEM", Priority: "HIGH", Title: "审批通过", Content: "业务已审批通过", TargetURL: "/opportunities/1", IdempotencyKey: "event-001", Recipients: []string{"01H00000000000000000000001"}, OccurredAt: time.Date(2026, 8, 20, 0, 0, 0, 0, time.UTC)}}
}
