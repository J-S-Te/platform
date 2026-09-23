package middleware

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
)

func loginRateLimitTestRouter(limit, ipLimit int) *gin.Engine {
	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.POST("/login", LoginRateLimit("account", limit, ipLimit, time.Minute), func(context *gin.Context) {
		// handler 在限流层之后仍必须能读到完整请求体（验证 body 还原）。
		body, err := io.ReadAll(context.Request.Body)
		if err != nil {
			context.Status(http.StatusInternalServerError)
			return
		}
		var payload map[string]string
		if err := json.Unmarshal(body, &payload); err != nil {
			context.Status(http.StatusBadRequest)
			return
		}
		context.String(http.StatusNoContent, "account=%s", payload["account"])
	})
	return router
}

func loginRateLimitPost(router *gin.Engine, account, remoteAddr string) int {
	body := fmt.Sprintf("{\"account\":%q,\"password\":\"secret\"}", account)
	request := httptest.NewRequest(http.MethodPost, "/login", strings.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	request.RemoteAddr = remoteAddr
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, request)
	return recorder.Code
}

// 安全审查 SEC-B7：同 IP 不同账号互不影响——账号 A 触顶只锁 A，账号 B 在同一出口仍可登录。
func TestLoginRateLimitSeparatesAccountsOnSameIP(t *testing.T) {
	router := loginRateLimitTestRouter(3, 1000)
	for attempt := 0; attempt < 3; attempt++ {
		if code := loginRateLimitPost(router, "victim@example.com", "203.0.113.10:4000"); code != http.StatusNoContent {
			t.Fatalf("attempt %d status = %d, want 204", attempt+1, code)
		}
	}
	if code := loginRateLimitPost(router, "victim@example.com", "203.0.113.10:4000"); code != http.StatusTooManyRequests {
		t.Fatalf("触顶账号状态 = %d, want 429", code)
	}
	if code := loginRateLimitPost(router, "other@example.com", "203.0.113.10:4000"); code != http.StatusNoContent {
		t.Fatalf("同 IP 其他账号状态 = %d, want 204（不受触顶账号影响）", code)
	}
}

// 安全审查 SEC-B7：同账号多 IP 受账号桶约束——换出口 IP 不能重置该账号的额度。
func TestLoginRateLimitConstrainsSameAccountAcrossIPs(t *testing.T) {
	router := loginRateLimitTestRouter(2, 1000)
	if code := loginRateLimitPost(router, "target@example.com", "203.0.113.10:4000"); code != http.StatusNoContent {
		t.Fatalf("首次状态 = %d, want 204", code)
	}
	if code := loginRateLimitPost(router, "target@example.com", "198.51.100.7:4000"); code != http.StatusNoContent {
		t.Fatalf("换 IP 第二次状态 = %d, want 204", code)
	}
	if code := loginRateLimitPost(router, "target@example.com", "192.0.2.55:4000"); code != http.StatusTooManyRequests {
		t.Fatalf("换 IP 第三次状态 = %d, want 429（账号桶约束）", code)
	}
}

// 安全审查 SEC-B7：IP 兜底桶对海量随机账号撞库有上界。
func TestLoginRateLimitIPFallbackBoundsAccountRotations(t *testing.T) {
	router := loginRateLimitTestRouter(5, 4)
	for attempt := 0; attempt < 4; attempt++ {
		account := fmt.Sprintf("rotating-%d@example.com", attempt)
		if code := loginRateLimitPost(router, account, "203.0.113.66:4000"); code != http.StatusNoContent {
			t.Fatalf("随机账号 %d 状态 = %d, want 204", attempt, code)
		}
	}
	if code := loginRateLimitPost(router, "one-more@example.com", "203.0.113.66:4000"); code != http.StatusTooManyRequests {
		t.Fatalf("IP 兜底状态 = %d, want 429", code)
	}
}

// 限流层缓冲请求体后必须原样还原，handler 仍能读到完整登录载荷。
func TestLoginRateLimitRestoresBodyForHandler(t *testing.T) {
	router := loginRateLimitTestRouter(5, 1000)
	request := httptest.NewRequest(http.MethodPost, "/login", strings.NewReader(fmt.Sprintf("{\"account\":%q}", "body@example.com")))
	request.Header.Set("Content-Type", "application/json")
	request.RemoteAddr = "203.0.113.10:4000"
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204（body 已还原给 handler）", recorder.Code)
	}
}
