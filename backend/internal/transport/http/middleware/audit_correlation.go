package middleware

import (
	"net/http"
	"regexp"
	"strings"

	"github.com/J-S-Te/Basic-Platform/backend/internal/shared/httperror"
	"github.com/J-S-Te/Basic-Platform/backend/internal/shared/httpresponse"
	"github.com/J-S-Te/Basic-Platform/backend/internal/shared/requestctx"
	"github.com/gin-gonic/gin"
)

const (
	auditTraceparentHeader = "traceparent"
	auditCorrelationHeader = "X-Correlation-ID"
)

var auditCorrelationIdentifierPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:-]{0,127}$`)
var lowercaseHexPattern = regexp.MustCompile(`^[0-9a-f]+$`)

// AuditIngestionCorrelation validates the transport correlation headers required on subsystem
// audit ingestion routes. Only values validated here are stored in requestctx and may therefore be
// trusted by the audit application service.
func AuditIngestionCorrelation() gin.HandlerFunc {
	return func(c *gin.Context) {
		requestID := strings.TrimSpace(c.GetHeader(requestIDHeader))
		traceID, traceOK := parseTraceparent(strings.TrimSpace(c.GetHeader(auditTraceparentHeader)))
		correlationID := strings.TrimSpace(c.GetHeader(auditCorrelationHeader))
		if !validAuditCorrelationIdentifier(requestID) || !traceOK || !validAuditCorrelationIdentifier(correlationID) {
			httpresponse.WriteError(c.Writer, c.Request, http.StatusUnprocessableEntity, httperror.Validation)
			c.Abort()
			return
		}

		ctx := requestctx.WithRequestID(c.Request.Context(), requestID)
		ctx = requestctx.WithTraceID(ctx, traceID)
		ctx = requestctx.WithCorrelationID(ctx, correlationID)
		c.Request = c.Request.WithContext(ctx)
		c.Header(requestIDHeader, requestID)
		c.Next()
	}
}

func validAuditCorrelationIdentifier(value string) bool {
	return auditCorrelationIdentifierPattern.MatchString(value)
}

// parseTraceparent validates the W3C traceparent base format and returns its trace identifier.
// Version ff, all-zero trace/parent identifiers, uppercase hexadecimal and additional fields are
// rejected so the stored value has one canonical representation.
func parseTraceparent(value string) (string, bool) {
	parts := strings.Split(value, "-")
	if len(parts) != 4 || len(parts[0]) != 2 || len(parts[1]) != 32 || len(parts[2]) != 16 || len(parts[3]) != 2 {
		return "", false
	}
	for _, part := range parts {
		if !lowercaseHexPattern.MatchString(part) {
			return "", false
		}
	}
	if parts[0] == "ff" || parts[1] == strings.Repeat("0", 32) || parts[2] == strings.Repeat("0", 16) {
		return "", false
	}
	return parts[1], true
}
