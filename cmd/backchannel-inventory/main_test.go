package main

import (
	"bytes"
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/J-S-Te/Basic-Platform/internal/platform/oidc/backchannel"
)

func inventoryFixture() []backchannel.UnregisteredClient {
	return []backchannel.UnregisteredClient{
		{TenantID: "01KTENANT00000000000000001", OAuthClientID: "01KOAUTH000000000000000001", ClientID: "settlement-prod-web", ActiveFamilies: 4},
		{TenantID: "01KTENANT00000000000000001", OAuthClientID: "01KOAUTH000000000000000002", ClientID: "contract-prod-web", ActiveFamilies: 1},
	}
}

// 只读盘点的输出必须能定位影响面（tenant/client/活跃 family 数），且不含任何凭据材料。
func TestBackchannelInventoryTableReportsGapsWithoutSecrets(t *testing.T) {
	var buffer bytes.Buffer
	if err := writeInventoryTable(&buffer, inventoryFixture()); err != nil {
		t.Fatal(err)
	}
	output := buffer.String()
	for _, expected := range []string{
		"TENANT", "CLIENT_ID", "OAUTH_CLIENT_ID", "ACTIVE_FAMILIES",
		"01KTENANT00000000000000001", "settlement-prod-web", "01KOAUTH000000000000000001", "4",
		"contract-prod-web", "1",
	} {
		if !strings.Contains(output, expected) {
			t.Fatalf("inventory output missing %q:\n%s", expected, output)
		}
	}
	for _, forbidden := range []string{"secret", "SEGRET", "password", "Bearer", "eyJ"} {
		if strings.Contains(strings.ToLower(output), strings.ToLower(forbidden)) {
			t.Fatalf("inventory output leaked %q:\n%s", forbidden, output)
		}
	}
}

// 空缺口必须显式说明，避免被误读成"工具没跑"。
func TestBackchannelInventoryTableStatesNoGaps(t *testing.T) {
	var buffer bytes.Buffer
	if err := writeInventoryTable(&buffer, nil); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(buffer.String(), "没有缺少 Back-Channel Logout 登记") {
		t.Fatalf("empty inventory output = %q", buffer.String())
	}
}

// JSON 输出字段固定且与表格同一投影；解析后不得出现额外字段（防止未来误加凭据列）。
func TestBackchannelInventoryJSONIsStableAndSecretFree(t *testing.T) {
	var buffer bytes.Buffer
	if err := writeInventoryJSON(&buffer, inventoryFixture()); err != nil {
		t.Fatal(err)
	}
	var rows []map[string]any
	if err := json.Unmarshal(buffer.Bytes(), &rows); err != nil {
		t.Fatalf("inventory JSON is invalid: %v\n%s", err, buffer.String())
	}
	if len(rows) != 2 {
		t.Fatalf("inventory JSON rows = %d, want 2", len(rows))
	}
	want := map[string]struct{}{"tenant_id": {}, "client_id": {}, "oauth_client_id": {}, "active_families": {}}
	for _, row := range rows {
		if len(row) != len(want) {
			t.Fatalf("inventory JSON row fields = %d, want %d: %v", len(row), len(want), row)
		}
		for key := range row {
			if _, ok := want[key]; !ok {
				t.Fatalf("inventory JSON leaked unexpected field %q", key)
			}
		}
	}
}

// 盘点行结构体本身也不允许出现凭据/会话字段：这是"禁止输出凭据"的结构性保证。
func TestBackchannelInventoryRowHasNoCredentialFields(t *testing.T) {
	rowType := reflect.TypeOf(inventoryRow{})
	for index := 0; index < rowType.NumField(); index++ {
		field := strings.ToLower(rowType.Field(index).Name)
		for _, forbidden := range []string{"secret", "password", "token", "session", "credential"} {
			if strings.Contains(field, forbidden) {
				t.Fatalf("inventory row exposes %q", rowType.Field(index).Name)
			}
		}
	}
}
