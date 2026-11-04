package infrastructure

import (
	"context"
	stdhttp "net/http"
	"net/http/httptest"
	"testing"
)

// TestLogoutSessionTargetsOnlyOneSID 验证空闲到期仅删除精确上游会话。
// 参数：t为测试上下文。
// 返回值：无；若枚举用户或调用全账号注销等额外路径则失败。
func TestLogoutSessionTargetsOnlyOneSID(t *testing.T) {
	deletes := 0
	server := httptest.NewServer(stdhttp.HandlerFunc(func(w stdhttp.ResponseWriter, r *stdhttp.Request) {
		switch r.Method + " " + r.URL.Path {
		case "POST /realms/master/protocol/openid-connect/token":
			writeJSON(t, w, 200, map[string]string{"access_token": "test-admin"})
		case "DELETE /admin/realms/acme/sessions/browser-sid":
			deletes++
			w.WriteHeader(204)
		default:
			t.Errorf("unexpected broad logout path: %s %s", r.Method, r.URL.Path)
			w.WriteHeader(500)
		}
	}))
	defer server.Close()
	admin, err := NewKeycloakAdmin(server.URL, "acme", "test-admin", "test-secret", server.Client())
	if err != nil {
		t.Fatal(err)
	}
	if err = admin.LogoutSession(context.Background(), "browser-sid"); err != nil {
		t.Fatal(err)
	}
	if deletes != 1 {
		t.Fatalf("expected one precise logout, got%d", deletes)
	}
}
