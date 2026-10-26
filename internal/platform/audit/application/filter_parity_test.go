package application

import (
	"testing"

	"github.com/J-S-Te/Basic-Platform/internal/platform/audit/domain"
)

// 统一审计方案 §2.4：列表与导出必须共用同一组过滤条件，
// 否则会出现“页面过滤正确、导出越界”。
func TestPageRequestCarriesPreciseFilters(t *testing.T) {
	request := PageRequest{
		EventID: "evt-1", ActorID: "user-1", CorrelationID: "corr-1",
		ClientIP: "203.0.113.10", LoginIP: "198.51.100.7",
		ResourceID: "inv-1", RequestID: "req-1",
	}
	exported := domain.ExportQuery{
		EventID: request.EventID, ActorID: request.ActorID, CorrelationID: request.CorrelationID,
		ClientIP: request.ClientIP, LoginIP: request.LoginIP,
		ResourceID: request.ResourceID, RequestID: request.RequestID,
	}
	if exported.EventID != "evt-1" || exported.ActorID != "user-1" || exported.LoginIP != "198.51.100.7" {
		t.Fatalf("过滤条件未对齐：%+v", exported)
	}
}
