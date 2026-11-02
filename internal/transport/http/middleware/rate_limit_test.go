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

const (
	loginRateLimitSuccessPassword = "secret"
	loginRateLimitFailurePassword = "wrong-password"
	loginRateLimitSameIP          = "203.0.113.10:4000"
)

func loginRateLimitTestRouter(limit, ipLimit int) *gin.Engine {
	return loginRateLimitTestRouterWithWindow(limit, ipLimit, time.Minute)
}

func loginRateLimitTestRouterWithWindow(limit, ipLimit int, window time.Duration) *gin.Engine {
	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.POST("/login", LoginRateLimit("account", limit, ipLimit, window), func(context *gin.Context) {
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
		// 用口令字段模拟认证结果：成功 204、认证失败 401，另有非 401 失败态用于断言计数口径。
		switch payload["password"] {
		case loginRateLimitSuccessPassword:
			context.String(http.StatusNoContent, "account=%s", payload["account"])
		case "forbidden":
			context.Status(http.StatusForbidden)
		case "boom":
			context.Status(http.StatusInternalServerError)
		default:
			context.Status(http.StatusUnauthorized)
		}
	})
	return router
}

func loginRateLimitAttempt(router *gin.Engine, account, password, remoteAddr string) int {
	body := fmt.Sprintf("{\"account\":%q,\"password\":%q}", account, password)
	request := httptest.NewRequest(http.MethodPost, "/login", strings.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	request.RemoteAddr = remoteAddr
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, request)
	return recorder.Code
}

func loginRateLimitPost(router *gin.Engine, account, remoteAddr string) int {
	return loginRateLimitAttempt(router, account, loginRateLimitSuccessPassword, remoteAddr)
}

func loginRateLimitFail(router *gin.Engine, account, remoteAddr string) int {
	return loginRateLimitAttempt(router, account, loginRateLimitFailurePassword, remoteAddr)
}

// 安全审查 SEC-B7 / M-2：账号桶只统计失败尝试——同 IP 不同账号互不影响，
// 触顶账号只锁自己。
func TestLoginRateLimitSeparatesAccountsOnSameIP(t *testing.T) {
	router := loginRateLimitTestRouter(3, 1000)
	for attempt := 0; attempt < 3; attempt++ {
		if code := loginRateLimitFail(router, "victim@example.com", loginRateLimitSameIP); code != http.StatusUnauthorized {
			t.Fatalf("失败尝试 %d 状态 = %d, want 401", attempt+1, code)
		}
	}
	if code := loginRateLimitFail(router, "victim@example.com", loginRateLimitSameIP); code != http.StatusTooManyRequests {
		t.Fatalf("失败触顶账号状态 = %d, want 429", code)
	}
	if code := loginRateLimitPost(router, "other@example.com", loginRateLimitSameIP); code != http.StatusNoContent {
		t.Fatalf("同 IP 其他账号状态 = %d, want 204（不受触顶账号影响）", code)
	}
}

// 安全审查 SEC-B7 / M-2：同账号多 IP 受账号失败桶约束——换出口 IP 不能重置该账号额度。
func TestLoginRateLimitConstrainsSameAccountAcrossIPs(t *testing.T) {
	router := loginRateLimitTestRouter(2, 1000)
	if code := loginRateLimitFail(router, "target@example.com", "203.0.113.10:4000"); code != http.StatusUnauthorized {
		t.Fatalf("首次失败状态 = %d, want 401", code)
	}
	if code := loginRateLimitFail(router, "target@example.com", "198.51.100.7:4000"); code != http.StatusUnauthorized {
		t.Fatalf("换 IP 第二次失败状态 = %d, want 401", code)
	}
	if code := loginRateLimitFail(router, "target@example.com", "192.0.2.55:4000"); code != http.StatusTooManyRequests {
		t.Fatalf("换 IP 第三次状态 = %d, want 429（账号失败桶约束）", code)
	}
}

// 安全审查 SEC-B7：IP 兜底桶对海量随机账号撞库有上界（即使每次认证都成功）。
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

// M-2：连续成功登录不消耗账号额度，攻击者无法用正常登录把任意账号锁死。
func TestLoginRateLimitSuccessLoginsDoNotConsumeAccountQuota(t *testing.T) {
	router := loginRateLimitTestRouter(2, 1000)
	for attempt := 0; attempt < 10; attempt++ {
		if code := loginRateLimitPost(router, "admin@example.com", loginRateLimitSameIP); code != http.StatusNoContent {
			t.Fatalf("成功登录 %d 状态 = %d, want 204（成功不消耗账号额度）", attempt+1, code)
		}
	}
}

// M-2：成功后清零该账号历史失败计数，避免故障期积累的失败拖累正常登录。
// 触顶（失败计数 >= accountLimit）后预检查直接 429 是防撞库预期行为，因此清零只会发生在
// 尚未触顶的窗口内成功登录上。
func TestLoginRateLimitSuccessResetsAccountFailureCount(t *testing.T) {
	router := loginRateLimitTestRouter(3, 1000)
	for attempt := 0; attempt < 2; attempt++ {
		if code := loginRateLimitFail(router, "user@example.com", loginRateLimitSameIP); code != http.StatusUnauthorized {
			t.Fatalf("第 %d 次失败状态 = %d, want 401", attempt+1, code)
		}
	}
	if code := loginRateLimitPost(router, "user@example.com", loginRateLimitSameIP); code != http.StatusNoContent {
		t.Fatalf("成功登录状态 = %d, want 204", code)
	}
	// 若成功不清零，这里累计失败会达到 3 并提前 429；清零后仍应有完整额度。
	for attempt := 0; attempt < 2; attempt++ {
		if code := loginRateLimitFail(router, "user@example.com", loginRateLimitSameIP); code != http.StatusUnauthorized {
			t.Fatalf("清零后第 %d 次失败状态 = %d, want 401", attempt+1, code)
		}
	}
	if code := loginRateLimitPost(router, "user@example.com", loginRateLimitSameIP); code != http.StatusNoContent {
		t.Fatalf("再次成功登录状态 = %d, want 204", code)
	}
	if code := loginRateLimitFail(router, "user@example.com", loginRateLimitSameIP); code != http.StatusUnauthorized {
		t.Fatalf("清零后失败状态 = %d, want 401", code)
	}
}

// M-2：账号失败桶按账号隔离——同 IP 下其他账号的失败额度不受影响。
func TestLoginRateLimitFailureBucketIsolatedPerAccount(t *testing.T) {
	router := loginRateLimitTestRouter(2, 1000)
	for attempt := 0; attempt < 2; attempt++ {
		if code := loginRateLimitFail(router, "victim@example.com", loginRateLimitSameIP); code != http.StatusUnauthorized {
			t.Fatalf("victim 第 %d 次失败状态 = %d, want 401", attempt+1, code)
		}
	}
	if code := loginRateLimitFail(router, "other@example.com", loginRateLimitSameIP); code != http.StatusUnauthorized {
		t.Fatalf("其他账号首次失败状态 = %d, want 401（不共享 victim 的失败额度）", code)
	}
	if code := loginRateLimitFail(router, "victim@example.com", loginRateLimitSameIP); code != http.StatusTooManyRequests {
		t.Fatalf("victim 触顶状态 = %d, want 429", code)
	}
	if code := loginRateLimitFail(router, "other@example.com", loginRateLimitSameIP); code != http.StatusUnauthorized {
		t.Fatalf("其他账号第二次失败状态 = %d, want 401（仍在自己额度内）", code)
	}
}

// M-2：只有 401 消耗账号失败额度，其他失败状态（403/500）不计数。
func TestLoginRateLimitNonUnauthorizedFailuresDoNotConsumeQuota(t *testing.T) {
	router := loginRateLimitTestRouter(1, 1000)
	for _, password := range []string{"boom", "forbidden", "boom"} {
		if code := loginRateLimitAttempt(router, "user@example.com", password, loginRateLimitSameIP); code != http.StatusInternalServerError && code != http.StatusForbidden {
			t.Fatalf("非 401 失败状态 = %d, want 403/500", code)
		}
	}
	if code := loginRateLimitFail(router, "user@example.com", loginRateLimitSameIP); code != http.StatusUnauthorized {
		t.Fatalf("首个 401 状态 = %d, want 401（非 401 失败未消耗额度）", code)
	}
	if code := loginRateLimitFail(router, "user@example.com", loginRateLimitSameIP); code != http.StatusTooManyRequests {
		t.Fatalf("第二个 401 状态 = %d, want 429", code)
	}
}

// M-2：账号失败桶仍按固定窗口计时，窗口过期后失败计数清零。
func TestLoginRateLimitWindowExpiryResetsAccountFailures(t *testing.T) {
	router := loginRateLimitTestRouterWithWindow(1, 1000, 100*time.Millisecond)
	if code := loginRateLimitFail(router, "user@example.com", loginRateLimitSameIP); code != http.StatusUnauthorized {
		t.Fatalf("窗口内首次失败状态 = %d, want 401", code)
	}
	if code := loginRateLimitFail(router, "user@example.com", loginRateLimitSameIP); code != http.StatusTooManyRequests {
		t.Fatalf("窗口内触顶状态 = %d, want 429", code)
	}
	time.Sleep(250 * time.Millisecond)
	if code := loginRateLimitFail(router, "user@example.com", loginRateLimitSameIP); code != http.StatusUnauthorized {
		t.Fatalf("新窗口首次失败状态 = %d, want 401（计数已过期清零）", code)
	}
}

// 限流层缓冲请求体后必须原样还原，handler 仍能读到完整登录载荷。
func TestLoginRateLimitRestoresBodyForHandler(t *testing.T) {
	router := loginRateLimitTestRouter(5, 1000)
	request := httptest.NewRequest(http.MethodPost, "/login", strings.NewReader(fmt.Sprintf("{\"account\":%q,\"password\":%q}", "body@example.com", loginRateLimitSuccessPassword)))
	request.Header.Set("Content-Type", "application/json")
	request.RemoteAddr = loginRateLimitSameIP
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204（body 已还原给 handler）", recorder.Code)
	}
}
