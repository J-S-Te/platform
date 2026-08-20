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
}

func TestApplicationInputUsesDeliveryIPWhenUserLoginIPMissing(t *testing.T) {
	request := requestWithDeliveryIP("172.20.0.9")
	input, err := applicationInput(eventInputPayload{
		ApplicationCode: "customer", EnvironmentCode: "prod",
	}, request, testApplicationPrincipal())
	if err != nil {
		t.Fatalf("applicationInput() error = %v", err)
	}
	if input.SourceIP != "172.20.0.9" {
		t.Fatalf("SourceIP = %q, want delivery IP", input.SourceIP)
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
