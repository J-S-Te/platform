package identityhttp

import (
	"context"
	"io"
	"log/slog"
	"testing"

	applicationregistry "github.com/J-S-Te/Basic-Platform/internal/platform/applicationregistry/application"
)

type loginTargetResolverStub struct {
	uri string
	err error
}

func (stub loginTargetResolverStub) ResolveActiveTargetURI(context.Context, applicationregistry.LoginTargetResolveInput) (string, error) {
	return stub.uri, stub.err
}

// 登录完成后的重定向必须再经过服务端来源判定：解析器返回未批准地址（仓储脏数据、
// 遗留明文地址或未来回归）时失败关闭为平台同源页面，绝不把浏览器送去该站点。
func TestResolveLoginRedirectRejectsUnapprovedDestination(t *testing.T) {
	handler := &Handler{
		logger:                  slog.New(slog.NewTextHandler(io.Discard, nil)),
		postLoginRedirectPolicy: applicationregistry.NewPostLoginRedirectPolicy("https://portal.example.com"),
	}
	payload := loginRequest{ApplicationID: "app-1", EnvironmentID: "env-1", LoginTargetCode: "home"}

	tests := []struct {
		name string
		uri  string
		err  error
		want string
	}{
		{
			name: "allowlisted cross origin destination",
			uri:  "https://portal.example.com/portal",
			want: "https://portal.example.com/portal",
		},
		{
			name: "unlisted https destination falls back to same origin",
			uri:  "https://evil.example.net/portal",
			want: "/",
		},
		{
			name: "non loopback plaintext destination falls back to same origin",
			uri:  "http://portal.example.com/portal",
			want: "/",
		},
		{
			name: "loopback plaintext destination stays acceptable",
			uri:  "http://127.0.0.1:8081/portal",
			want: "http://127.0.0.1:8081/portal",
		},
		{
			name: "scheme relative destination is rejected",
			uri:  "//evil.example.net/portal",
			want: "/",
		},
		{
			name: "resolution failure falls back to same origin",
			err:  applicationregistry.ErrNotFound,
			want: "/",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			handler.loginTargets = loginTargetResolverStub{uri: test.uri, err: test.err}
			got := handler.resolveLoginRedirect(context.Background(), "tenant-1", payload)
			if got != test.want {
				t.Fatalf("resolveLoginRedirect() = %q, want %q", got, test.want)
			}
		})
	}
}

// 未请求登录目标或未接线解析器时保持平台同源回退，不因默认值变化改变既有行为。
func TestResolveLoginRedirectKeepsSameOriginFallback(t *testing.T) {
	handler := &Handler{logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
	if got := handler.resolveLoginRedirect(context.Background(), "tenant-1", loginRequest{}); got != "/" {
		t.Fatalf("redirect without application id = %q, want %q", got, "/")
	}
	handler.loginTargets = loginTargetResolverStub{uri: "https://evil.example.net/portal"}
	if got := handler.resolveLoginRedirect(context.Background(), "tenant-1", loginRequest{ApplicationID: "app-1"}); got != "https://evil.example.net/portal" {
		t.Fatalf("unconfigured policy must keep the structural behaviour, got %q", got)
	}
}
