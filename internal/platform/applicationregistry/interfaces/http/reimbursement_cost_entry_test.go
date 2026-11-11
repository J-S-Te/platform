package http

import (
	"context"
	"io"
	"log/slog"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/J-S-Te/Basic-Platform/internal/platform/applicationregistry/application"
)

// reimbursementCostEntryHandler 创建真实HTTP适配入口及合成登记依赖。
// 参数：clients为已登记公开身份，不包含任何明文凭据。
// 返回值：可直接调用HTTP相关身份端口的处理器。
func reimbursementCostEntryHandler(clients []application.OAuthClientView) *SubsystemOnboardingHandler {
	return &SubsystemOnboardingHandler{
		service:            &stubSubsystemOnboardingService{},
		serviceCredentials: &serviceCredentialManagerStub{clients: clients},
		logger:             slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
}

// reimbursementCostEntryClient 提供与结算受控登记协议一致的合成身份。
// 参数：无。
// 返回值：独立最小用途的公开客户端元数据。
func reimbursementCostEntryClient() application.OAuthClientView {
	return application.OAuthClientView{ID: "cost-registry", TenantID: "tenant-1",
		ApplicationID: "app-1", EnvironmentID: "env-1",
		ClientID: "settlement-prod-reimbursement-cost", ClientType: "service",
		TokenAuthMethod: "client_secret_basic", Status: "ACTIVE",
		GrantTypes: []string{"client_credentials"}, Scopes: []string{"reimbursement.cost_candidate.read"}}
}

// TestReimbursementCostHTTPEntryDeliversPublicSource 覆盖HTTP接入真实入口到应用解析器。
// 参数：t为测试上下文，依赖使用合成登记元数据。
// 返回值：无；报销入口被提前跳过或身份未交付时失败。
func TestReimbursementCostHTTPEntryDeliversPublicSource(t *testing.T) {
	client := reimbursementCostEntryClient()
	handler := reimbursementCostEntryHandler([]application.OAuthClientView{client})
	got, err := handler.ResolveSubsystemRelatedServiceClients(
		context.Background(), "tenant-1", "reimbursement", "prod")
	if err != nil || len(got) != 1 {
		t.Fatalf("报销HTTP入口未交付结算来源: count=%d err=%v", len(got), err)
	}
	source := got[0]
	if source.Client.ID != client.ID || source.Client.ClientID != client.ClientID ||
		source.ApplicationCode != "settlement" || source.Environment != "prod" ||
		source.Purpose != application.ServiceCredentialSettlementReimbursementCostRead {
		t.Fatalf("公开来源身份不匹配: %+v", source)
	}
}

// TestReimbursementCostHTTPEntryRejectsDrift 拒绝真实入口的扩权身份并返回报销专用错误。
// 参数：t为测试上下文，合成来源增加无关scope模拟登记漂移。
// 返回值：无；漂移被静默当成未安装或错误指向合同依赖时失败。
func TestReimbursementCostHTTPEntryRejectsDrift(t *testing.T) {
	client := reimbursementCostEntryClient()
	client.Scopes = append(client.Scopes, "audit.ingest")
	handler := reimbursementCostEntryHandler([]application.OAuthClientView{client})
	_, err := handler.ResolveSubsystemRelatedServiceClients(
		context.Background(), "tenant-1", "reimbursement", "prod")
	if err == nil {
		t.Fatal("报销HTTP入口忽略了来源身份漂移")
	}
	writer := httptest.NewRecorder()
	handler.writeError(writer, httptest.NewRequest("POST", "/api/v1/subsystem-retry", nil), err)
	if writer.Code != 409 || !strings.Contains(writer.Body.String(),
		"IAM_SUBSYSTEM_REIMBURSEMENT_COST_DEPENDENCY_REQUIRED") {
		t.Fatalf("报销专用依赖错误缺失: status=%d body=%s", writer.Code, writer.Body.String())
	}
}

// TestReimbursementCostHTTPEntryMissingSource 保留未安装关闭并拒绝不可用解析依赖。
// 参数：t为测试上下文，不连接外部服务。
// 返回值：无；缺来源或缺管理能力被错误处理时失败。
func TestReimbursementCostHTTPEntryMissingSource(t *testing.T) {
	handler := reimbursementCostEntryHandler(nil)
	got, err := handler.relatedServiceClients(context.Background(), "tenant-1", "reimbursement", "prod")
	if err != nil || len(got) != 0 {
		t.Fatalf("尚无结算来源应明确关闭: got=%+v err=%v", got, err)
	}
	handler.serviceCredentials = nil
	if _, err = handler.relatedServiceClients(context.Background(), "tenant-1", "reimbursement", "prod"); err == nil {
		t.Fatal("接入入口不能静默忽略缺失的凭据管理能力")
	}
	if got, err = handler.relatedServiceClients(context.Background(), "tenant-1", "data_analysis", "prod"); err != nil || len(got) != 0 {
		t.Fatalf("无关应用行为被改变: got=%+v err=%v", got, err)
	}
}
