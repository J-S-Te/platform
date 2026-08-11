package middleware

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/J-S-Te/Basic-Platform/internal/shared/appctx"
	"github.com/J-S-Te/Basic-Platform/internal/shared/authctx"
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

type testConsoleAuthenticator struct{}

func (testConsoleAuthenticator) Authenticate(_ context.Context, token string) (authctx.Principal, error) {
	if token != "session" {
		return authctx.Principal{}, errors.New("invalid session")
	}
	return authctx.Principal{Tenant: authctx.ReferenceName{ID: "tenant"}, User: authctx.ReferenceName{ID: "user"}}, nil
}

func TestAuthenticationDisablesCachingAcrossSessionCookies(t *testing.T) {
	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.Use(Authentication(testConsoleAuthenticator{}, "session"))
	router.GET("/principal", func(context *gin.Context) { context.Status(http.StatusNoContent) })

	request := httptest.NewRequest(http.MethodGet, "/principal", nil)
	request.AddCookie(&http.Cookie{Name: "session", Value: "session"})
	response := httptest.NewRecorder()
	router.ServeHTTP(response, request)

	if response.Code != http.StatusNoContent {
		t.Fatalf("status = %d", response.Code)
	}
	if got := response.Header().Get("Cache-Control"); got != "no-store, private" {
		t.Fatalf("Cache-Control = %q", got)
	}
	if got := response.Header().Get("Pragma"); got != "no-cache" {
		t.Fatalf("Pragma = %q", got)
	}
	if got := response.Header().Get("Vary"); got != "Cookie" {
		t.Fatalf("Vary = %q", got)
	}
}

type testApplicationAuthenticator struct{}

func (testApplicationAuthenticator) Authenticate(_ context.Context, token string) (appctx.Principal, error) {
	if token != "application-token" {
		return appctx.Principal{}, errors.New("invalid application token")
	}
	return appctx.Principal{
		OAuthClientID:   "client",
		ClientID:        "client",
		TenantID:        "tenant",
		ApplicationID:   "application",
		ApplicationCode: "sample",
		EnvironmentID:   "environment",
		EnvironmentCode: "dev",
		Scopes:          map[string]struct{}{"authorization.catalog.sync": {}},
	}, nil
}

func TestRequireAllowedOriginForUnsafeMethodsOrBearer(t *testing.T) {
	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.Use(
		RequireAllowedOriginForUnsafeMethodsOrBearer("https://portal.example.com"),
		ConsoleOrApplicationAuthentication(testConsoleAuthenticator{}, "session", testApplicationAuthenticator{}),
	)
	router.PUT("/catalog", func(context *gin.Context) { context.Status(http.StatusNoContent) })

	tests := []struct {
		name          string
		authorization string
		origin        string
		secFetchSite  string
		cookie        *http.Cookie
		wantStatus    int
	}{
		{
			name:          "valid bearer without cookie or origin reaches application authentication",
			authorization: "Bearer application-token",
			wantStatus:    http.StatusNoContent,
		},
		{
			name:          "syntactically valid but invalid bearer is rejected by authentication",
			authorization: "Bearer invalid-token",
			wantStatus:    http.StatusUnauthorized,
		},
		{
			name:          "session cookie and fake authorization without origin is forbidden",
			authorization: "not-a-real-authorization",
			cookie:        &http.Cookie{Name: "session", Value: "session"},
			wantStatus:    http.StatusForbidden,
		},
		{
			name:          "session cookie and valid bearer without origin is forbidden",
			authorization: "Bearer application-token",
			cookie:        &http.Cookie{Name: "session", Value: "session"},
			wantStatus:    http.StatusForbidden,
		},
		{
			name:          "session cookie with allowed origin uses console authentication",
			authorization: "not-a-real-authorization",
			origin:        "https://portal.example.com",
			cookie:        &http.Cookie{Name: "session", Value: "session"},
			wantStatus:    http.StatusNoContent,
		},
		{
			name:          "unrelated cookie also requires origin",
			authorization: "Bearer application-token",
			cookie:        &http.Cookie{Name: "preferences", Value: "compact"},
			wantStatus:    http.StatusForbidden,
		},
		{
			name:          "basic authorization cannot bypass missing origin",
			authorization: "Basic Y2xpZW50OnNlY3JldA==",
			wantStatus:    http.StatusForbidden,
		},
		{
			name:          "arbitrary authorization cannot bypass missing origin",
			authorization: "ApiKey secret",
			wantStatus:    http.StatusForbidden,
		},
		{
			name:          "malformed bearer token68 cannot bypass missing origin",
			authorization: "Bearer invalid,token",
			wantStatus:    http.StatusForbidden,
		},
		{
			name:          "bearer with extra field cannot bypass missing origin",
			authorization: "Bearer application-token extra",
			wantStatus:    http.StatusForbidden,
		},
		{
			name:          "cross origin valid bearer is forbidden",
			authorization: "Bearer application-token",
			origin:        "https://evil.example",
			wantStatus:    http.StatusForbidden,
		},
		{
			name:          "cross site fetch valid bearer without origin is forbidden",
			authorization: "Bearer application-token",
			secFetchSite:  "cross-site",
			wantStatus:    http.StatusForbidden,
		},
		{
			name:          "same origin valid bearer is allowed",
			authorization: "Bearer application-token",
			origin:        "https://portal.example.com",
			wantStatus:    http.StatusNoContent,
		},
		{
			name:       "missing authorization and origin is forbidden",
			wantStatus: http.StatusForbidden,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			request := httptest.NewRequest(http.MethodPut, "/catalog", strings.NewReader(`{"catalog_version":"1"}`))
			if test.authorization != "" {
				request.Header.Set("Authorization", test.authorization)
			}
			if test.origin != "" {
				request.Header.Set("Origin", test.origin)
			}
			if test.secFetchSite != "" {
				request.Header.Set("Sec-Fetch-Site", test.secFetchSite)
			}
			if test.cookie != nil {
				request.AddCookie(test.cookie)
			}

			response := httptest.NewRecorder()
			router.ServeHTTP(response, request)
			if response.Code != test.wantStatus {
				t.Fatalf("status = %d, want %d", response.Code, test.wantStatus)
			}
		})
	}
}

func TestHasStrictBearerAuthorization(t *testing.T) {
	tests := []struct {
		header string
		want   bool
	}{
		{header: "Bearer abcDEF012-._~+/==", want: true},
		{header: "bearer application-token", want: true},
		{header: "Bearer"},
		{header: "Basic abc"},
		{header: "Bearer abc def"},
		{header: "Bearer invalid,token"},
		{header: "Bearer =padding"},
		{header: "Bearer abc=def"},
	}
	for _, test := range tests {
		t.Run(test.header, func(t *testing.T) {
			if got := hasStrictBearerAuthorization(test.header); got != test.want {
				t.Fatalf("hasStrictBearerAuthorization(%q) = %v, want %v", test.header, got, test.want)
			}
		})
	}
}
