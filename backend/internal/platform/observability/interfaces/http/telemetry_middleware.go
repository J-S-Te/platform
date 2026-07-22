package http

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/J-S-Te/Basic-Platform/backend/internal/platform/observability/domain"
	"github.com/J-S-Te/Basic-Platform/backend/internal/shared/authctx"
	"github.com/J-S-Te/Basic-Platform/backend/internal/shared/requestctx"
	"github.com/gin-gonic/gin"
)

// TelemetryRecorder is the narrow capture contract used by the Gin middleware.
type TelemetryRecorder interface {
	RecordLog(context.Context, domain.LogRecord) error
	RecordSpan(context.Context, domain.TraceSpan) error
	RecordMetric(context.Context, domain.MetricPoint) error
}

// NewTelemetryMiddleware returns a Gin middleware that records a redacted HTTP span, compact log
// and duration/request/error metrics. It does not accept telemetry from anonymous callers and it
// never changes a response when diagnostics storage is unavailable.
func NewTelemetryMiddleware(recorder TelemetryRecorder, baseResource domain.ResourceLabels, logger *slog.Logger) (gin.HandlerFunc, error) {
	if recorder == nil || logger == nil {
		return nil, errors.New("telemetry middleware dependencies must not be nil")
	}
	if strings.TrimSpace(baseResource.ApplicationID) == "" {
		return nil, errors.New("telemetry middleware application_id is required")
	}

	return func(ginContext *gin.Context) {
		startedAt := time.Now().UTC()
		traceID, parentSpanID := inboundTrace(ginContext.GetHeader("traceparent"))
		if traceID == "" {
			traceID = randomHex(16)
		}
		spanID := randomHex(8)
		if traceID != "" && spanID != "" {
			ginContext.Header("traceparent", fmt.Sprintf("00-%s-%s-01", traceID, spanID))
			ginContext.Request = ginContext.Request.WithContext(requestctx.WithTraceID(ginContext.Request.Context(), traceID))
		}

		ginContext.Next()

		resource, ok := resolvedResource(ginContext.Request.Context(), baseResource)
		if !ok {
			return
		}
		endedAt := time.Now().UTC()
		duration := endedAt.Sub(startedAt)
		route := ginContext.FullPath()
		if route == "" {
			route = ginContext.Request.URL.Path
		}
		if route == "" {
			route = "/"
		}
		status := ginContext.Writer.Status()
		statusCode := "OK"
		severity := domain.SeverityInfo
		if status >= http.StatusInternalServerError {
			statusCode = "ERROR"
			severity = domain.SeverityError
		} else if status >= http.StatusBadRequest {
			severity = domain.SeverityWarn
		}

		requestID := requestctx.RequestID(ginContext.Request.Context())
		attributes := map[string]any{
			"http.method": ginContext.Request.Method,
			"http.route":  route,
			"http.status": status,
			"request_id":  requestID,
		}
		captureContext := ginContext.Request.Context()
		if err := recorder.RecordSpan(captureContext, domain.TraceSpan{
			TraceID:      traceID,
			SpanID:       spanID,
			ParentSpanID: parentSpanID,
			Name:         ginContext.Request.Method + " " + route,
			Kind:         "SERVER",
			StatusCode:   statusCode,
			StartedAt:    startedAt,
			EndedAt:      endedAt,
			Resource:     resource,
			Attributes:   attributes,
		}); err != nil {
			logger.Error("telemetry span capture failed", "error", err)
		}
		if err := recorder.RecordMetric(captureContext, domain.MetricPoint{
			Timestamp: endedAt,
			Name:      "http.server.duration",
			Unit:      "ms",
			TraceID:   traceID,
			Value:     float64(duration) / float64(time.Millisecond),
			Resource:  resource,
			Attributes: map[string]string{
				"http.method": ginContext.Request.Method,
				"http.route":  route,
				"http.status": fmt.Sprintf("%d", status),
			},
		}); err != nil {
			logger.Error("telemetry duration metric capture failed", "error", err)
		}
		if err := recorder.RecordMetric(captureContext, domain.MetricPoint{
			Timestamp: endedAt,
			Name:      "http.server.requests",
			Unit:      "count",
			TraceID:   traceID,
			Value:     1,
			Resource:  resource,
			Attributes: map[string]string{
				"http.method": ginContext.Request.Method,
				"http.route":  route,
				"http.status": fmt.Sprintf("%d", status),
			},
		}); err != nil {
			logger.Error("telemetry request metric capture failed", "error", err)
		}
		if status >= http.StatusInternalServerError {
			if err := recorder.RecordMetric(captureContext, domain.MetricPoint{
				Timestamp: endedAt,
				Name:      "http.server.errors",
				Unit:      "count",
				TraceID:   traceID,
				Value:     1,
				Resource:  resource,
				Attributes: map[string]string{
					"http.method": ginContext.Request.Method,
					"http.route":  route,
					"http.status": fmt.Sprintf("%d", status),
				},
			}); err != nil {
				logger.Error("telemetry error metric capture failed", "error", err)
			}
		}
		if err := recorder.RecordLog(captureContext, domain.LogRecord{
			Timestamp:   endedAt,
			Severity:    severity,
			Message:     "http request completed",
			TraceID:     traceID,
			SpanID:      spanID,
			RequestID:   requestID,
			Application: resource.ApplicationID,
			Module:      "http",
			Operation:   route,
			DurationMS:  float64(duration) / float64(time.Millisecond),
			Resource:    resource,
			Attributes:  attributes,
		}); err != nil {
			logger.Error("telemetry runtime log capture failed", "error", err)
		}
	}, nil
}

func resolvedResource(ctx context.Context, baseResource domain.ResourceLabels) (domain.ResourceLabels, bool) {
	principal, authenticated := authctx.PrincipalFromContext(ctx)
	if !authenticated || strings.TrimSpace(principal.Tenant.ID) == "" {
		return domain.ResourceLabels{}, false
	}
	baseResource.TenantID = principal.Tenant.ID
	return baseResource, true
}

func inboundTrace(raw string) (string, string) {
	parts := strings.Split(strings.TrimSpace(raw), "-")
	if len(parts) != 4 || len(parts[0]) != 2 || len(parts[1]) != 32 || len(parts[2]) != 16 || len(parts[3]) != 2 {
		return "", ""
	}
	if strings.EqualFold(parts[0], "ff") || allZero(parts[1]) || allZero(parts[2]) {
		return "", ""
	}
	for _, part := range parts {
		if _, err := hex.DecodeString(part); err != nil {
			return "", ""
		}
	}
	return strings.ToLower(parts[1]), strings.ToLower(parts[2])
}

func allZero(value string) bool {
	for _, character := range value {
		if character != '0' {
			return false
		}
	}
	return true
}

func randomHex(bytes int) string {
	value := make([]byte, bytes)
	if _, err := rand.Read(value); err != nil {
		return ""
	}
	return hex.EncodeToString(value)
}
