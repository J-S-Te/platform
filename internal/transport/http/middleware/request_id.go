// Package middleware contains transport-level HTTP middleware shared by all modules.
package middleware

import (
	"crypto/rand"
	"strings"
	"time"

	"github.com/J-S-Te/Basic-Platform/internal/shared/requestctx"
	"github.com/gin-gonic/gin"
)

const requestIDHeader = "X-Request-ID"

// RequestID 仅接收规范 ULID 形态的外部追踪号，否则重新生成，并把同一值贯穿日志、响应与审计。
// 这样既保留调用链关联，又避免任意长或带控制字符的请求头污染日志。
func RequestID() gin.HandlerFunc {
	return func(context *gin.Context) {
		requestID := strings.ToUpper(strings.TrimSpace(context.GetHeader(requestIDHeader)))
		if !isULID(requestID) {
			requestID = newULID(time.Now().UTC())
		}

		context.Header(requestIDHeader, requestID)
		context.Request = context.Request.WithContext(requestctx.WithRequestID(context.Request.Context(), requestID))
		context.Next()
	}
}

func isULID(value string) bool {
	if len(value) != 26 {
		return false
	}
	for _, character := range value {
		if !strings.ContainsRune("0123456789ABCDEFGHJKMNPQRSTVWXYZ", character) {
			return false
		}
	}
	return true
}

func newULID(now time.Time) string {
	const alphabet = "0123456789ABCDEFGHJKMNPQRSTVWXYZ"
	var entropy [10]byte
	if _, err := rand.Read(entropy[:]); err != nil {
		// 系统熵源失败时仍要给请求可关联的 ID；纳秒降级值不是安全随机数，只用于避免固定追踪号。
		nanoseconds := now.UnixNano()
		for index := range entropy {
			entropy[index] = byte(nanoseconds >> (index % 8 * 8))
		}
	}

	milliseconds := uint64(now.UnixMilli())
	encoded := make([]byte, 26)
	for index := 9; index >= 0; index-- {
		encoded[index] = alphabet[milliseconds&31]
		milliseconds >>= 5
	}

	var buffer uint32
	bits := 0
	outputIndex := 10
	for _, value := range entropy {
		buffer = (buffer << 8) | uint32(value)
		bits += 8
		for bits >= 5 && outputIndex < len(encoded) {
			bits -= 5
			encoded[outputIndex] = alphabet[(buffer>>bits)&31]
			outputIndex++
		}
	}
	if outputIndex < len(encoded) {
		encoded[outputIndex] = alphabet[(buffer<<(5-bits))&31]
	}
	return string(encoded)
}
