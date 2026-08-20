package middleware

import (
	"context"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/J-S-Te/Basic-Platform/internal/platform/audit/application"
	"github.com/J-S-Te/Basic-Platform/internal/platform/audit/domain"
	"github.com/J-S-Te/Basic-Platform/internal/shared/authctx"
	"github.com/J-S-Te/Basic-Platform/internal/shared/requestctx"
	"github.com/gin-gonic/gin"
)

type auditRecorderStub struct{ input application.EventInput }

func (stub *auditRecorderStub) Ingest(_ context.Context, _ string, input application.EventInput) (domain.Receipt, error) {
	stub.input = input
	return domain.Receipt{}, nil
}

func TestAuditTrailUsesSessionLoginIPAndRetainsRequestIP(t *testing.T) {
	gin.SetMode(gin.TestMode)
	recorder := &auditRecorderStub{}
	router := gin.New()
	router.Use(AuditTrail(recorder, slog.New(slog.NewTextHandler(io.Discard, nil))))
	router.POST("/write", func(context *gin.Context) { context.Status(http.StatusNoContent) })

	request := httptest.NewRequest(http.MethodPost, "/write", nil)
	request = request.WithContext(authctx.WithPrincipal(request.Context(), authctx.Principal{
		Tenant:  authctx.ReferenceName{ID: "tenant-1"},
		User:    authctx.ReferenceName{ID: "user-1"},
		LoginIP: net.ParseIP("203.0.113.10"),
	}))
	request = request.WithContext(requestctx.WithClientIP(request.Context(), "198.51.100.20"))
	router.ServeHTTP(httptest.NewRecorder(), request)

	if got := recorder.input.SourceIP; got != "203.0.113.10" {
		t.Fatalf("SourceIP = %q, want session login IP", got)
	}
	if got := recorder.input.Metadata["request_source_ip"]; got != "198.51.100.20" {
		t.Fatalf("request_source_ip = %v, want current request IP", got)
	}
}

func TestAuditTrailFallsBackToCurrentRequestIPWithoutSessionLoginIP(t *testing.T) {
	gin.SetMode(gin.TestMode)
	recorder := &auditRecorderStub{}
	router := gin.New()
	router.Use(AuditTrail(recorder, slog.New(slog.NewTextHandler(io.Discard, nil))))
	router.POST("/write", func(context *gin.Context) { context.Status(http.StatusNoContent) })

	request := httptest.NewRequest(http.MethodPost, "/write", nil)
	request = request.WithContext(authctx.WithPrincipal(request.Context(), authctx.Principal{
		Tenant: authctx.ReferenceName{ID: "tenant-1"},
		User:   authctx.ReferenceName{ID: "user-1"},
	}))
	request = request.WithContext(requestctx.WithClientIP(request.Context(), "198.51.100.20"))
	router.ServeHTTP(httptest.NewRecorder(), request)

	if got := recorder.input.SourceIP; got != "198.51.100.20" {
		t.Fatalf("SourceIP = %q, want current request IP", got)
	}
	if _, found := recorder.input.Metadata["request_source_ip"]; found {
		t.Fatal("request_source_ip should be omitted when it matches SourceIP")
	}
}

func TestAuditRiskLevelUsesBusinessSensitivity(t *testing.T) {
	tests := []struct {
		name, method, route string
		status, want        int
	}{
		{name: "role write", method: http.MethodPut, route: "/api/v1/roles/:role_id", status: http.StatusOK, want: 3},
		{name: "audit read", method: http.MethodGet, route: "/api/v1/audit/events", status: http.StatusOK, want: 2},
		{name: "forbidden read", method: http.MethodGet, route: "/api/v1/users", status: http.StatusForbidden, want: 2},
		{name: "ordinary create", method: http.MethodPost, route: "/api/v1/dictionaries", status: http.StatusCreated, want: 1},
	}
	levels := map[string]int{"LOW": 1, "MEDIUM": 2, "HIGH": 3, "CRITICAL": 4}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := levels[auditRiskLevel(test.method, test.route, test.status)]; got != test.want {
				t.Fatalf("risk level rank = %d, want %d", got, test.want)
			}
		})
	}
}

func TestShouldRecordAuditTrailSuppressesOnlyRicherBusinessEvents(t *testing.T) {
	userRoute := "/api/v1/users/:user_id/applications/:application_code/access"
	subjectRoute := "/api/v1/authorization-subjects/:subject_type/:subject_id/applications/:application_code/access"
	if shouldRecordAuditTrail(http.MethodPut, userRoute) || shouldRecordAuditTrail(http.MethodDelete, userRoute) {
		t.Fatal("user application access writes should be recorded only by the business audit event")
	}
	if shouldRecordAuditTrail(http.MethodDelete, subjectRoute) {
		t.Fatal("subject access deletion should be recorded only by the business audit event")
	}
	if !shouldRecordAuditTrail(http.MethodPut, subjectRoute) {
		t.Fatal("rejected subject access update must remain in the generic audit trail")
	}
	if shouldRecordAuditTrail(http.MethodPut, "/api/v1/applications/:application_id/authorization-catalog") {
		t.Fatal("catalog synchronization should be recorded only by its business audit event")
	}
}

func TestAuditResourceIDUsesInnermostResource(t *testing.T) {
	context := &gin.Context{Params: gin.Params{
		{Key: "user_id", Value: "user-1"},
		{Key: "application_id", Value: "application-1"},
	}}
	if got := auditResourceID(context); got != "application-1" {
		t.Fatalf("resource ID = %q, want application-1", got)
	}
}
