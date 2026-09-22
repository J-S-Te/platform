package infrastructure

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestRehireResponseNeverCarriesAnUndeliverableTemporaryPassword(t *testing.T) {
	model := personnelChangeModel{ID: "change-1", TenantID: "tenant-1", UserID: "user-1"}
	request := toPersonnel(model)
	encoded, err := json.Marshal(request)
	if err != nil {
		t.Fatal(err)
	}
	// Rehire executes in a background worker. A generated password returned only
	// from Execute would be discarded and make the restored account unusable.
	if strings.Contains(string(encoded), "temporary_password") {
		t.Fatalf("personnel rehire response must not expose a temporary password: %s", encoded)
	}
}
