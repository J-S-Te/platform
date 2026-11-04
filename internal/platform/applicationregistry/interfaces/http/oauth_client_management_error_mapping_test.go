package http

import (
	"errors"
	"net/http/httptest"
	"testing"

	application "github.com/J-S-Te/Basic-Platform/internal/platform/applicationregistry/application"
)

// oauthClientErrorMappingService 仅用于错误映射测试：嵌入接口即可满足
// oauthClientManagementService 的方法集，映射路径不会调用任何服务方法。
type oauthClientErrorMappingService struct {
	oauthClientManagementService
}

// TestOAuthClientManagementErrorMapping 锁定 H-1 新增的 403 语义：
// 归属拒绝必须是 Forbidden 而不是内部错误；输入错误仍为 422、冲突仍为 409。
func TestOAuthClientManagementErrorMapping(t *testing.T) {
	handler := &OAuthClientManagementHandler{service: oauthClientErrorMappingService{}}
	cases := []struct {
		name string
		err  error
		want int
	}{
		{name: "forbidden", err: application.ErrManagementForbidden, want: 403},
		{name: "wrapped forbidden", err: errors.Join(errors.New("context"), application.ErrManagementForbidden), want: 403},
		{name: "validation", err: application.ErrManagementValidation, want: 422},
		{name: "conflict", err: application.ErrManagementConflict, want: 409},
		{name: "not found", err: application.ErrManagementNotFound, want: 404},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			request := httptest.NewRequest("POST", "/api/v1/oauth-clients", nil)
			handler.writeOAuthClientManagementError(recorder, request, test.err)
			if recorder.Code != test.want {
				t.Fatalf("status=%d, want %d", recorder.Code, test.want)
			}
		})
	}
}
