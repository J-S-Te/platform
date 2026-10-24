package http

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/J-S-Te/Basic-Platform/internal/platform/audit/application"
	"github.com/J-S-Te/Basic-Platform/internal/shared/appctx"
	"github.com/J-S-Te/Basic-Platform/internal/shared/requestctx"
)

func TestApplicationInputUsesValidUserLoginIP(t *testing.T) {
	request := requestWithDeliveryIP("172.20.0.9")
	input, err := applicationInput(eventInputPayload{
		ApplicationCode: "customer", EnvironmentCode: "prod", UserLoginIP: "2001:db8::1",
	}, request, testApplicationPrincipal())
	if err != nil {
		t.Fatalf("applicationInput() error = %v", err)
	}
	if input.SourceIP != "2001:db8::1" {
		t.Fatalf("SourceIP = %q, want user login IP", input.SourceIP)
	}
	// 旧字段只代表登录地址：按 LEGACY_PROTOCOL 记录，且不会被当成操作地址。
	if input.LoginIP != "2001:db8::1" || input.ClientIP != "" {
		t.Fatalf("LoginIP/ClientIP = %q/%q, want login only", input.LoginIP, input.ClientIP)
	}
	if input.IPSource != "LEGACY_PROTOCOL" {
		t.Fatalf("IPSource = %q, want LEGACY_PROTOCOL", input.IPSource)
	}
}

func TestApplicationInputUsesDeliveryIPWhenUserLoginIPMissing(t *testing.T) {
	request := requestWithDeliveryIP("172.20.0.9")
	input, err := applicationInput(eventInputPayload{
		ApplicationCode: "customer", EnvironmentCode: "prod",
	}, request, testApplicationPrincipal())
	if err != nil {
		t.Fatalf("applicationInput() error = %v", err)
	}
	// 兼容列保持历史语义（登录地址缺省时用投递地址），但新协议字段必须如实为空：
	// 投递地址只能描述"谁送来审计"，不能冒充用户的登录地址。
	if input.SourceIP != "172.20.0.9" {
		t.Fatalf("SourceIP = %q, want delivery IP (legacy column)", input.SourceIP)
	}
	if input.LoginIP != "" {
		t.Fatalf("LoginIP = %q, 未提供登录地址时必须为空，不能用投递地址冒充", input.LoginIP)
	}
	if input.IngestPeerIP != "172.20.0.9" {
		t.Fatalf("IngestPeerIP = %q, want delivery IP", input.IngestPeerIP)
	}
	if input.IPSource != "UNKNOWN" {
		t.Fatalf("IPSource = %q, want UNKNOWN", input.IPSource)
	}
	if delivery := batchDeliveryInput(request, testApplicationPrincipal()); delivery.SourceIP != "172.20.0.9" {
		t.Fatalf("batch receipt SourceIP = %q, want delivery IP", delivery.SourceIP)
	}
}

func TestApplicationInputRejectsInvalidUserLoginIP(t *testing.T) {
	_, err := applicationInput(eventInputPayload{
		ApplicationCode: "customer", EnvironmentCode: "prod", UserLoginIP: "not-an-ip",
	}, requestWithDeliveryIP("172.20.0.9"), testApplicationPrincipal())
	if !errors.Is(err, application.ErrValidation) {
		t.Fatalf("applicationInput() error = %v, want ErrValidation", err)
	}
}

func requestWithDeliveryIP(ip string) *http.Request {
	request := httptest.NewRequest(http.MethodPost, "/api/v1/audit/events/batch", nil)
	return request.WithContext(requestctx.WithClientIP(request.Context(), ip))
}

func testApplicationPrincipal() appctx.Principal {
	return appctx.Principal{ApplicationCode: "customer", EnvironmentCode: "prod", ClientID: "customer-prod-audit"}
}

// 新协议：登录地址与操作地址分开上报，投递地址由平台摄取端记录，三者互不替代。
func TestApplicationInputKeepsLoginAndClientIPSeparate(t *testing.T) {
	request := requestWithDeliveryIP("172.20.0.9")
	input, err := applicationInput(eventInputPayload{
		ApplicationCode: "customer", EnvironmentCode: "prod",
		LoginIP: "203.0.113.10", ClientIP: "10.8.0.5", IPSource: "TRUSTED_PROXY",
	}, request, testApplicationPrincipal())
	if err != nil {
		t.Fatalf("applicationInput() error = %v", err)
	}
	if input.LoginIP != "203.0.113.10" || input.ClientIP != "10.8.0.5" {
		t.Fatalf("LoginIP/ClientIP = %q/%q, want 203.0.113.10/10.8.0.5", input.LoginIP, input.ClientIP)
	}
	if input.IngestPeerIP != "172.20.0.9" {
		t.Fatalf("IngestPeerIP = %q, want delivery IP", input.IngestPeerIP)
	}
	if input.IPSource != "TRUSTED_PROXY" {
		t.Fatalf("IPSource = %q, want TRUSTED_PROXY", input.IPSource)
	}
	// 兼容列沿用登录地址，保持旧消费方读数不变。
	if input.SourceIP != "203.0.113.10" {
		t.Fatalf("SourceIP = %q, want login IP (legacy column)", input.SourceIP)
	}
}

func TestApplicationInputRejectsMalformedProtocolIP(t *testing.T) {
	_, err := applicationInput(eventInputPayload{
		ApplicationCode: "customer", EnvironmentCode: "prod", LoginIP: "203.0.113.10", ClientIP: "not-an-ip",
	}, requestWithDeliveryIP("172.20.0.9"), testApplicationPrincipal())
	if !errors.Is(err, application.ErrValidation) {
		t.Fatalf("applicationInput() error = %v, want ErrValidation", err)
	}
}

// 未知来源标记不能被自由文本污染。
func TestApplicationInputNormalizesUnknownIPSource(t *testing.T) {
	input, err := applicationInput(eventInputPayload{
		ApplicationCode: "customer", EnvironmentCode: "prod", ClientIP: "198.51.100.7", IPSource: "whatever",
	}, requestWithDeliveryIP("172.20.0.9"), testApplicationPrincipal())
	if err != nil {
		t.Fatalf("applicationInput() error = %v", err)
	}
	if input.IPSource != "UNKNOWN" {
		t.Fatalf("IPSource = %q, want UNKNOWN", input.IPSource)
	}
}
