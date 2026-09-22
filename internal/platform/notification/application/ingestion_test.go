package application

import (
	"context"
	"testing"
	"time"

	"github.com/J-S-Te/Basic-Platform/internal/platform/notification/domain"
)

type ingestionTestRepository struct {
	Repository
	accepted    bool
	receiptArgs []string
}

func (r *ingestionTestRepository) CreateTemplate(_ context.Context, template domain.Template, version domain.TemplateVersion) (domain.Template, domain.TemplateVersion, error) {
	return template, version, nil
}

func (r *ingestionTestRepository) AcceptIngestion(context.Context, string, string, string, domain.IngestionEvent, string, time.Time) (domain.IngestionReceipt, error) {
	r.accepted = true
	return domain.IngestionReceipt{Status: domain.IngestionStatusAccepted}, nil
}

func (r *ingestionTestRepository) GetIngestionReceipt(_ context.Context, tenantID, sourceApplication, sourceEnvironment, receiptID string) (domain.IngestionReceipt, error) {
	r.receiptArgs = []string{tenantID, sourceApplication, sourceEnvironment, receiptID}
	return domain.IngestionReceipt{ReceiptID: receiptID, Status: domain.IngestionStatusCompleted}, nil
}

type ingestionTestPolicy struct{ enabled bool }

func (p ingestionTestPolicy) InboxEnabled(context.Context, string) (bool, error) {
	return p.enabled, nil
}

type ingestionTestResolver struct{ users []string }

func (r ingestionTestResolver) ResolveRecipients(context.Context, string, []domain.RecipientTarget, time.Time) ([]string, error) {
	return r.users, nil
}

type ingestionTestIDs struct{}

func (ingestionTestIDs) New(time.Time) (string, error) { return "01H00000000000000000000001", nil }

type ingestionTestClock struct{}

func (ingestionTestClock) Now() time.Time { return time.Date(2026, 8, 20, 0, 0, 0, 0, time.UTC) }

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

func TestIngestRejectsKnownUndeliverableAudienceBeforeAccepting(t *testing.T) {
	repository := &ingestionTestRepository{}
	service, err := NewService(repository, ingestionTestPolicy{enabled: true}, ingestionTestResolver{}, ingestionTestIDs{}, ingestionTestClock{})
	if err != nil {
		t.Fatalf("new service: %v", err)
	}
	_, err = service.Ingest(context.Background(), validIngestInput())
	if err != ErrNoRecipients {
		t.Fatalf("ingest error=%v, want ErrNoRecipients", err)
	}
	if repository.accepted {
		t.Fatal("known-undeliverable event must not be accepted")
	}
}

func TestIngestRejectsDisabledInboxBeforeAccepting(t *testing.T) {
	repository := &ingestionTestRepository{}
	service, err := NewService(repository, ingestionTestPolicy{enabled: false}, ingestionTestResolver{users: []string{"01H00000000000000000000001"}}, ingestionTestIDs{}, ingestionTestClock{})
	if err != nil {
		t.Fatalf("new service: %v", err)
	}
	_, err = service.Ingest(context.Background(), validIngestInput())
	if err != ErrNoRecipients {
		t.Fatalf("ingest error=%v, want ErrNoRecipients", err)
	}
	if repository.accepted {
		t.Fatal("disabled inbox event must not be accepted")
	}
}

func TestGetIngestionReceiptScopesToApplicationAndEnvironment(t *testing.T) {
	repository := &ingestionTestRepository{}
	service, err := NewService(repository, ingestionTestPolicy{enabled: true}, ingestionTestResolver{users: []string{"01H00000000000000000000001"}}, ingestionTestIDs{}, ingestionTestClock{})
	if err != nil {
		t.Fatalf("new service: %v", err)
	}
	if _, err := service.GetIngestionReceipt(context.Background(), "tenant-a", "customer_and_opportunity", "prod", "01H00000000000000000000001"); err != nil {
		t.Fatalf("get receipt: %v", err)
	}
	want := []string{"tenant-a", "customer_and_opportunity", "prod", "01H00000000000000000000001"}
	if len(repository.receiptArgs) != len(want) {
		t.Fatalf("receipt args=%v, want=%v", repository.receiptArgs, want)
	}
	for i := range want {
		if repository.receiptArgs[i] != want[i] {
			t.Fatalf("receipt args=%v, want=%v", repository.receiptArgs, want)
		}
	}
}

func TestCreateTemplatePersistsPublishedFirstVersion(t *testing.T) {
	service, err := NewService(&ingestionTestRepository{}, ingestionTestPolicy{enabled: true}, ingestionTestResolver{}, ingestionTestIDs{}, ingestionTestClock{})
	if err != nil {
		t.Fatal(err)
	}
	template, version, err := service.CreateTemplate(context.Background(), CreateTemplateInput{TenantID: "tenant", OperatorID: "user", Code: "PROJECT_DELAYED", Name: "项目延期", Status: domain.TemplateStatusActive, TitleTemplate: "项目 {{project}} 延期", BodyTemplate: "项目 {{project}} 已延期", Variables: []domain.VariableDefinition{{Name: "project", Required: true, MaxLength: 64}}})
	if err != nil {
		t.Fatal(err)
	}
	if template.CurrentVersion != 1 || version.Status != domain.TemplateVersionPublished {
		t.Fatalf("template=%+v version=%+v", template, version)
	}
}

func validIngestInput() IngestInput {
	return IngestInput{TenantID: "01H00000000000000000000000", SourceApplication: "CRM", SourceEnvironment: "PROD", Event: domain.IngestionEvent{EventID: "EVENT_001", EventType: "OPPORTUNITY_APPROVED", NotificationScope: "CROSS_SYSTEM", Priority: "HIGH", Title: "审批通过", Content: "业务已审批通过", TargetURL: "/opportunities/1", IdempotencyKey: "event-001", Recipients: []string{"01H00000000000000000000001"}, OccurredAt: time.Date(2026, 8, 20, 0, 0, 0, 0, time.UTC)}}
}
