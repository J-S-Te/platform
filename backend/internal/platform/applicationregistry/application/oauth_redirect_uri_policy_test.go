package application

import "testing"

func TestValidRedirectURIDefaultPolicy(t *testing.T) {
	tests := []struct {
		name  string
		value string
		want  bool
	}{
		{name: "https public host", value: "https://contracts.example/auth/callback", want: true},
		{name: "http localhost", value: "http://localhost:8081/auth/callback", want: true},
		{name: "http loopback", value: "http://127.0.0.1:8081/auth/callback", want: true},
		{name: "http public ip", value: "http://115.159.219.156/contract/auth/callback", want: false},
		{name: "fragment", value: "https://contracts.example/auth/callback#fragment", want: false},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := validRedirectURI(test.value, false); got != test.want {
				t.Fatalf("validRedirectURI(%q, false) = %v, want %v", test.value, got, test.want)
			}
		})
	}
}

func TestValidRedirectURIAllowsPublicHTTPWhenExplicitlyEnabled(t *testing.T) {
	if !validRedirectURI("http://115.159.219.156/contract/auth/callback", true) {
		t.Fatal("explicit non-production policy rejected an absolute HTTP callback")
	}
}
