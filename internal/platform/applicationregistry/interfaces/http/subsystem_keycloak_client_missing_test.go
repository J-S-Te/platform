package http

import (
	"context"
	"io"
	"log/slog"
	"testing"

	"github.com/J-S-Te/Basic-Platform/internal/platform/applicationregistry/application"
)

// keycloakClientMissingService 同时实现 onboarding 服务与回调解析能力，
// 用于验证"缺失才补建"的幂等判定。
type keycloakClientMissingService struct {
	stubSubsystemOnboardingService
	redirectURI string
	resolveErr  error
	called      bool
}

func (service *keycloakClientMissingService) ResolveApplicationEnvironmentRedirectURI(context.Context, string, string, string) (string, error) {
	service.called = true
	return service.redirectURI, service.resolveErr
}

// keycloakClientMissingCredentials 只按 client_id 列表决定"是否存在"。
type keycloakClientMissingCredentials struct {
	clients []application.OAuthClientView
	err     error
}

func (stub *keycloakClientMissingCredentials) ListOAuthClients(context.Context, string) ([]application.OAuthClientView, error) {
	return stub.clients, stub.err
}

func (stub *keycloakClientMissingCredentials) CreateOAuthClient(context.Context, application.OAuthClientCreateInput) (application.OAuthClientCreateResult, error) {
	return application.OAuthClientCreateResult{}, nil
}

func (stub *keycloakClientMissingCredentials) CreateOAuthClientSecret(context.Context, application.OAuthClientSecretCreateInput) (application.OAuthClientSecretResult, error) {
	return application.OAuthClientSecretResult{}, nil
}

func (stub *keycloakClientMissingCredentials) ReplaceOAuthClientScopes(context.Context, application.OAuthClientScopesUpdateInput) (application.OAuthClientView, error) {
	return application.OAuthClientView{}, nil
}

func newKeycloakClientMissingHandler(service *keycloakClientMissingService, credentials *keycloakClientMissingCredentials) *SubsystemOnboardingHandler {
	handler := &SubsystemOnboardingHandler{
		service:            service,
		serviceCredentials: credentials,
		logger:             slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	return handler
}

func TestKeycloakBrowserClientMissingOnlyWhenAbsent(t *testing.T) {
	service := &keycloakClientMissingService{redirectURI: "http://localhost:8081/customer-portal/auth/callback"}

	// 缺失：没有同名 ACTIVE 客户端时必须判定为缺失，供调用方补建。
	absent := newKeycloakClientMissingHandler(service, &keycloakClientMissingCredentials{})
	if !absent.keycloakBrowserClientMissing(context.Background(), "tenant", "customer_portal", "dev") {
		t.Fatal("客户端不存在时应判定为缺失")
	}
	if service.called {
		t.Fatal("缺失判定不应触发回调推导")
	}

	// 已存在：同名且 ACTIVE 时必须判定为存在，调用方不得轮换或重建。
	present := newKeycloakClientMissingHandler(service, &keycloakClientMissingCredentials{clients: []application.OAuthClientView{
		{ClientID: "customer_portal-dev-web", Status: "ACTIVE"},
	}})
	if present.keycloakBrowserClientMissing(context.Background(), "tenant", "customer_portal", "dev") {
		t.Fatal("客户端已存在时不得判定为缺失")
	}

	// 非 ACTIVE 视为缺失（需要重新建立可用客户端）。
	disabled := newKeycloakClientMissingHandler(service, &keycloakClientMissingCredentials{clients: []application.OAuthClientView{
		{ClientID: "customer_portal-dev-web", Status: "DISABLED"},
	}})
	if !disabled.keycloakBrowserClientMissing(context.Background(), "tenant", "customer_portal", "dev") {
		t.Fatal("非 ACTIVE 客户端应按缺失处理")
	}

	// 查询失败按"不缺失"处理，避免把基础设施故障误判成新建凭据。
	failing := newKeycloakClientMissingHandler(service, &keycloakClientMissingCredentials{err: application.ErrSubsystemProvisioningUnavailable})
	if failing.keycloakBrowserClientMissing(context.Background(), "tenant", "customer_portal", "dev") {
		t.Fatal("查询失败时不得判定为缺失")
	}
}

// 缺少回调解析能力时保持原语义：既不补建也不轮换，避免轻量装配被误判为需要新建凭据。
func TestKeycloakBrowserClientMissingKeepsLegacyPathWithoutResolver(t *testing.T) {
	handler := &SubsystemOnboardingHandler{
		service:            &stubSubsystemOnboardingService{},
		serviceCredentials: &keycloakClientMissingCredentials{},
		logger:             slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	if handler.keycloakBrowserClientMissing(context.Background(), "tenant", "customer_portal", "dev") {
		t.Fatal("无回调解析能力时不得判定为缺失")
	}
}
