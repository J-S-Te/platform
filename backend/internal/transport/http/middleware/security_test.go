package middleware

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
)

func TestClientIPUsesForwardingHeaderOnlyFromTrustedProxy(t *testing.T) {
	gin.SetMode(gin.TestMode)
	tests := []struct {
		name           string
		trustedProxies []string
		remoteAddress  string
		forwardedFor   string
		want           string
	}{
		{name: "trusted proxy", trustedProxies: []string{"192.0.2.10/32"}, remoteAddress: "192.0.2.10:1234", forwardedFor: "198.51.100.25", want: "198.51.100.25"},
		{name: "untrusted proxy", trustedProxies: []string{"127.0.0.1/32"}, remoteAddress: "192.0.2.10:1234", forwardedFor: "198.51.100.25", want: "192.0.2.10"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			router := gin.New()
			if err := router.SetTrustedProxies(test.trustedProxies); err != nil {
				t.Fatalf("SetTrustedProxies() error = %v", err)
			}
			router.Use(ClientIP())
			router.GET("/", func(context *gin.Context) { _, _ = context.Writer.WriteString(RequestClientIP(context.Request)) })

			request := httptest.NewRequest(http.MethodGet, "/", nil)
			request.RemoteAddr = test.remoteAddress
			request.Header.Set("X-Forwarded-For", test.forwardedFor)
			response := httptest.NewRecorder()
			router.ServeHTTP(response, request)
			if got := response.Body.String(); got != test.want {
				t.Fatalf("resolved client IP = %q, want %q", got, test.want)
			}
		})
	}
}

func TestRequireAllowedOriginForUnsafeMethods(t *testing.T) {
	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.Use(RequireAllowedOriginForUnsafeMethods("https://portal.example.com"))
	router.POST("/write", func(context *gin.Context) { context.Status(http.StatusNoContent) })

	tests := []struct {
		name         string
		origin       string
		secFetchSite string
		wantStatus   int
	}{
		{name: "same origin", origin: "https://portal.example.com", wantStatus: http.StatusNoContent},
		{name: "cross origin", origin: "https://evil.example", wantStatus: http.StatusForbidden},
		{name: "cross site browser without origin", secFetchSite: "cross-site", wantStatus: http.StatusForbidden},
		{name: "missing origin is rejected", wantStatus: http.StatusForbidden},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			request := httptest.NewRequest(http.MethodPost, "/write", nil)
			if test.origin != "" {
				request.Header.Set("Origin", test.origin)
			}
			if test.secFetchSite != "" {
				request.Header.Set("Sec-Fetch-Site", test.secFetchSite)
			}
			response := httptest.NewRecorder()
			router.ServeHTTP(response, request)
			if response.Code != test.wantStatus {
				t.Fatalf("status = %d, want %d", response.Code, test.wantStatus)
			}
		})
	}
}

func TestRequireSafeWriteContentType(t *testing.T) {
	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.Use(RequireSafeWriteContentType())
	router.POST("/write", func(context *gin.Context) { context.Status(http.StatusNoContent) })

	tests := []struct {
		name        string
		contentType string
		body        string
		wantStatus  int
	}{
		{name: "json", contentType: "application/json; charset=utf-8", body: `{}`, wantStatus: http.StatusNoContent},
		{name: "simple text body", contentType: "text/plain", body: `{}`, wantStatus: http.StatusUnsupportedMediaType},
		{name: "empty action", wantStatus: http.StatusNoContent},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			request := httptest.NewRequest(http.MethodPost, "/write", strings.NewReader(test.body))
			if test.contentType != "" {
				request.Header.Set("Content-Type", test.contentType)
			}
			response := httptest.NewRecorder()
			router.ServeHTTP(response, request)
			if response.Code != test.wantStatus {
				t.Fatalf("status = %d, want %d", response.Code, test.wantStatus)
			}
		})
	}
}
