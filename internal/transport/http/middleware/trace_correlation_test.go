package middleware

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/J-S-Te/Basic-Platform/internal/shared/requestctx"
	"github.com/gin-gonic/gin"
)

func TestTraceCorrelationPropagatesValidatedIdentifiers(t *testing.T) {
	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.Use(RequestID(), TraceCorrelation())
	router.GET("/test", func(context *gin.Context) {
		if requestctx.TraceID(context.Request.Context()) != "0123456789abcdef0123456789abcdef" {
			t.Fatalf("trace_id=%q", requestctx.TraceID(context.Request.Context()))
		}
		if requestctx.CorrelationID(context.Request.Context()) != "business-42" {
			t.Fatalf("correlation_id=%q", requestctx.CorrelationID(context.Request.Context()))
		}
		context.Status(http.StatusNoContent)
	})
	request := httptest.NewRequest(http.MethodGet, "/test", nil)
	request.Header.Set("traceparent", "00-0123456789abcdef0123456789abcdef-0123456789abcdef-01")
	request.Header.Set("X-Correlation-ID", "business-42")
	response := httptest.NewRecorder()

	router.ServeHTTP(response, request)

	if response.Code != http.StatusNoContent || response.Header().Get("X-Correlation-ID") != "business-42" {
		t.Fatalf("status=%d headers=%v", response.Code, response.Header())
	}
}

func TestTraceCorrelationRejectsUntrustedHeaders(t *testing.T) {
	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.Use(RequestID(), TraceCorrelation())
	router.GET("/test", func(context *gin.Context) {
		if len(requestctx.TraceID(context.Request.Context())) != 32 {
			t.Fatalf("generated trace_id=%q", requestctx.TraceID(context.Request.Context()))
		}
		if requestctx.CorrelationID(context.Request.Context()) != requestctx.RequestID(context.Request.Context()) {
			t.Fatalf("invalid correlation header was trusted")
		}
		context.Status(http.StatusNoContent)
	})
	request := httptest.NewRequest(http.MethodGet, "/test", nil)
	request.Header.Set("traceparent", "00-00000000000000000000000000000000-0000000000000000-01")
	request.Header.Set("X-Correlation-ID", "bad\nvalue")
	response := httptest.NewRecorder()
	router.ServeHTTP(response, request)
}
