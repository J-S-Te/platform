package http

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/J-S-Te/Basic-Platform/internal/platform/applicationregistry/application"
)

func TestEnvironmentDirectoryResponseNeverReturnsMetadataOrCredentials(t *testing.T) {
	t.Parallel()
	response := environmentToResponse(application.Environment{
		ID: "env-1", ApplicationID: "app-1", Environment: "prod", Status: "ACTIVE",
		Metadata: json.RawMessage(`{"display_name":"production","client_secret":"must-not-leak","nested":{"password":"must-not-leak"}}`),
	})
	body, err := json.Marshal(response)
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{"must-not-leak", "client_secret", "password", "metadata"} {
		if strings.Contains(string(body), forbidden) {
			t.Fatalf("directory response leaked %q: %s", forbidden, body)
		}
	}
}
