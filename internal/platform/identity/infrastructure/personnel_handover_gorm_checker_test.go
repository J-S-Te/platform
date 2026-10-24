package infrastructure

import "testing"

func TestPersonnelHandoverFailsClosedWithoutSnapshot(t *testing.T) {
	report := summarizeHandoverRows(nil)
	if report.Ready {
		t.Fatal("a termination without a responsibility snapshot must not be ready")
	}
}

// 本地人工签认（COMPLETED）与编排依据真实回执推进的结清状态（TRANSFERRED/SKIPPED）
// 都表示本地已无未处理责任；远端完成的严格核验由跨系统协调器另行把关。
func TestSummarizeHandoverRowsAcceptsResolvedResponsibilities(t *testing.T) {
	report := summarizeHandoverRows([]personnelHandoverRow{
		{ID: "local-1", System: "platform", Status: "COMPLETED"},
		{ID: "remote-1", System: "contract", Status: "TRANSFERRED"},
		{ID: "remote-2", System: "contract", Status: "SKIPPED"},
	})
	if !report.Ready || len(report.Outstanding) != 0 {
		t.Fatalf("report = %+v, want all resolved responsibilities to be ready", report)
	}
}

// 已完成的状态不能被再次“完成”覆盖；跨系统回执结清的项与本地人工签认同等处理。
func TestHandoverItemResolvedStatusesCannotBeCompletedAgain(t *testing.T) {
	for _, status := range []string{"COMPLETED", "TRANSFERRED", "SKIPPED"} {
		if !handoverItemResolved(status) {
			t.Fatalf("status %s must be treated as resolved", status)
		}
	}
	for _, status := range []string{"PENDING", "FAILED", "BLOCKED", "UNKNOWN", ""} {
		if handoverItemResolved(status) {
			t.Fatalf("status %s must remain actionable", status)
		}
	}
}

func TestSummarizeHandoverRowsKeepsUnresolvedItemsOutstanding(t *testing.T) {
	for _, status := range []string{"PENDING", "FAILED", "BLOCKED", "UNKNOWN"} {
		report := summarizeHandoverRows([]personnelHandoverRow{
			{ID: "local-1", System: "platform", Status: "COMPLETED"},
			{ID: "remote-1", System: "contract", Status: status},
		})
		if report.Ready || len(report.Outstanding) != 1 || report.Outstanding[0].ID != "remote-1" {
			t.Fatalf("status %s must stay outstanding: %+v", status, report)
		}
	}
}
