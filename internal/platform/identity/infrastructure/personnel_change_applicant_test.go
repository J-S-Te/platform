package infrastructure

import (
	"encoding/json"
	"testing"
)

// TestPersonnelChangeListProjectsApplicant 验证审批读接口使用原提交人、保留缺姓名历史单据并限定租户。
// 参数：t 为测试上下文，查询由隔离的 DryRun 数据库捕获。
// 返回值：无；投影来源或连接条件缺失时测试失败。
func TestPersonnelChangeListProjectsApplicant(t *testing.T) {
	db, capture := newStatementCapturingDryRunDatabase(t)
	_, _ = NewPersonnelChangeGORMRepository(db).List(t.Context(), "tenant-1", "PENDING_APPROVAL", "", "")
	statements := capture.all()
	for _, fragment := range []string{
		"COALESCE(applicant.display_name, '') AS submitted_by_display_name",
		"LEFT JOIN iam_user AS applicant ON applicant.tenant_id = pcr.tenant_id AND applicant.id = pcr.submitted_by",
		"pcr.tenant_id = 'tenant-1'",
		"pcr.status = 'PENDING_APPROVAL'",
	} {
		assertStatementFragment(t, statements, fragment)
	}
}

// TestPersonnelChangeApplicantJSON 区分提交人、异动对象及历史姓名缺失。
// 参数：t 为测试上下文。
// 返回值：无；申请人投影被省略或误用异动对象时测试失败。
func TestPersonnelChangeApplicantJSON(t *testing.T) {
	row := personnelChangeModel{UserID: "employee-1", UserDisplayName: "异动人员", SubmittedBy: "submitter-1"}
	for _, name := range []string{"实际申请人", ""} {
		row.SubmittedByDisplayName = name
		body, err := json.Marshal(toPersonnel(row))
		if err != nil {
			t.Fatal(err)
		}
		var response map[string]any
		if err := json.Unmarshal(body, &response); err != nil {
			t.Fatal(err)
		}
		if response["user_display_name"] != "异动人员" || response["submitted_by"] != "submitter-1" {
			t.Fatalf("异动对象或提交人编号被替换：%s", body)
		}
		got, exists := response["submitted_by_display_name"]
		if (name == "" && exists) || (name != "" && got != name) {
			t.Fatalf("申请人字段错误：%s", body)
		}
	}
}
