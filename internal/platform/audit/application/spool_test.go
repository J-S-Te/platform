package application

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/J-S-Te/Basic-Platform/internal/platform/audit/domain"
)

// spoolRepositoryStub 复用 verifyRepositoryStub 的审计仓储实现，再补上死信仓储能力，
// 用来验证 Spool 走的是与 DeadLetterService.RecordFailure 同一套死信语义。
type spoolRepositoryStub struct {
	*verifyRepositoryStub
	created []domain.DeadLetter
	err     error
}

func (stub *spoolRepositoryStub) CreateDeadLetter(_ context.Context, letter domain.DeadLetter) (domain.DeadLetter, error) {
	if stub.err != nil {
		return domain.DeadLetter{}, stub.err
	}
	stub.created = append(stub.created, letter)
	return letter, nil
}

func (stub *spoolRepositoryStub) GetDeadLetter(context.Context, string, string) (domain.DeadLetter, error) {
	return domain.DeadLetter{}, nil
}

func (stub *spoolRepositoryStub) ListDeadLetters(context.Context, string, DeadLetterPageRequest) (PageResult[domain.DeadLetter], error) {
	return PageResult[domain.DeadLetter]{}, nil
}

func (stub *spoolRepositoryStub) MarkDeadLetterReplayed(context.Context, string, string, time.Time) error {
	return nil
}

func (stub *spoolRepositoryStub) MarkDeadLetterReplayFailed(context.Context, string, string, string, string, time.Time) error {
	return nil
}

func (stub *spoolRepositoryStub) DeadLetterStatus(context.Context, string, string) (domain.DeadLetterStatus, error) {
	return domain.DeadLetterStatus{}, nil
}

// spoolableInput 是必开模式“结果事件”的最小合法输入（含 BIZ-RUNTIME-11 的 phase/correlates_to）。
func spoolableInput() EventInput {
	return EventInput{
		EventID:         "platform-result-1",
		ApplicationCode: "platform",
		EnvironmentCode: "dev",
		Action:          "POST /api/v1/dictionaries",
		EventCategory:   "PLATFORM",
		EventType:       "POST /api/v1/dictionaries",
		OccurredAt:      time.Date(2026, 11, 2, 10, 0, 0, 0, time.UTC),
		ActorType:       "USER",
		ActorID:         "user-1",
		Result:          "SUCCESS",
		RiskLevel:       "LOW",
		Classification:  "INTERNAL",
		ResourceType:    "PLATFORM_API",
		Summary:         "POST /api/v1/dictionaries completed with HTTP Created",
		Metadata:        map[string]any{"phase": "result", "correlates_to": "platform-intent-1", "status_code": 201},
	}
}

// BIZ-RUNTIME-11：Spool 必须把失败的结果事件按 PENDING 死信保存，保持原 event_id 与
// 租户边界，载荷可被既有重放路径解码。
func TestSpoolPersistsPendingDeadLetterWithOriginalEvent(t *testing.T) {
	repository := &spoolRepositoryStub{verifyRepositoryStub: &verifyRepositoryStub{}}
	service := newVerifyService(t, repository)
	input := spoolableInput()

	if err := service.Spool(context.Background(), " tenant-1 ", input); err != nil {
		t.Fatalf("spool: %v", err)
	}
	if len(repository.created) != 1 {
		t.Fatalf("dead letters created = %d, want 1", len(repository.created))
	}
	letter := repository.created[0]
	if letter.TenantID != "tenant-1" {
		t.Fatalf("dead letter tenant = %q, want normalized tenant-1", letter.TenantID)
	}
	if letter.Status != domain.DeadLetterPending {
		t.Fatalf("dead letter status = %q, want %q", letter.Status, domain.DeadLetterPending)
	}
	if letter.EventID != input.EventID || letter.ApplicationCode != "platform" || letter.EnvironmentCode != "dev" {
		t.Fatalf("dead letter identity = %+v, want the original event identity", letter)
	}
	if string(letter.LastErrorCode) != SpoolErrorCodeWriteExhausted {
		t.Fatalf("dead letter error code = %q, want %q", letter.LastErrorCode, SpoolErrorCodeWriteExhausted)
	}
	if !strings.Contains(string(letter.LastErrorMessage), "synchronous retry") {
		t.Fatalf("dead letter error message = %q, want retry-exhaustion summary", letter.LastErrorMessage)
	}
	replayed, err := unmarshalDeadLetterEvent(letter.Payload)
	if err != nil {
		t.Fatalf("decode dead letter payload: %v", err)
	}
	if replayed.EventID != input.EventID || replayed.Metadata["phase"] != "result" || replayed.Metadata["correlates_to"] != "platform-intent-1" {
		t.Fatalf("replayed event = %+v, want the original result event", replayed)
	}
}

// 死信也写不进去时必须失败关闭（返回错误），让中间件按“审计不可用”拒绝响应；
// 不能悄悄丢弃已经失败的结果事件。
func TestSpoolFailsClosedWhenDeadLetterStoreUnavailable(t *testing.T) {
	repository := &spoolRepositoryStub{verifyRepositoryStub: &verifyRepositoryStub{}, err: errors.New("dead letter store unavailable")}
	service := newVerifyService(t, repository)
	if err := service.Spool(context.Background(), "tenant-1", spoolableInput()); err == nil {
		t.Fatal("spool must return the dead-letter store error")
	}
}

// 仓储不支持死信能力时同样失败关闭，而不是静默成功。
func TestSpoolFailsClosedWhenRepositoryHasNoDeadLetterSupport(t *testing.T) {
	service := newVerifyService(t, &verifyRepositoryStub{})
	if err := service.Spool(context.Background(), "tenant-1", spoolableInput()); err == nil {
		t.Fatal("spool must fail when the repository does not implement DeadLetterRepository")
	}
}

// 意图事件的 Result=REQUESTED 必须通过摄取校验；其余非法取值仍被拒绝，
// 必开检查没有因此放宽。
func TestValidateInputAcceptsRequestedResultOnly(t *testing.T) {
	input := spoolableInput()
	input.EventID = "platform-intent-1"
	input.Result = "REQUESTED"
	if err := validateInput("tenant-1", input); err != nil {
		t.Fatalf("REQUESTED intent result must be accepted: %v", err)
	}
	input.Result = "ACCEPTED"
	if err := validateInput("tenant-1", input); !errors.Is(err, ErrValidation) {
		t.Fatalf("unknown result error = %v, want ErrValidation", err)
	}
}
