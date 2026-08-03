package application

import "testing"

func TestValidAccessSettings(t *testing.T) {
	tests := []struct {
		name       string
		input      AccessSettingsUpdateInput
		wantNormal bool
	}{
		{name: "local only", input: AccessSettingsUpdateInput{TenantID: "tenant-1", Version: 1}, wantNormal: true},
		{name: "https public origin", input: AccessSettingsUpdateInput{TenantID: "tenant-1", PublicOrigin: "https://portal.example.com", Version: 1}, wantNormal: true},
		{name: "http non-loopback requires insecure flag", input: AccessSettingsUpdateInput{TenantID: "tenant-1", PublicOrigin: "http://47.111.20.119:8081", AllowInsecureHTTPRedirect: true, Version: 1}, wantNormal: true},
		{name: "loopback http allowed without flag", input: AccessSettingsUpdateInput{TenantID: "tenant-1", PublicOrigin: "http://127.0.0.1:8081", Version: 1}, wantNormal: true},
		{name: "trailing slash trimmed", input: AccessSettingsUpdateInput{TenantID: "tenant-1", PublicOrigin: "https://portal.example.com/", Version: 1}, wantNormal: true},
		{name: "empty tenant rejected", input: AccessSettingsUpdateInput{PublicOrigin: "", Version: 1}, wantNormal: false},
		{name: "zero version rejected", input: AccessSettingsUpdateInput{TenantID: "tenant-1", PublicOrigin: "", Version: 0}, wantNormal: false},
		{name: "relative path rejected", input: AccessSettingsUpdateInput{TenantID: "tenant-1", PublicOrigin: "/customer-portal", Version: 1}, wantNormal: false},
		{name: "hostname only rejected", input: AccessSettingsUpdateInput{TenantID: "tenant-1", PublicOrigin: "example.com", Version: 1}, wantNormal: false},
		{name: "ftp scheme rejected", input: AccessSettingsUpdateInput{TenantID: "tenant-1", PublicOrigin: "ftp://example.com", Version: 1}, wantNormal: false},
		{name: "userinfo rejected", input: AccessSettingsUpdateInput{TenantID: "tenant-1", PublicOrigin: "https://user:pass@example.com", Version: 1}, wantNormal: false},
		{name: "query rejected", input: AccessSettingsUpdateInput{TenantID: "tenant-1", PublicOrigin: "https://example.com?x=1", Version: 1}, wantNormal: false},
		{name: "http non-loopback auto-forces insecure flag", input: AccessSettingsUpdateInput{TenantID: "tenant-1", PublicOrigin: "http://47.111.20.119:8081", Version: 1}, wantNormal: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			normalized := normalizeAccessSettings(test.input)
			if got := validAccessSettings(normalized); got != test.wantNormal {
				t.Fatalf("validAccessSettings(%+v) = %v, want %v", normalized, got, test.wantNormal)
			}
		})
	}
}

func TestNormalizeAccessSettingsForcesInsecureHTTP(t *testing.T) {
	normalized := normalizeAccessSettings(AccessSettingsUpdateInput{
		TenantID: "tenant-1", PublicOrigin: "http://47.111.20.119:8081", Version: 1,
	})
	if !normalized.AllowInsecureHTTPRedirect {
		t.Fatal("http non-loopback public origin must force allow_insecure_http_redirect")
	}
}
