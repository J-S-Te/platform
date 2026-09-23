package middleware

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/J-S-Te/Basic-Platform/internal/shared/authctx"
	"github.com/gin-gonic/gin"
)

// 安全审查 SEC-B8：路由级 RequirePermission 必须对缺少权限码的已认证主体返回 403、
// 对未认证请求返回 401、对持有权限码的主体放行——这是组织/岗位/任职10条路由
// 补挂的同一中间件。
func TestRequirePermissionRejectsWithForbiddenWhenCodeMissing(t *testing.T) {
	gin.SetMode(gin.TestMode)
	tests := []struct {
		name       string
		principal  *authctx.Principal
		wantStatus int
	}{
		{name: "未认证请求", principal: nil, wantStatus: http.StatusUnauthorized},
		{name: "已认证但缺少权限码", principal: &authctx.Principal{
			Tenant:          authctx.ReferenceName{ID: "tenant-1"},
			User:            authctx.ReferenceName{ID: "user-1"},
			PermissionCodes: []string{"platform:user:read"},
		}, wantStatus: http.StatusForbidden},
		{name: "持有匹配权限码放行", principal: &authctx.Principal{
			Tenant:          authctx.ReferenceName{ID: "tenant-1"},
			User:            authctx.ReferenceName{ID: "user-1"},
			PermissionCodes: []string{"platform:organization:create"},
		}, wantStatus: http.StatusNoContent},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			router := gin.New()
			router.Use(func(context *gin.Context) {
				if test.principal != nil {
					context.Request = context.Request.WithContext(authctx.WithPrincipal(context.Request.Context(), *test.principal))
				}
			})
			router.POST("/org-units", RequirePermission("platform:organization:create"), func(context *gin.Context) {
				context.Status(http.StatusNoContent)
			})
			recorder := httptest.NewRecorder()
			router.ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/org-units", nil))
			if recorder.Code != test.wantStatus {
				t.Fatalf("status = %d, want %d (body=%s)", recorder.Code, test.wantStatus, recorder.Body.String())
			}
		})
	}
}
