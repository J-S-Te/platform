package bootstrap

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	applicationregistryapplication "github.com/J-S-Te/Basic-Platform/internal/platform/applicationregistry/application"
	auditapplication "github.com/J-S-Te/Basic-Platform/internal/platform/audit/application"
	auditdomain "github.com/J-S-Te/Basic-Platform/internal/platform/audit/domain"
	"github.com/J-S-Te/Basic-Platform/internal/shared/config"
	"github.com/J-S-Te/Basic-Platform/internal/shared/ulid"
)

// recordingAuditRepository 是审计摄取端的替身：只记录被 ingestion 接收的事件，
// 验证适配器确实走审计模块既有入口，而不是直接写 audit_event。
type recordingAuditRepository struct {
	auditapplication.Repository
	tenants []string
	inputs  []auditapplication.EventInput
	err     error
}

func (repository *recordingAuditRepository) Ingest(_ context.Context, tenantID string, input auditapplication.EventInput, _ time.Time) (auditdomain.Receipt, error) {
	if repository.err != nil {
		return auditdomain.Receipt{}, repository.err
	}
	repository.tenants = append(repository.tenants, tenantID)
	repository.inputs = append(repository.inputs, input)
	return auditdomain.Receipt{EventID: input.EventID, Status: "ACCEPTED"}, nil
}

func newRecordingAuditService(t *testing.T) (*auditapplication.Service, *recordingAuditRepository) {
	t.Helper()
	repository := &recordingAuditRepository{}
	service, err := auditapplication.NewService(repository, ulid.Generator{}, auditapplication.SystemClock{})
	if err != nil {
		t.Fatal(err)
	}
	return service, repository
}

func backchannelOptOutEvent() applicationregistryapplication.BackchannelLogoutAuditEvent {
	return applicationregistryapplication.BackchannelLogoutAuditEvent{
		TenantID: "01KTENANT00000000000000001", OperatorID: "01KUSER000000000000000001",
		ApplicationCode: "contract_management", Environment: "prod",
		OAuthClientID: "01KOAUTH000000000000000001", ClientID: "contract_management-prod-web",
		Decision: applicationregistryapplication.BackchannelLogoutDecisionOptOut,
		Reason:   "opt_out",
		Metadata: map[string]any{"missing_logout_uri": true, "reason": "opt_out", "oauth_client_id": "01KOAUTH000000000000000001"},
	}
}

// 接线后 opt-out 必须真的进入审计 ingestion，并保留机器可检索的缺口事实。
func TestBackchannelLogoutAuditAdapterRecordsOptOutThroughIngestion(t *testing.T) {
	service, repository := newRecordingAuditService(t)
	adapter := &backchannelLogoutAuditAdapter{service: service, config: config.AuditConfig{ApplicationCode: "platform", EnvironmentCode: "prod"}, ids: ulid.Generator{}}

	if err := adapter.RecordBackchannelLogoutDecision(context.Background(), backchannelOptOutEvent()); err != nil {
		t.Fatal(err)
	}
	if len(repository.inputs) != 1 || repository.tenants[0] != "01KTENANT00000000000000001" {
		t.Fatalf("ingested events = %d tenants = %v", len(repository.inputs), repository.tenants)
	}
	input := repository.inputs[0]
	if input.EventID == "" || input.ApplicationCode != "platform" || input.EnvironmentCode != "prod" {
		t.Fatalf("audit input = %+v", input)
	}
	if input.Action != "UPDATE" || input.ResourceType != "OAUTH_BACKCHANNEL_LOGOUT" || input.ResourceID != "01KOAUTH000000000000000001" {
		t.Fatalf("audit resource = %+v", input)
	}
	if input.Result != "SUCCESS" || input.ReasonCode != "opt_out" || input.RiskLevel != "HIGH" {
		t.Fatalf("audit result = %+v", input)
	}
	if input.ActorType != "USER" || input.ActorID != "01KUSER000000000000000001" {
		t.Fatalf("audit actor = %+v", input)
	}
	if input.Metadata["missing_logout_uri"] != true || input.Metadata["reason"] != "opt_out" {
		t.Fatalf("audit metadata = %#v", input.Metadata)
	}
	if input.Metadata["decision"] != applicationregistryapplication.BackchannelLogoutDecisionOptOut {
		t.Fatalf("audit decision = %#v", input.Metadata["decision"])
	}
	if input.Metadata["oauth_client_id"] != "01KOAUTH000000000000000001" || input.Metadata["client_id"] != "contract_management-prod-web" {
		t.Fatalf("audit correlation metadata = %#v", input.Metadata)
	}
	if !strings.Contains(input.Summary, "无法接收登出通知") {
		t.Fatalf("audit summary = %q", input.Summary)
	}
}

// 端点校验失败被跳过的决定同样要留痕，并带稳定原因码。
func TestBackchannelLogoutAuditAdapterRecordsSkippedEndpoint(t *testing.T) {
	service, repository := newRecordingAuditService(t)
	adapter := &backchannelLogoutAuditAdapter{service: service, config: config.AuditConfig{ApplicationCode: "platform", EnvironmentCode: "prod"}, ids: ulid.Generator{}}
	event := backchannelOptOutEvent()
	event.Decision = applicationregistryapplication.BackchannelLogoutDecisionSkipped
	event.Reason = "invalid_logout_endpoint"
	event.Metadata = map[string]any{"missing_logout_uri": true, "reason": "invalid_logout_endpoint", "oauth_client_id": event.OAuthClientID}

	if err := adapter.RecordBackchannelLogoutDecision(context.Background(), event); err != nil {
		t.Fatal(err)
	}
	if len(repository.inputs) != 1 || repository.inputs[0].ReasonCode != "invalid_logout_endpoint" {
		t.Fatalf("skipped endpoint audit = %#v", repository.inputs)
	}
}

// 即使调用方漏填元数据，适配器也必须补齐 missing_logout_uri：告警规则不能因为字段
// 缺失而漏聚合（这是 BIZ-XEVT2-07 的核心可观测契约）。
func TestBackchannelLogoutAuditAdapterAlwaysMarksMissingLogoutURI(t *testing.T) {
	service, repository := newRecordingAuditService(t)
	adapter := &backchannelLogoutAuditAdapter{service: service, config: config.AuditConfig{ApplicationCode: "platform", EnvironmentCode: "prod"}, ids: ulid.Generator{}}
	event := backchannelOptOutEvent()
	event.Metadata = nil

	if err := adapter.RecordBackchannelLogoutDecision(context.Background(), event); err != nil {
		t.Fatal(err)
	}
	if metadata := repository.inputs[0].Metadata; metadata["missing_logout_uri"] != true || metadata["oauth_client_id"] != event.OAuthClientID {
		t.Fatalf("audit metadata = %#v", metadata)
	}
}

// 未接线或摄取失败必须返回错误：接入侧据此失败关闭，绝不做出无法追责的决定。
func TestBackchannelLogoutAuditAdapterFailsClosed(t *testing.T) {
	var unconfigured *backchannelLogoutAuditAdapter
	if err := unconfigured.RecordBackchannelLogoutDecision(context.Background(), backchannelOptOutEvent()); err == nil {
		t.Fatal("adapter without audit service accepted an event")
	}
	// 显式 nil 服务（组合根缺审计依赖）同样失败关闭。
	unwired := &backchannelLogoutAuditAdapter{config: config.AuditConfig{}}
	if err := unwired.RecordBackchannelLogoutDecision(context.Background(), backchannelOptOutEvent()); err == nil {
		t.Fatal("adapter without ingestion service accepted an event")
	}
	service, repository := newRecordingAuditService(t)
	repository.err = errors.New("audit ingestion unavailable")
	adapter := &backchannelLogoutAuditAdapter{service: service, config: config.AuditConfig{ApplicationCode: "platform", EnvironmentCode: "prod"}, ids: ulid.Generator{}}
	if err := adapter.RecordBackchannelLogoutDecision(context.Background(), backchannelOptOutEvent()); err == nil {
		t.Fatal("ingestion failure was swallowed")
	}
}
