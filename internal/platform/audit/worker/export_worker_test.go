package worker

import (
	"encoding/csv"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/J-S-Te/Basic-Platform/internal/platform/audit/domain"
)

// 安全审查 SEC-B6：五类公式注入载荷（=、+、-、@、前导 tab/CR）必须加单引号前缀，
// 普通值保持原样。
func TestEscapeCSVFormulaCellNeutralizesFivePayloadClasses(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  string
	}{
		{name: "等号公式", input: "=cmd|' /C calc'!A0", want: "'=cmd|' /C calc'!A0"},
		{name: "加号公式", input: "+1+1", want: "'+1+1"},
		{name: "减号公式", input: "-2+3", want: "'-2+3"},
		{name: "AT符号公式", input: "@SUM(A1)", want: "'@SUM(A1)"},
		{name: "前导tab公式", input: "	=1+1", want: "'	=1+1"},
		{name: "前导CR公式", input: "=1+1", want: "'=1+1"},
		{name: "普通文本不动", input: "张三", want: "张三"},
		{name: "空串不动", input: "", want: ""},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := escapeCSVFormulaCell(test.input); got != test.want {
				t.Fatalf("escapeCSVFormulaCell(%q) = %q, want %q", test.input, got, test.want)
			}
		})
	}
}

// 端到端：writeCSV 落盘的行中，攻击者可控的 User-Agent/资源名载荷必须已被转义，
// 且普通行内容不被改写。
func TestWriteCSVEscapesAttackerControlledCells(t *testing.T) {
	storageRoot := t.TempDir()
	worker := &ExportWorker{storageRoot: storageRoot}
	hostile := domain.Event{
		EventID:             "evt-1",
		OccurredAt:          time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC),
		ApplicationCode:     "platform",
		EnvironmentCode:     "prod",
		Action:              "GET /api/v1/audit/events",
		Result:              "SUCCESS",
		ResourceType:        "AUDIT",
		ResourceID:          "evt-1",
		ResourceName:        `=HYPERLINK("http://evil.example")`,
		OperatorDisplayName: "审计员",
		RiskLevel:           "LOW",
		Summary:             "+86遥测",
		ClientIP:            "203.0.113.9",
		UserAgent:           "@SUM(1+1)*cmd",
		Path:                "/api/v1/audit/events",
	}
	normal := domain.Event{
		EventID:             "evt-2",
		OccurredAt:          time.Date(2026, 1, 2, 3, 4, 6, 0, time.UTC),
		ApplicationCode:     "platform",
		EnvironmentCode:     "prod",
		Action:              "POST /api/v1/users",
		Result:              "SUCCESS",
		ResourceType:        "IDENTITY",
		ResourceName:        "普通资源名",
		OperatorDisplayName: "李四",
		RiskLevel:           "MEDIUM",
		ClientIP:            "198.51.100.7",
		UserAgent:           "Mozilla/5.0",
	}
	_, absolutePath, err := worker.writeCSV(domain.ExportWork{TenantID: "tenant-1", ApplicationID: "app-1", JobID: "job-1"}, []domain.Event{hostile, normal})
	if err != nil {
		t.Fatalf("writeCSV() error = %v", err)
	}
	raw, err := os.ReadFile(filepath.Clean(absolutePath))
	if err != nil {
		t.Fatalf("read export: %v", err)
	}
	content := string(raw)
	for _, escaped := range []string{
		"'=HYPERLINK(",
		"'+86遥测",
		"'@SUM(1+1)*cmd",
		"普通资源名",
		"Mozilla/5.0",
	} {
		if !strings.Contains(content, escaped) {
			t.Fatalf("export missing %q:\n%s", escaped, content)
		}
	}
	// 用 csv 解析器确认行数与字段数完整（转义不破坏结构）。
	records, err := csv.NewReader(strings.NewReader(content)).ReadAll()
	if err != nil {
		t.Fatalf("parse export csv: %v", err)
	}
	if len(records) != 3 {
		t.Fatalf("rows = %d, want header + 2 data rows", len(records))
	}
}
