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
)

// 凭据加密登录的限流回归：账号在 credential_envelope 密文里，只有解密之后才能读到。
// 下面三个用例锁定“限流必须包在解密后的 handler 上”这一执行顺序。

const loginRateLimitHTTPDecryptPath = "/api/v1/auth/login"

// fakeCredentialDecrypt 按 credentialcrypto.WrapJSON 的真实顺序模拟解密：先用密文 body
// 进入，解密成功则把明文 body 与 Content-Type 还原后交给内层 handler。
func fakeCredentialDecrypt(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		var envelope struct {
			Ciphertext string `json:"ciphertext"`
		}
		if err := json.Unmarshal(body, &envelope); err != nil || envelope.Ciphertext == "" {
			// 密文缺失/损坏时不得进入业务链，等价于解密失败 400。
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		r.Body = io.NopCloser(strings.NewReader(envelope.Ciphertext))
		r.Header.Set("Content-Type", "application/json")
		next(w, r)
	}
}

// fakeLoginHandler 依据口令字段模拟认证结果：成功 204、认证失败 401、其它失败 403。
func fakeLoginHandler(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	var payload map[string]string
	if err := json.Unmarshal(body, &payload); err != nil {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	switch payload["password"] {
	case loginRateLimitSuccessPassword:
		w.WriteHeader(http.StatusNoContent)
	case "forbidden":
		w.WriteHeader(http.StatusForbidden)
	default:
		w.WriteHeader(http.StatusUnauthorized)
	}
}

// loginRateLimitHTTPCall 把账号/口令封装成凭据加密信封后投递（密文里才有明文账号）。
func loginRateLimitHTTPCall(t *testing.T, handler http.HandlerFunc, account, password, remoteAddr string) int {
	t.Helper()
	plain := fmt.Sprintf("{\"account\":%q,\"password\":%q}", account, password)
	envelope, err := json.Marshal(map[string]string{"ciphertext": plain})
	if err != nil {
		t.Fatalf("marshal envelope: %v", err)
	}
	request := httptest.NewRequest(http.MethodPost, loginRateLimitHTTPDecryptPath, strings.NewReader(string(envelope)))
	request.Header.Set("Content-Type", "application/json")
	request.RemoteAddr = remoteAddr
	recorder := httptest.NewRecorder()
	handler(recorder, request)
	return recorder.Code
}

// 解密之后的限流包装器必须按密文里的账号计桶：同账号失败触顶后 429，其它账号不受影响。
func TestLoginRateLimitHTTPCountsAccountAfterDecryption(t *testing.T) {
	handler := fakeCredentialDecrypt(LoginRateLimitHTTP("account", 1, 1000, time.Minute)(fakeLoginHandler))

	if code := loginRateLimitHTTPCall(t, handler, "victim@example.com", loginRateLimitFailurePassword, loginRateLimitSameIP); code != http.StatusUnauthorized {
		t.Fatalf("首次失败状态 = %d, want 401", code)
	}
	if code := loginRateLimitHTTPCall(t, handler, "victim@example.com", loginRateLimitFailurePassword, "198.51.100.9:4000"); code != http.StatusTooManyRequests {
		t.Fatalf("换 IP 后同账号状态 = %d, want 429（账号桶在解密后生效）", code)
	}
	if code := loginRateLimitHTTPCall(t, handler, "other@example.com", loginRateLimitFailurePassword, loginRateLimitSameIP); code != http.StatusUnauthorized {
		t.Fatalf("其它账号状态 = %d, want 401（不共享失败额度）", code)
	}
}

// 反向用例：保留“限流挂在解密外层”的旧顺序时，密文 body 里取不到 account，账号桶完全
// 失效——这正是本次修复的攻击面，必须由测试钉死。
func TestLoginRateLimitOutsideDecryptionCannotSeeAccount(t *testing.T) {
	handler := LoginRateLimitHTTP("account", 1, 1000, time.Minute)(fakeCredentialDecrypt(fakeLoginHandler))

	for attempt := 0; attempt < 3; attempt++ {
		if code := loginRateLimitHTTPCall(t, handler, "victim@example.com", loginRateLimitFailurePassword, loginRateLimitSameIP); code != http.StatusUnauthorized {
			t.Fatalf("外层限流第 %d 次状态 = %d, want 401（密文里读不到账号，账号桶未计数）", attempt+1, code)
		}
	}
}

// IP 兜底桶在 HTTP 包装器里保持“所有放行请求都计数”的语义。
func TestLoginRateLimitHTTPKeepsIPFallbackBucket(t *testing.T) {
	handler := fakeCredentialDecrypt(LoginRateLimitHTTP("account", 100, 2, time.Minute)(fakeLoginHandler))

	for attempt := 0; attempt < 2; attempt++ {
		account := fmt.Sprintf("rotating-%d@example.com", attempt)
		if code := loginRateLimitHTTPCall(t, handler, account, loginRateLimitSuccessPassword, loginRateLimitSameIP); code != http.StatusNoContent {
			t.Fatalf("随机账号 %d 状态 = %d, want 204", attempt, code)
		}
	}
	if code := loginRateLimitHTTPCall(t, handler, "one-more@example.com", loginRateLimitSuccessPassword, loginRateLimitSameIP); code != http.StatusTooManyRequests {
		t.Fatalf("IP 兜底状态 = %d, want 429", code)
	}
}

// 成功登录清零账号失败计数，HTTP 包装器必须与 gin 中间件语义一致（M-2）。
func TestLoginRateLimitHTTPResetsFailuresOnSuccess(t *testing.T) {
	handler := fakeCredentialDecrypt(LoginRateLimitHTTP("account", 2, 1000, time.Minute)(fakeLoginHandler))

	if code := loginRateLimitHTTPCall(t, handler, "user@example.com", loginRateLimitFailurePassword, loginRateLimitSameIP); code != http.StatusUnauthorized {
		t.Fatalf("首次失败状态 = %d, want 401", code)
	}
	if code := loginRateLimitHTTPCall(t, handler, "user@example.com", loginRateLimitSuccessPassword, loginRateLimitSameIP); code != http.StatusNoContent {
		t.Fatalf("成功登录状态 = %d, want 204", code)
	}
	if code := loginRateLimitHTTPCall(t, handler, "user@example.com", loginRateLimitFailurePassword, loginRateLimitSameIP); code != http.StatusUnauthorized {
		t.Fatalf("清零后失败状态 = %d, want 401", code)
	}
	if code := loginRateLimitHTTPCall(t, handler, "user@example.com", loginRateLimitFailurePassword, loginRateLimitSameIP); code != http.StatusUnauthorized {
		t.Fatalf("清零后第二次失败状态 = %d, want 401（额度已重置）", code)
	}
	if code := loginRateLimitHTTPCall(t, handler, "user@example.com", loginRateLimitFailurePassword, loginRateLimitSameIP); code != http.StatusTooManyRequests {
		t.Fatalf("再次触顶状态 = %d, want 429", code)
	}
}

// 限流层仍必须把请求体原样交给业务 handler（密码校验由 handler 解析）。
func TestLoginRateLimitHTTPRestoresBodyForHandler(t *testing.T) {
	var seen string
	handler := LoginRateLimitHTTP("account", 5, 1000, time.Minute)(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		seen = string(body)
		w.WriteHeader(http.StatusNoContent)
	})
	request := httptest.NewRequest(http.MethodPost, loginRateLimitHTTPDecryptPath, strings.NewReader(fmt.Sprintf("{\"account\":%q,\"password\":%q}", "body@example.com", loginRateLimitSuccessPassword)))
	request.Header.Set("Content-Type", "application/json")
	request.RemoteAddr = loginRateLimitSameIP
	recorder := httptest.NewRecorder()

	handler(recorder, request)

	if recorder.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204", recorder.Code)
	}
	if seen != fmt.Sprintf("{\"account\":%q,\"password\":%q}", "body@example.com", loginRateLimitSuccessPassword) {
		t.Fatalf("handler 读到的 body = %q，未原样还原", seen)
	}
}
