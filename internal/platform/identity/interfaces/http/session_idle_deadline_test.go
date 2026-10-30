package identityhttp

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/J-S-Te/Basic-Platform/internal/shared/authctx"
)

type idleDeadlineService struct {
	applicationService
}

// SessionIdleTimeout 为只读身份响应提供租户空闲策略。
// 参数：上下文和租户标识均由已认证请求提供。
// 返回值：三十分钟策略和空错误。
func (idleDeadlineService) SessionIdleTimeout(context.Context, string) (time.Duration, error) {
	return 30 * time.Minute, nil
}

// TestMeReturnsAuthoritativeRemainingWindow 验证剩余窗口、到期截断及兼容省略。
// 参数：t 为测试运行上下文。
// 返回值：无；不符合协议时报告失败。
func TestMeReturnsAuthoritativeRemainingWindow(t *testing.T) {
	for _, tc := range []struct {
		name          string
		deadline      time.Time
		wantRemaining bool
		min, max      int64
	}{
		{"remaining", time.Now().Add(5 * time.Second), true, 4000, 5000},
		{"expired", time.Now().Add(-time.Second), true, 0, 0},
		{"legacy", time.Time{}, false, 0, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			principal := authctx.Principal{
				SessionID: "internal-session", SessionIdleDeadline: tc.deadline,
				Tenant: authctx.ReferenceName{ID: "t1"},
			}
			request := httptest.NewRequest(http.MethodGet, "/auth/me", nil)
			request = request.WithContext(authctx.WithPrincipal(request.Context(), principal))
			response := httptest.NewRecorder()
			(&Handler{service: idleDeadlineService{}}).Me(response, request)
			if response.Code != http.StatusOK {
				t.Fatalf("status = %d", response.Code)
			}
			var body struct {
				Data map[string]json.RawMessage `json:"data"`
			}
			if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
				t.Fatal(err)
			}
			raw, exists := body.Data["idle_remaining_ms"]
			if exists != tc.wantRemaining {
				t.Fatalf("remaining exists = %v, body = %s", exists, response.Body)
			}
			if exists {
				var remaining int64
				if err := json.Unmarshal(raw, &remaining); err != nil {
					t.Fatal(err)
				}
				if remaining < tc.min || remaining > tc.max {
					t.Fatalf("remaining = %d", remaining)
				}
			}
			if strings.Contains(response.Body.String(), "internal-session") ||
				strings.Contains(response.Body.String(), "SessionIdleDeadline") {
				t.Fatalf("internal session data leaked: %s", response.Body)
			}
			if response.Header().Get("Cache-Control") != "no-store, private" {
				t.Fatal("identity response must not be cached")
			}
		})
	}
}
