package http

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/J-S-Te/Basic-Platform/internal/platform/audit/domain"
)

// 统一审计方案 §2.2：查询响应必须同时给出名称快照与稳定定位字段；
// received_at 只在平台确实记录过接收时间时出现。
func TestEventResponseExposesStableIdentity(t *testing.T) {
	received := time.Date(2026, 10, 25, 1, 2, 3, 0, time.UTC)
	response := eventToResponse(domain.Event{
		EventID: "evt-1", OccurredAt: received.Add(-time.Minute), ReceivedAt: received,
		OperatorDisplayName: "张三", ActorID: "user-1", ActorType: "USER",
		Action: "SETTLEMENT_INVOICE_ISSUED", Result: "SUCCESS", ResourceType: "tax_invoice", ResourceID: "inv-1",
		RequestID: "req-1", CorrelationID: "corr-1", ReasonCode: "OK", BusinessID: "biz-1",
		ClientIP: "203.0.113.10", LoginIP: "198.51.100.7", LoginIPSource: "TRUSTED_PROXY", IPSource: "DIRECT",
	})
	payload, err := json.Marshal(response)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(payload, &decoded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	for key, want := range map[string]string{
		"actor_id": "user-1", "actor_type": "USER", "reason_code": "OK",
		"business_id": "biz-1", "correlation_id": "corr-1", "login_ip_source": "TRUSTED_PROXY",
		"operator_display_name": "张三",
	} {
		if got, _ := decoded[key].(string); got != want {
			t.Errorf("%s = %q, want %q", key, got, want)
		}
	}
	if decoded["received_at"] == nil {
		t.Error("received_at 缺失")
	}
}

// 没有接收时间的历史/合成事件不能编造 received_at。
func TestEventResponseOmitsZeroReceivedAt(t *testing.T) {
	response := eventToResponse(domain.Event{EventID: "evt-2"})
	payload, _ := json.Marshal(response)
	var decoded map[string]any
	_ = json.Unmarshal(payload, &decoded)
	if _, ok := decoded["received_at"]; ok {
		t.Fatal("零值接收时间不应输出 received_at")
	}
}
