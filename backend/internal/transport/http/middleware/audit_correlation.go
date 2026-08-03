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

// AuditIngestionCorrelation 校验子系统审计摄取所需的关联头。只有在此处完成长度、字符集和
// traceparent 规范校验的值才进入可信 context，审计服务不会直接读取外部请求头。
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

// parseTraceparent 接受 W3C traceparent 基础格式并提取 trace-id。拒绝 ff 版本、全零 ID、
// 大写十六进制和附加字段，使持久化关联值只有一种规范表示，便于跨系统精确查询。
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
