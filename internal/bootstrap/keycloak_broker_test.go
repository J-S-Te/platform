package bootstrap

import "testing"

func TestKeycloakBrokerEnvironment(t *testing.T) {
	tests := []struct {
		name           string
		appEnvironment string
		want           string
	}{
		{name: "production uses prod", appEnvironment: "production", want: "prod"},
		{name: "development uses dev", appEnvironment: "development", want: "dev"},
		{name: "empty uses dev", appEnvironment: "", want: "dev"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := keycloakBrokerEnvironment(tt.appEnvironment); got != tt.want {
				t.Fatalf("keycloakBrokerEnvironment(%q) = %q, want %q", tt.appEnvironment, got, tt.want)
			}
		})
	}
}
