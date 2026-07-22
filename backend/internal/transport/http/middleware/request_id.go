// Package middleware contains transport-level HTTP middleware shared by all modules.
package middleware

import (
	"crypto/rand"
	"strings"
	"time"

	"github.com/J-S-Te/Basic-Platform/backend/internal/shared/requestctx"
	"github.com/gin-gonic/gin"
)

const requestIDHeader = "X-Request-ID"

// RequestID validates or generates a ULID-shaped request identifier and stores it in the request
// context so HTTP adapters, logs, and response envelopes use the same correlation value.
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
		// crypto/rand failure is exceptional. The timestamp and nanoseconds still prevent a
		// predictable constant ID while allowing the current request to receive a correlation ID.
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
