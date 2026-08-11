package middleware

import (
	"net/http"
	"testing"

	"github.com/gin-gonic/gin"
)

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
