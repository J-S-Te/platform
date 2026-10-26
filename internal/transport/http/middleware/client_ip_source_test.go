package middleware

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/J-S-Te/Basic-Platform/internal/shared/requestctx"
	"github.com/gin-gonic/gin"
)

// 来源必须由解析过程给出：直连、非可信对端伪造代理头、可信代理、无可信转发头，
// 四种情况的地址与标记都要独立且可断言。
func TestClientIPReportsParseSource(t *testing.T) {
	cases := []struct {
		name       string
		trusted    []string
		remoteAddr string
		header     string
		wantIP     string
		wantSource string
	}{
		{"直连不带代理头", []string{"192.0.2.10/32"}, "203.0.113.7:5000", "", "203.0.113.7", ipSourceDirect},
		{"非可信对端伪造代理头", []string{"192.0.2.10/32"}, "203.0.113.7:5000", "198.51.100.25", "203.0.113.7", ipSourceDirect},
		{"单级可信代理", []string{"192.0.2.10/32"}, "192.0.2.10:5000", "198.51.100.25", "198.51.100.25", ipSourceTrustedProxy},
		{"可信代理但没有转发头", []string{"192.0.2.10/32"}, "192.0.2.10:5000", "", "192.0.2.10", ipSourceDirect},
		{"无可信配置时一律直连", nil, "192.0.2.10:5000", "198.51.100.25", "192.0.2.10", ipSourceDirect},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			gin.SetMode(gin.TestMode)
			router := gin.New()
			if err := router.SetTrustedProxies(test.trusted); err != nil {
				t.Fatalf("SetTrustedProxies: %v", err)
			}
			var gotIP, gotSource string
			router.Use(ClientIP(test.trusted))
			router.GET("/", func(context *gin.Context) {
				gotIP, gotSource = requestctx.ClientIPWithSource(context.Request.Context())
				context.Status(http.StatusNoContent)
			})
			request := httptest.NewRequest(http.MethodGet, "/", nil)
			request.RemoteAddr = test.remoteAddr
			if test.header != "" {
				request.Header.Set("X-Forwarded-For", test.header)
			}
			router.ServeHTTP(httptest.NewRecorder(), request)
			if gotIP != test.wantIP || gotSource != test.wantSource {
				t.Fatalf("resolved = (%q,%q), want (%q,%q)", gotIP, gotSource, test.wantIP, test.wantSource)
			}
		})
	}
}
