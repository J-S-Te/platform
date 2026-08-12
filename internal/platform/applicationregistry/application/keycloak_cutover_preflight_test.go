package application

import "testing"

func TestValidateKeycloakCutoverTransportKeepsHTTPCompatibleUntilPolicyEnabled(t *testing.T) {
	transport, err := ValidateKeycloakCutoverTransport("http://47.111.20.119:8081", "/customer-portal", false)
	if err != nil {
		t.Fatalf("ValidateKeycloakCutoverTransport() error = %v", err)
	}
	if transport.RedirectURI != "http://47.111.20.119:8081/customer-portal/auth/callback" || transport.PublicURL != "http://47.111.20.119:8081/customer-portal/" || transport.CookieSecure {
		t.Fatalf("unexpected HTTP transport: %#v", transport)
	}
}

func TestValidateKeycloakCutoverTransportRequiresHTTPSOnlyWhenEnabled(t *testing.T) {
	if _, err := ValidateKeycloakCutoverTransport("http://platform.example.com", "/contract", true); err == nil {
		t.Fatal("HTTP cutover error = nil, want validation error when HTTPS policy is enabled")
	}
	transport, err := ValidateKeycloakCutoverTransport("https://platform.example.com", "/contract", true)
	if err != nil {
		t.Fatalf("HTTPS cutover error = %v", err)
	}
	if !transport.CookieSecure || transport.RedirectURI != "https://platform.example.com/contract/auth/callback" {
		t.Fatalf("unexpected HTTPS transport: %#v", transport)
	}
}

func TestValidateKeycloakCutoverTransportRejectsUnsafeBaseURL(t *testing.T) {
	for _, baseURL := range []string{
		"https://platform.example.com?token=secret",
		"https://user:password@platform.example.com",
		"ftp://platform.example.com",
		"https://platform.example.com/nested",
	} {
		if _, err := ValidateKeycloakCutoverTransport(baseURL, "/project", false); err == nil {
			t.Fatalf("baseURL %q error = nil, want validation error", baseURL)
		}
	}
}
