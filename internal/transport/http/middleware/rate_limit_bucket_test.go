package middleware

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
)

// 安全审计 M-9 回归：匿名 /credential-challenges 必须由进程内 IP 固定窗口约束。
// 本用例锁定限流器本身的计数口径——第 limit+1 次请求返回 429 PLATFORM_RATE_LIMITED，
// 且不同 IP 的桶互相独立；阈值与 /login 的 IP 兜底桶（120/min）以及 /oauth2/token
// 的出站桶（60/min）同量级，任何人调高这两个阈值都必须同时重估写入放大面。
func TestFixedWindowRateLimitRejectsAfterLimitPerIP(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name  string
		limit int
	}{
		{name: "challenge bucket 120/min", limit: 120},
		{name: "token bucket 60/min", limit: 60},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			router := gin.New()
			router.POST("/credential-challenges", FixedWindowRateLimit(test.limit, time.Minute), func(context *gin.Context) {
				context.Status(http.StatusNoContent)
			})

			const remoteAddr = "203.0.113.10:4000"
			for attempt := 1; attempt <= test.limit; attempt++ {
				if status := fixedWindowRateLimitAttempt(router, remoteAddr); status != http.StatusNoContent {
					t.Fatalf("attempt %d status = %d, want %d", attempt, status, http.StatusNoContent)
				}
			}
			recorder := fixedWindowRateLimitCall(router, remoteAddr)
			if recorder.Code != http.StatusTooManyRequests {
				t.Fatalf("attempt %d status = %d, want %d", test.limit+1, recorder.Code, http.StatusTooManyRequests)
			}
			if code := fixedWindowRateLimitErrorCode(t, recorder); code != "PLATFORM_RATE_LIMITED" {
				t.Fatalf("error code = %q, want PLATFORM_RATE_LIMITED", code)
			}
			// 异 IP 独立计桶：同一进程内的其它出口不受影响。
			if status := fixedWindowRateLimitAttempt(router, "198.51.100.20:4000"); status != http.StatusNoContent {
				t.Fatalf("other IP status = %d, want %d", status, http.StatusNoContent)
			}
		})
	}
}

func fixedWindowRateLimitAttempt(router *gin.Engine, remoteAddr string) int {
	return fixedWindowRateLimitCall(router, remoteAddr).Code
}

func fixedWindowRateLimitCall(router *gin.Engine, remoteAddr string) *httptest.ResponseRecorder {
	request := httptest.NewRequest(http.MethodPost, "/credential-challenges", nil)
	request.Header.Set("Content-Type", "application/json")
	request.RemoteAddr = remoteAddr
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, request)
	return recorder
}

func fixedWindowRateLimitErrorCode(t *testing.T, recorder *httptest.ResponseRecorder) string {
	t.Helper()
	var payload struct {
		Code string `json:"code"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode rate limit response: %v; body = %s", err, recorder.Body.String())
	}
	return payload.Code
}
