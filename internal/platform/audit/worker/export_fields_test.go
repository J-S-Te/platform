package worker

import (
	"encoding/csv"
	"os"
	"testing"
	"time"

	"github.com/J-S-Te/Basic-Platform/internal/platform/audit/domain"
)

// 统一审计方案 §2.4：列表、详情、导出必须对同一条件返回一致的事件范围与字段。
// 导出新增稳定定位字段与三类地址，且历史空值以空单元格输出（不编造）。
func TestExportIncludesStableIdentityColumns(t *testing.T) {
	worker := &ExportWorker{}
	received := time.Date(2026, 10, 25, 2, 3, 4, 0, time.UTC)
	event := domain.Event{
		EventID: "evt-1", OccurredAt: received.Add(-time.Hour), ReceivedAt: received,
		ApplicationCode: "settlement", EnvironmentCode: "dev", Action: "SETTLEMENT_INVOICE_ISSUED",
		Result: "SUCCESS", ResourceType: "tax_invoice", ResourceID: "inv-1", OperatorDisplayName: "张三",
		ActorID: "user-1", ActorType: "USER", ReasonCode: "OK", BusinessID: "biz-1",
		ClientIP: "203.0.113.10", LoginIP: "198.51.100.7", LoginIPSource: "TRUSTED_PROXY",
		IngestPeerIP: "172.31.255.32", IPSource: "DIRECT",
	}
	history := domain.Event{EventID: "evt-2", OccurredAt: received}
	file, path, err := worker.writeCSV(domain.ExportWork{TenantID: "tenant-1", ApplicationID: "app-1", JobID: "job-1"}, []domain.Event{event, history})
	if err != nil {
		t.Fatalf("writeCSV: %v", err)
	}
	defer os.Remove(path)
	if file.StorageRelativePath == "" || file.SizeBytes == 0 {
		t.Fatalf("导出文件元数据不完整：%+v", file)
	}
	stream, err := os.Open(path)
	if err != nil {
		t.Fatalf("open export: %v", err)
	}
	defer stream.Close()
	records, err := csv.NewReader(stream).ReadAll()
	if err != nil {
		t.Fatalf("read csv: %v", err)
	}
	if len(records) != 3 {
		t.Fatalf("rows = %d, want header + 2 events", len(records))
	}
	columns := map[string]int{}
	for index, name := range records[0] {
		columns[name] = index
	}
	for _, name := range []string{"actor_id", "actor_type", "reason_code", "business_id", "login_ip", "login_ip_source", "ingest_peer_ip", "ip_source", "received_at"} {
		if _, ok := columns[name]; !ok {
			t.Fatalf("缺少导出列 %q", name)
		}
	}
	if got := records[1][columns["actor_id"]]; got != "user-1" {
		t.Fatalf("actor_id = %q", got)
	}
	if got := records[1][columns["login_ip_source"]]; got != "TRUSTED_PROXY" {
		t.Fatalf("login_ip_source = %q", got)
	}
	// 历史缺字段数据保持空单元格，不能伪装成已采集。
	if got := records[2][columns["login_ip"]]; got != "" {
		t.Fatalf("历史事件 login_ip = %q, want 空值", got)
	}
	if got := records[2][columns["received_at"]]; got != "" {
		t.Fatalf("历史事件 received_at = %q, want 空值", got)
	}
}
