package infrastructure

import (
	registryhttp "github.com/J-S-Te/Basic-Platform/internal/platform/applicationregistry/interfaces/http"
	"testing"
	"time"
)

func TestKeycloakCutoverDistinguishesMissingObservationFromActiveWindow(t *testing.T) {
	now := time.Date(2026, 10, 27, 0, 0, 0, 0, time.UTC)
	past := now.Add(-time.Hour)
	future := now.Add(time.Hour)
	for _, tc := range []struct {
		name, status string
		end          *time.Time
		want         string
	}{
		{"never started", "NOT_STARTED", nil, "Keycloak observation has not started"},
		{"active window", "OBSERVING", &future, "Keycloak observation window has not completed"},
		{"malformed active window", "OBSERVING", nil, "Keycloak observation window has not completed"},
		{"window completed", "OBSERVING", &past, ""},
		{"already switched", "SWITCHED", &past, "Keycloak environment is already switched"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := keycloakCutoverGate(registryhttp.KeycloakCutoverLifecycle{Status: tc.status, ObservationEndsAt: tc.end}, now)
			got := ""
			if err != nil {
				got = err.Error()
			}
			if got != tc.want {
				t.Fatalf("got %q want %q", got, tc.want)
			}
		})
	}
}
