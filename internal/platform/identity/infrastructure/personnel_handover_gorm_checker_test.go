package infrastructure

import "testing"

func TestPersonnelHandoverFailsClosedWithoutSnapshot(t *testing.T) {
	report := summarizeHandoverRows(nil)
	if report.Ready {
		t.Fatal("a termination without a responsibility snapshot must not be ready")
	}
}

func TestPersonnelHandoverRequiresEveryItemCompleted(t *testing.T) {
	rows := []personnelHandoverRow{
		{ID: "one", Status: "COMPLETED"},
		{ID: "two", Status: "TRANSFERRED"},
	}
	report := summarizeHandoverRows(rows)
	if report.Ready || len(report.Outstanding) != 1 || report.Outstanding[0].ID != "two" {
		t.Fatalf("transferred item must remain outstanding: %+v", report)
	}
	rows[1].Status = "COMPLETED"
	if completed := summarizeHandoverRows(rows); !completed.Ready || len(completed.Outstanding) != 0 {
		t.Fatalf("all completed items must be ready: %+v", completed)
	}
}
