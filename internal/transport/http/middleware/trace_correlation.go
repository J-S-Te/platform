package middleware

import (
	"crypto/rand"
	"encoding/hex"
	"regexp"
	"strings"

	"github.com/J-S-Te/Basic-Platform/internal/shared/requestctx"
	"github.com/gin-gonic/gin"
)

const correlationIDHeader = "X-Correlation-ID"

var correlationIDPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:-]{0,127}$`)

// TraceCorrelation 建立可信的 trace_id 与 correlation_id，并通过响应头返回同一条链路。
// 外部 traceparent 只有满足 W3C 基础格式且 trace/span 均非零时才会被继承；非法值不会
// 进入日志。缺少业务关联号时回退到已生成的 request_id，确保每个请求均可检索。
func TraceCorrelation() gin.HandlerFunc {
	return func(context *gin.Context) {
		traceID, ok := traceIDFromTraceparent(context.GetHeader("traceparent"))
		if !ok {
			traceID = randomHex(16)
		}
		correlationID := strings.TrimSpace(context.GetHeader(correlationIDHeader))
		if !correlationIDPattern.MatchString(correlationID) {
			correlationID = requestctx.RequestID(context.Request.Context())
		}

		spanID := randomHex(8)
		context.Header("traceparent", "00-"+traceID+"-"+spanID+"-01")
		context.Header(correlationIDHeader, correlationID)
		requestContext := requestctx.WithTraceID(context.Request.Context(), traceID)
		requestContext = requestctx.WithCorrelationID(requestContext, correlationID)
		context.Request = context.Request.WithContext(requestContext)
		context.Next()
	}
}

func traceIDFromTraceparent(value string) (string, bool) {
	parts := strings.Split(strings.ToLower(strings.TrimSpace(value)), "-")
	if len(parts) != 4 || len(parts[0]) != 2 || parts[0] == "ff" || len(parts[1]) != 32 || len(parts[2]) != 16 || len(parts[3]) != 2 {
		return "", false
	}
	if !validNonZeroHex(parts[1]) || !validNonZeroHex(parts[2]) || !validHex(parts[0]) || !validHex(parts[3]) {
		return "", false
	}
	return parts[1], true
}

func validNonZeroHex(value string) bool {
	return validHex(value) && strings.Trim(value, "0") != ""
}

func validHex(value string) bool {
	_, err := hex.DecodeString(value)
	return err == nil
}

func randomHex(byteCount int) string {
	buffer := make([]byte, byteCount)
	if _, err := rand.Read(buffer); err != nil {
		// 熵源故障极少发生；全 1 降级值仍满足协议并避免生成无效的全零 ID。
		for index := range buffer {
			buffer[index] = 1
		}
	}
	return hex.EncodeToString(buffer)
}
