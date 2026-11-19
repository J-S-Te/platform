package http

import (
	"bufio"
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/J-S-Te/Basic-Platform/internal/shared/authctx"
)

// TestInboxStreamRequiresPrincipal 用途：验证 SSE 拒绝匿名订阅。
// 参数：t 为测试状态。
// 返回值：无。
func TestInboxStreamRequiresPrincipal(t *testing.T) {
	stream := NewInboxStream(nil, nil, "test_session", slog.New(slog.NewTextHandler(io.Discard, nil)))
	response := httptest.NewRecorder()
	stream.ServeHTTP(response, httptest.NewRequest("GET", "/stream", nil))
	if response.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", response.Code)
	}
}

// TestInboxStreamFlushesChanges 用途：验证连接立即就绪并只发送受众匹配的变更。
// 参数：t 为测试状态。
// 返回值：无。
func TestInboxStreamFlushesChanges(t *testing.T) {
	changes := make(chan struct{}, 1)
	subscribe := func(tenant, user string) (<-chan struct{}, func()) {
		if tenant != "tenant-a" || user != "user-a" {
			t.Errorf("unexpected subscription key: %s/%s", tenant, user)
		}
		return changes, func() {}
	}
	principal := authctx.Principal{SessionID: "session-a", Tenant: authctx.ReferenceName{ID: "tenant-a"}, User: authctx.ReferenceName{ID: "user-a"}}
	validate := func(context.Context, string) (authctx.Principal, error) { return principal, nil }
	stream := NewInboxStream(subscribe, validate, "test_session", slog.New(slog.NewTextHandler(io.Discard, nil)))
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		stream.ServeHTTP(w, r.WithContext(authctx.WithPrincipal(r.Context(), principal)))
	}))
	defer server.Close()
	client := &http.Client{Timeout: 3 * time.Second}
	request, _ := http.NewRequest("GET", server.URL, nil)
	request.AddCookie(&http.Cookie{Name: "test_session", Value: "session"})
	result, err := client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer result.Body.Close()
	if result.StatusCode != 200 || result.Header.Get("Content-Type") != "text/event-stream" {
		t.Fatalf("status = %d, headers = %v", result.StatusCode, result.Header)
	}
	reader := bufio.NewReader(result.Body)
	readStreamEvent(t, reader, "ready")
	changes <- struct{}{}
	readStreamEvent(t, reader, "inbox-changed")
	close(changes)
	if trailing, err := io.ReadAll(io.LimitReader(reader, 256)); err != nil || strings.Contains(string(trailing), "event:") {
		t.Fatalf("closed subscription did not end the stream: %q (%v)", trailing, err)
	}
}

// readStreamEvent 用途：从 SSE 响应读取下一个事件名。
// 参数：t 为测试状态，reader 为响应读取器，want 为应收到的事件名。
// 返回值：无。
func readStreamEvent(t *testing.T, reader *bufio.Reader, want string) {
	t.Helper()
	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			t.Fatal(err)
		}
		if name, ok := strings.CutPrefix(strings.TrimSpace(line), "event: "); ok {
			if name != want {
				t.Fatalf("event = %q, want %q", name, want)
			}
			return
		}
	}
}
