package application

import (
	"context"
	"errors"
	"testing"
	"time"
)

// backchannelDecisionRecorder 是 Back-Channel Logout 决定的审计替身：只记录事件，
// 便于断言"opt-out / 跳过登记"确实留痕而不是静默。
type backchannelDecisionRecorder struct {
	events []BackchannelLogoutAuditEvent
	err    error
}

func (recorder *backchannelDecisionRecorder) RecordBackchannelLogoutDecision(_ context.Context, event BackchannelLogoutAuditEvent) error {
	recorder.events = append(recorder.events, event)
	return recorder.err
}

func newBackchannelOnboardingService(t *testing.T, repository *subsystemOnboardingRepositoryStub) *SubsystemOnboardingService {
	t.Helper()
	return newBackchannelOnboardingServiceWithPolicy(t, repository, RedirectURIValidationPolicy{})
}

func newBackchannelOnboardingServiceWithPolicy(t *testing.T, repository *subsystemOnboardingRepositoryStub, policy RedirectURIValidationPolicy) *SubsystemOnboardingService {
	t.Helper()
	service, err := NewSubsystemOnboardingService(repository, &sequentialManagementIDs{}, fixedSubsystemClock{now: time.Date(2026, time.October, 30, 0, 0, 0, 0, time.UTC)}, policy)
	if err != nil {
		t.Fatal(err)
	}
	return service
}

func backchannelOnboardingInput() SubsystemOnboardingInput {
	return SubsystemOnboardingInput{
		TenantID: "01K10A00000000000000000001", OperatorID: "01K10B00000000000000000001",
		ApplicationCode: "contract_management", ApplicationName: "合同管理系统", Environment: "prod",
		PublicBaseURL: "https://platform.example.com", UpstreamURL: "http://contract-api:8080",
		PathPrefix: "/contract", ClientType: "confidential",
	}
}

// 接入默认行为：按公开基址 + 接入路径自动登记 Back-Channel Logout 端点，写模型带着
// 该地址进入与浏览器客户端同一个事务（BIZ-XEVT2-07）。
func TestOnboardSubsystemRegistersBackchannelLogoutURIByDefault(t *testing.T) {
	repository := &subsystemOnboardingRepositoryStub{}
	service := newBackchannelOnboardingService(t, repository)
	sink := &backchannelDecisionRecorder{}
	service.SetBackchannelLogoutAuditSink(sink)

	result, err := service.OnboardSubsystem(context.Background(), backchannelOnboardingInput())
	if err != nil {
		t.Fatal(err)
	}
	want := "https://platform.example.com/contract/auth/backchannel-logout"
	if repository.write.BackchannelLogoutURI != want {
		t.Fatalf("write logout URI = %q, want %q", repository.write.BackchannelLogoutURI, want)
	}
	if result.BackchannelLogoutURI != want || result.BackchannelLogoutDecision != BackchannelLogoutDecisionRegistered {
		t.Fatalf("result logout URI=%q decision=%q", result.BackchannelLogoutURI, result.BackchannelLogoutDecision)
	}
	// 登记成功是正常路径，不应产生额外审计噪音。
	if len(sink.events) != 0 {
		t.Fatalf("unexpected audit events %#v", sink.events)
	}
}

// 显式路径按"相对公开基址"归一化后登记，便于 RP 使用非默认接收端点。
func TestOnboardSubsystemRegistersConfiguredBackchannelLogoutPath(t *testing.T) {
	repository := &subsystemOnboardingRepositoryStub{}
	service := newBackchannelOnboardingService(t, repository)
	input := backchannelOnboardingInput()
	path := "auth/oidc/backchannel-logout/"
	input.BackchannelLogoutPath = &path

	if _, err := service.OnboardSubsystem(context.Background(), input); err != nil {
		t.Fatal(err)
	}
	want := "https://platform.example.com/contract/auth/oidc/backchannel-logout"
	if repository.write.BackchannelLogoutURI != want {
		t.Fatalf("write logout URI = %q, want %q", repository.write.BackchannelLogoutURI, want)
	}
}

// 显式 opt-out 必须写审计（metadata.missing_logout_uri）；没有审计通道时失败关闭，
// 不允许在"没人知道该 RP 收不到登出"的情况下完成接入。
func TestOnboardSubsystemBackchannelLogoutOptOutIsAudited(t *testing.T) {
	repository := &subsystemOnboardingRepositoryStub{}
	service := newBackchannelOnboardingService(t, repository)
	sink := &backchannelDecisionRecorder{}
	service.SetBackchannelLogoutAuditSink(sink)
	optedOut := ""
	input := backchannelOnboardingInput()
	input.BackchannelLogoutPath = &optedOut

	result, err := service.OnboardSubsystem(context.Background(), input)
	if err != nil {
		t.Fatal(err)
	}
	if repository.write.BackchannelLogoutURI != "" {
		t.Fatalf("opt-out wrote logout URI %q", repository.write.BackchannelLogoutURI)
	}
	if result.BackchannelLogoutDecision != BackchannelLogoutDecisionOptOut {
		t.Fatalf("decision = %q", result.BackchannelLogoutDecision)
	}
	if len(sink.events) != 1 {
		t.Fatalf("audit events = %d, want 1", len(sink.events))
	}
	event := sink.events[0]
	if event.Decision != BackchannelLogoutDecisionOptOut || event.Reason != "opt_out" {
		t.Fatalf("audit event decision=%q reason=%q", event.Decision, event.Reason)
	}
	if event.Metadata["missing_logout_uri"] != true || event.Metadata["reason"] != "opt_out" {
		t.Fatalf("audit metadata = %#v", event.Metadata)
	}
	if event.ClientID == "" || event.OAuthClientID == "" || event.TenantID != input.TenantID || event.Environment != "prod" {
		t.Fatalf("audit event lacks correlation fields: %#v", event)
	}
}

func TestOnboardSubsystemBackchannelLogoutOptOutWithoutAuditFailsClosed(t *testing.T) {
	repository := &subsystemOnboardingRepositoryStub{}
	service := newBackchannelOnboardingService(t, repository)
	optedOut := ""
	input := backchannelOnboardingInput()
	input.BackchannelLogoutPath = &optedOut

	if _, err := service.OnboardSubsystem(context.Background(), input); !errors.Is(err, ErrBackchannelLogoutAuditUnavailable) {
		t.Fatalf("opt-out without audit sink err = %v", err)
	}
	if repository.createCalls != 0 {
		t.Fatalf("opt-out without audit sink wrote control plane %d times", repository.createCalls)
	}
}

// 端点未通过协议校验时（这里是进程策略禁止的明文 HTTP 非回环地址）不放宽校验，
// 改为"跳过登记并审计"：接入仍可完成，但缺口可见。
func TestOnboardSubsystemSkipsInvalidBackchannelLogoutEndpointAndAudits(t *testing.T) {
	repository := &subsystemOnboardingRepositoryStub{}
	// 进程策略允许明文 HTTP（本地部署），但 Back-Channel Logout 端点校验不放宽：
	// 非回环明文 HTTP 端点被拒绝，登记跳过并留痕。
	service := newBackchannelOnboardingServiceWithPolicy(t, repository, RedirectURIValidationPolicy{AllowInsecureHTTP: true})
	sink := &backchannelDecisionRecorder{}
	service.SetBackchannelLogoutAuditSink(sink)
	input := backchannelOnboardingInput()
	input.PublicBaseURL = "http://platform.example.test"
	input.UpstreamURL = "http://contract-api:8080"

	result, err := service.OnboardSubsystem(context.Background(), input)
	if err != nil {
		t.Fatal(err)
	}
	if repository.write.BackchannelLogoutURI != "" || result.BackchannelLogoutDecision != BackchannelLogoutDecisionSkipped {
		t.Fatalf("invalid endpoint registered: %#v", result)
	}
	if len(sink.events) != 1 || sink.events[0].Reason != "invalid_logout_endpoint" || sink.events[0].Metadata["missing_logout_uri"] != true {
		t.Fatalf("audit events = %#v", sink.events)
	}
}

// 审计写入失败必须让接入失败：决定无法追责时不得提交控制面变更。
func TestOnboardSubsystemBackchannelLogoutAuditFailureFailsOnboarding(t *testing.T) {
	repository := &subsystemOnboardingRepositoryStub{}
	service := newBackchannelOnboardingService(t, repository)
	service.SetBackchannelLogoutAuditSink(&backchannelDecisionRecorder{err: errors.New("audit unavailable")})
	optedOut := ""
	input := backchannelOnboardingInput()
	input.BackchannelLogoutPath = &optedOut

	if _, err := service.OnboardSubsystem(context.Background(), input); err == nil {
		t.Fatal("audit failure was swallowed")
	}
}
