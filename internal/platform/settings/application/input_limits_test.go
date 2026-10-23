package application

import (
	"strings"
	"testing"
)

func TestAccessSettingsRejectsOversizedPublicOrigin(t *testing.T) {
	input := AccessSettingsUpdateInput{
		TenantID: "tenant", Version: 1,
		PublicOrigin: "https://example.com/" + strings.Repeat("a", 500),
	}
	if validAccessSettings(input) {
		t.Fatal("oversized public origin was accepted")
	}
}
