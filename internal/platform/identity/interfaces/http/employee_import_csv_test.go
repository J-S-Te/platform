package identityhttp

import "testing"

func TestParseEmployeeCSVUsesAuthoritativeFileAndSelection(t *testing.T) {
	content := []byte("\ufeff姓名,邮箱,手机号,状态,组织,岗位,应用角色\n张三,a@example.com,,启用,销售部,销售员,crm:sales\n李四,,,停用,技术部,工程师,项目管理系统：项目经理\n")
	request, err := parseEmployeeCSV(content, []int{3})
	if err != nil {
		t.Fatalf("parse employee CSV: %v", err)
	}
	if len(request.Items) != 1 || request.Items[0].DisplayName != "李四" || request.Items[0].LineNo != 3 || request.Items[0].Organization != "技术部" {
		t.Fatalf("unexpected request: %#v", request)
	}
	if got := request.Items[0].ApplicationRoles; len(got) != 1 || got[0].ApplicationName != "项目管理系统" || got[0].RoleName != "项目经理" {
		t.Fatalf("unexpected roles: %#v", got)
	}
}

func TestParseEmployeeCSVRejectsUnknownSelectedLine(t *testing.T) {
	if _, err := parseEmployeeCSV([]byte("姓名,组织,岗位\n张三,销售部,销售员\n"), []int{99}); err == nil {
		t.Fatal("expected invalid selected line to fail")
	}
}

func TestParseEmployeeCSVStripsBOMWithoutHeader(t *testing.T) {
	content := append([]byte{0xEF, 0xBB, 0xBF}, []byte("张三,a@example.com,,启用,销售部,销售员,crm:sales\n")...)
	request, err := parseEmployeeCSV(content, nil)
	if err != nil {
		t.Fatalf("parse employee CSV: %v", err)
	}
	if len(request.Items) != 1 || request.Items[0].DisplayName != "张三" {
		t.Fatalf("BOM leaked into first column: %#v", request.Items)
	}
}

func TestParseEmployeeCSVRejectsNonUTF8(t *testing.T) {
	// GBK 编码的“张三”不是合法 UTF-8；必须失败关闭，不能按乱码姓名导入。
	if _, err := parseEmployeeCSV([]byte{0xD5, 0xC5, 0xC8, 0xFD, ',', 'a', '\n'}, nil); err == nil {
		t.Fatal("expected non-UTF-8 employee CSV to fail")
	}
}
