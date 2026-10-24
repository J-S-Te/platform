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
	// 复职在后台 worker 中执行。若生成的密码只通过
	// Execute 返回，就会被丢弃，导致恢复的账号无法使用。
	if strings.Contains(string(encoded), "temporary_password") {
		t.Fatalf("personnel rehire response must not expose a temporary password: %s", encoded)
	}
}
