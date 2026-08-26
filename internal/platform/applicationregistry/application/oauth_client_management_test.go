package application

import "testing"

func TestValidRedirectURI(t *testing.T) {
	tests := []struct {
		name   string
		value  string
		policy RedirectURIValidationPolicy
		want   bool
	}{
		{name: "HTTPS callback", value: "https://portal.example.com/oauth/callback", want: true},
		{name: "HTTP server callback rejected by default", value: "http://10.20.30.40:8080/oauth/callback", want: false},
		{name: "HTTP hostname callback rejected by default", value: "http://portal.internal/oauth/callback", want: false},
		{name: "configured HTTP server callback", value: "http://10.20.30.40:8080/oauth/callback", policy: RedirectURIValidationPolicy{AllowInsecureHTTP: true}, want: true},
		{name: "configured HTTP hostname callback", value: "http://portal.internal/oauth/callback", policy: RedirectURIValidationPolicy{AllowInsecureHTTP: true}, want: true},
		{name: "localhost callback", value: "http://localhost:3000/oauth/callback", want: true},
		{name: "unsupported scheme", value: "ftp://portal.example.com/callback", want: false},
		{name: "credentials rejected", value: "https://user:password@portal.example.com/callback", want: false},
		{name: "fragment rejected", value: "https://portal.example.com/callback#section", want: false},
		{name: "relative path rejected", value: "/oauth/callback", want: false},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := validRedirectURI(test.value, test.policy); got != test.want {
				t.Fatalf("validRedirectURI(%q, policy=%+v) = %v, want %v", test.value, test.policy, got, test.want)
			}
		})
	}
}

func TestServerManagedBrokerPKCEExceptionIsNarrow(t *testing.T) {
	base := OAuthClientCreateInput{
		ClientID: "keycloak-broker", ClientType: "confidential", TokenAuthMethod: "client_secret_basic",
		GrantTypes: []string{"authorization_code", "refresh_token"}, RedirectURIs: []string{"https://keycloak.example/broker"},
	}
	if !isServerManagedBrokerClient(base) {
		t.Fatal("expected the dedicated platform broker to be recognized")
	}
	base.ClientID = "customer-web"
	if isServerManagedBrokerClient(base) {
		t.Fatal("ordinary browser client must not receive the broker exception")
	}
	base.ClientID = "keycloak-broker"
	base.TokenAuthMethod = "none"
	if isServerManagedBrokerClient(base) {
		t.Fatal("broker exception must require confidential client authentication")
	}
}
