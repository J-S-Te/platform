package middleware

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/J-S-Te/Basic-Platform/internal/platform/securityevent"
	"github.com/J-S-Te/Basic-Platform/internal/shared/authctx"
	"github.com/gin-gonic/gin"
)

type captureSecurityRecorder struct{ events []securityevent.Event }

func (c *captureSecurityRecorder) Record(_ context.Context, event securityevent.Event) error {
	c.events = append(c.events, event)
	return nil
}

func TestSecurityEventTrailRecordsUnauthenticatedRequest(t *testing.T) {
	gin.SetMode(gin.TestMode)
	recorder := &captureSecurityRecorder{}
	router := gin.New()
	router.Use(SecurityEventTrail(recorder, "platform", "test", nil))
	router.GET("/api/v1/private", func(c *gin.Context) { c.Status(http.StatusUnauthorized) })
	router.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/api/v1/private", nil))
	if len(recorder.events) != 1 {
		t.Fatalf("events = %d, want 1", len(recorder.events))
	}
	if recorder.events[0].Result != "DENIED" || recorder.events[0].ApplicationCode != "platform" {
		t.Fatalf("unexpected event: %+v", recorder.events[0])
	}
}

func TestSecurityEventTrailSkipsAuthenticatedForbidden(t *testing.T) {
	gin.SetMode(gin.TestMode)
	recorder := &captureSecurityRecorder{}
	router := gin.New()
	router.Use(func(c *gin.Context) {
		c.Request = c.Request.WithContext(authctx.WithPrincipal(c.Request.Context(), authctx.Principal{Tenant: authctx.ReferenceName{ID: "tenant-1"}}))
		c.Next()
	})
	router.Use(SecurityEventTrail(recorder, "platform", "test", nil))
	router.GET("/api/v1/private", func(c *gin.Context) { c.Status(http.StatusForbidden) })
	router.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/api/v1/private", nil))
	if len(recorder.events) != 0 {
		t.Fatalf("authenticated denial must stay in tenant audit, got %d events", len(recorder.events))
	}
}
