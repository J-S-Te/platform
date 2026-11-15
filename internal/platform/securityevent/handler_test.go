package securityevent

import (
	"net/http"
	"testing"
	"time"

	"github.com/J-S-Te/Basic-Platform/internal/shared/appctx"
)

func testPrincipal() appctx.Principal {
	return appctx.Principal{
		OAuthClientID: "client", ClientID: "client", TenantID: "tenant-1",
		ApplicationID: "app-1", ApplicationCode: "project_management",
		EnvironmentID: "env-1", EnvironmentCode: "test",
		Scopes: map[string]struct{}{"audit.ingest": {}},
	}
}

func TestEventFromRequestUsesTrustedApplicationIdentity(t *testing.T) {
	h := &Handler{}
	event, err := h.eventFromRequest(testPrincipal(), ingestRequest{
		EventID: "evt-1", OccurredAt: time.Now().UTC().Format(time.RFC3339Nano),
		Action: "AUTHENTICATION_DENIED", Method: "post", Path: "/api/v1/projects",
		ClientIP: "203.0.113.10", Result: "DENIED", RiskLevel: "HIGH",
	})
	if err != nil {
		t.Fatal(err)
	}
	if event.ApplicationCode != "project_management" || event.TenantID != "tenant-1" || event.Method != http.MethodPost {
		t.Fatalf("unexpected event: %+v", event)
	}
	if event.Result != "DENIED" || event.RiskLevel != "HIGH" {
		t.Fatalf("unexpected result: %+v", event)
	}
	if _, err := h.eventFromRequest(testPrincipal(), ingestRequest{Method: "POST", Path: "no-leading-slash"}); err == nil {
		t.Fatal("relative path must be rejected")
	}
}
