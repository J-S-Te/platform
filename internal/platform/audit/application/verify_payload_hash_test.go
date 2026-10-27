package application

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/J-S-Te/Basic-Platform/internal/platform/audit/domain"
)

// verifyRepositoryStub 只关心 VerifyPayloadHash；其余接口方法为满足编译的空实现。
type verifyRepositoryStub struct {
	gotTenant, gotEventID string
	gotPayload            []byte
	result                domain.PayloadHashVerification
	err                   error
}

func (s *verifyRepositoryStub) VerifyPayloadHash(_ context.Context, tenantID, eventID string, payload []byte) (domain.PayloadHashVerification, error) {
	s.gotTenant, s.gotEventID, s.gotPayload = tenantID, eventID, payload
	return s.result, s.err
}
func (s *verifyRepositoryStub) Ingest(context.Context, string, EventInput, time.Time) (domain.Receipt, error) {
	return domain.Receipt{}, nil
}
func (s *verifyRepositoryStub) IngestBatch(context.Context, string, BatchDeliveryInput, []EventInput, time.Time) ([]domain.Receipt, error) {
	return nil, nil
}
func (s *verifyRepositoryStub) List(context.Context, string, PageRequest) (PageResult[domain.Event], error) {
	return PageResult[domain.Event]{}, nil
}
func (s *verifyRepositoryStub) Get(context.Context, string, string) (domain.Event, error) {
	return domain.Event{}, nil
}
func (s *verifyRepositoryStub) IngestionHealth(context.Context, string, time.Time) (IngestionHealth, error) {
	return IngestionHealth{}, nil
}
func (s *verifyRepositoryStub) CreateExportJob(context.Context, string, string, PageRequest, string, time.Time) (domain.ExportJob, error) {
	return domain.ExportJob{}, nil
}
func (s *verifyRepositoryStub) GetExportJob(context.Context, string, string) (domain.ExportJob, error) {
	return domain.ExportJob{}, nil
}
func (s *verifyRepositoryStub) ClaimExportJob(context.Context, string, time.Time, time.Time) (domain.ExportWork, bool, error) {
	return domain.ExportWork{}, false, nil
}
func (s *verifyRepositoryStub) ListExportEvents(context.Context, string, domain.ExportQuery) ([]domain.Event, error) {
	return nil, nil
}
func (s *verifyRepositoryStub) CompleteExportJob(context.Context, domain.ExportWork, domain.ExportFile, time.Time) error {
	return nil
}
func (s *verifyRepositoryStub) FailExportJob(context.Context, domain.ExportWork, string, string, time.Time, time.Time) error {
	return nil
}
func (s *verifyRepositoryStub) GetExportFile(context.Context, string, string) (domain.ExportFile, error) {
	return domain.ExportFile{}, nil
}
func (s *verifyRepositoryStub) VerifyEventChain(context.Context, string, string, string, uint64, int) (domain.ChainVerification, error) {
	return domain.ChainVerification{}, nil
}

type fixedIDs struct{}

func (fixedIDs) New(time.Time) (string, error) { return "id", nil }

type fixedClock struct{}

func (fixedClock) Now() time.Time { return time.Unix(0, 0).UTC() }

func newVerifyService(t *testing.T, repository Repository) *Service {
	t.Helper()
	service, err := NewService(repository, fixedIDs{}, fixedClock{})
	if err != nil {
		t.Fatalf("new service: %v", err)
	}
	return service
}

// 规范化必须与摄取一致：对 EventInput 做结构体 JSON 序列化后交给仓储，租户与 event_id 原样透传。
func TestVerifyPayloadHashCanonicalizesInputLikeIngest(t *testing.T) {
	stub := &verifyRepositoryStub{result: domain.PayloadHashVerification{EventID: "evt-1", Algorithm: "HMAC-SHA256", Matched: true}}
	service := newVerifyService(t, stub)
	input := EventInput{EventID: "evt-1", Action: "POST /api/v1/users", Result: "SUCCESS", OccurredAt: time.Date(2026, 10, 27, 1, 2, 3, 0, time.UTC)}
	result, err := service.VerifyPayloadHash(context.Background(), "tenant-1", " evt-1 ", input)
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	// 与 Ingest 同一条规范化路径：normalizeInput 补默认值（ActorType/RiskLevel/Classification/EventCategory/EventType）后再序列化。
	want, _ := json.Marshal(normalizeInput(input))
	if string(stub.gotPayload) != string(want) {
		t.Fatalf("payload passed to repository = %s, want normalized struct JSON %s", stub.gotPayload, want)
	}
	if normalized := normalizeInput(input); normalized.ActorType != "SYSTEM" || normalized.RiskLevel != "LOW" {
		t.Fatalf("normalizeInput defaults changed: %+v", normalized)
	}
	if stub.gotTenant != "tenant-1" || stub.gotEventID != "evt-1" {
		t.Fatalf("tenant/event passed = %q/%q", stub.gotTenant, stub.gotEventID)
	}
	if !result.Matched || result.Algorithm != "HMAC-SHA256" {
		t.Fatalf("result = %+v", result)
	}
}

func TestVerifyPayloadHashRejectsMissingOrMismatchedEventID(t *testing.T) {
	stub := &verifyRepositoryStub{}
	service := newVerifyService(t, stub)
	if _, err := service.VerifyPayloadHash(context.Background(), "tenant-1", "  ", EventInput{}); !errors.Is(err, ErrValidation) {
		t.Fatalf("blank event_id err = %v, want ErrValidation", err)
	}
	if _, err := service.VerifyPayloadHash(context.Background(), "tenant-1", "evt-1", EventInput{EventID: "evt-2"}); !errors.Is(err, ErrValidation) {
		t.Fatalf("mismatched EventID err = %v, want ErrValidation", err)
	}
	if stub.gotPayload != nil {
		t.Fatal("repository must not be called when validation fails")
	}
}

func TestVerifyPayloadHashPropagatesRepositoryErrors(t *testing.T) {
	stub := &verifyRepositoryStub{err: ErrNotFound}
	service := newVerifyService(t, stub)
	if _, err := service.VerifyPayloadHash(context.Background(), "tenant-1", "evt-404", EventInput{}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
}
