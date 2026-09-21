package application

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/J-S-Te/Basic-Platform/internal/platform/notification/domain"
)

func TestInboxPageUsesFrontendJSONFieldNames(t *testing.T) {
	readAt := time.Date(2026, time.September, 21, 10, 0, 0, 0, time.UTC)
	payload, err := json.Marshal(PageResult[domain.InboxItem]{
		Items: []domain.InboxItem{{
			DeliveryID: "delivery-1", MessageID: "message-1", Category: "TEAM_ASSIGNED",
			Title: "待办通知", Content: "请处理项目", DeliveredAt: readAt, ReadAt: nil,
		}},
		Page: 1, PageSize: 20, Total: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	wantFields := []string{`"items"`, `"delivery_id"`, `"message_id"`, `"category"`, `"title"`, `"content"`, `"delivered_at"`, `"page_size"`, `"total"`}
	for _, field := range wantFields {
		if !containsJSONField(payload, field) {
			t.Fatalf("serialized inbox payload is missing %s: %s", field, payload)
		}
	}
	if containsJSONField(payload, `"Items"`) || containsJSONField(payload, `"DeliveryID"`) {
		t.Fatalf("serialized inbox payload contains Go field names: %s", payload)
	}
}

func containsJSONField(payload []byte, field string) bool {
	return strings.Contains(string(payload), field)
}
