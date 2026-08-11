package infrastructure

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/J-S-Te/Basic-Platform/internal/platform/identity/application"
)

func TestRehireTemporaryPasswordIsStrongAndNeverMappedFromPersistence(t *testing.T) {
	password, err := (application.CryptoPasswordGenerator{}).Generate()
	if err != nil {
		t.Fatal(err)
	}
	if len(password) != 24 || !strings.ContainsAny(password, "ABCDEFGHIJKLMNOPQRSTUVWXYZ") || !strings.ContainsAny(password, "abcdefghijklmnopqrstuvwxyz") || !strings.ContainsAny(password, "23456789") || !strings.ContainsAny(password, "!@#$%^&*-_=+") {
		t.Fatalf("generated rehire password does not meet policy: %q", password)
	}
	model := personnelChangeModel{ID: "change-1", TenantID: "tenant-1", UserID: "user-1"}
	request := toPersonnel(model)
	request.TemporaryPassword = "must-not-be-persisted"
	encoded, err := json.Marshal(request)
	if err != nil {
		t.Fatal(err)
	}
	// The transient field is only present when explicitly set by the immediate
	// execution response; a freshly loaded persistence model never carries it.
	loaded := toPersonnel(model)
	if loaded.TemporaryPassword != "" || strings.Contains(string(encoded), "must-not-be-persisted") == false {
		t.Fatalf("unexpected transient password mapping: loaded=%q json=%s", loaded.TemporaryPassword, encoded)
	}
}
