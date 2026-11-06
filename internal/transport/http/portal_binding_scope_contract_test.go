package httptransport

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"regexp"
	"strings"
	"testing"

	registry "github.com/J-S-Te/Basic-Platform/internal/platform/applicationregistry/application"
	"github.com/J-S-Te/Basic-Platform/internal/shared/appctx"
	"github.com/J-S-Te/Basic-Platform/internal/transport/http/middleware"
	"github.com/gin-gonic/gin"
)

// TestPortalBindingRoutesMatchIssuedCredentialScopes 对照真实登记源验证路由接收签发scope。
// 参数：t为源码与中间件契约测试上下文。
// 返回值：无；用途名误作scope、点号scope被拒或旧下划线获准时失败。
func TestPortalBindingRoutesMatchIssuedCredentialScopes(t *testing.T) {
	issued := map[string]string{}
	for _, requirement := range registry.SubsystemUpdateServiceCredentialRequirements("customer_portal") {
		if requirement.Purpose == registry.ServiceCredentialPortalMappingProvision || requirement.Purpose == registry.ServiceCredentialPortalMappingDisable {
			if len(requirement.Scopes) != 1 {
				t.Fatal("binding credential must have one precise scope")
			}
			issued[requirement.Purpose] = requirement.Scopes[0]
		}
	}
	raw, err := os.ReadFile("router.go")
	if err != nil {
		t.Fatal(err)
	}
	count := 0
	for _, line := range strings.Split(string(raw), "\n") {
		if !strings.Contains(line, "integrationRouter.") || !strings.Contains(line, "/customer-binding\"") && !strings.Contains(line, "/customer-binding/disable\"") {
			continue
		}
		match := regexp.MustCompile(`RequireApplicationScope\("([^"]+)"\)`).FindStringSubmatch(line)
		if len(match) != 2 {
			t.Fatal("binding route missing exact scope")
		}
		purpose := registry.ServiceCredentialPortalMappingProvision
		if strings.Contains(line, "/disable\"") {
			purpose = registry.ServiceCredentialPortalMappingDisable
		}
		want := issued[purpose]
		if match[1] != want {
			t.Errorf("route scope %q differs from actually issued %q (purpose %q)", match[1], want, purpose)
		}
		for _, scope := range []string{want, purpose} {
			router := gin.New()
			router.Use(func(c *gin.Context) {
				c.Request = c.Request.WithContext(appctx.WithPrincipal(context.Background(), appctx.Principal{OAuthClientID: "oauth-fixture", ClientID: "portal-fixture", TenantID: "tenant-fixture", ApplicationID: "app-fixture", ApplicationCode: "customer_portal", EnvironmentID: "env-fixture", EnvironmentCode: "test", Scopes: map[string]struct{}{scope: {}}}))
			})
			router.GET("/binding", middleware.RequireApplicationScope(match[1]), func(c *gin.Context) { c.Status(200) })
			response := httptest.NewRecorder()
			router.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/binding", nil))
			expected := 403
			if scope == want {
				expected = 200
			}
			if response.Code != expected {
				t.Errorf("issued %q route %q status=%d want=%d", scope, match[1], response.Code, expected)
			}
		}
		count++
	}
	if count != 3 {
		t.Fatalf("binding route contract covered %d routes, expected 3", count)
	}
}

// TestExplicitBindingScopeTemplatesUseIssuedValues 验证生产显式scope不覆盖正确Worker默认值。
// 参数：t为本地模板契约测试。
// 返回值：无；用途名出现在_SCOPE值而非服务绑定键时失败。
func TestExplicitBindingScopeTemplatesUseIssuedValues(t *testing.T) {
	for _, path := range []string{"../../../deploy/production/compose.yaml", "../../../deploy/production/docker-compose.yml", "../../../docker/.env.customer.local.example"} {
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		for _, line := range strings.Split(string(raw), "\n") {
			if strings.Contains(line, "_SCOPE") && (strings.Contains(line, "portal_mapping_provision") || strings.Contains(line, "portal_mapping_disable")) {
				t.Errorf("template %s overrides registered scope: %s", path, line)
			}
		}
	}
}
