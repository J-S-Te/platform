package applicationaccess

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"
)

type failingAuditRecorder struct{}

func (failingAuditRecorder) RecordApplicationAccessAudit(context.Context, AuditEvent) error {
	return errors.New("audit ingest unavailable")
}

// 安全审查 SEC-D4a：业务审计摄取失败不允许被空白标识符（_ = ...）静默丢弃，
// 必须输出带可告警字段（tenant/action/resource/result）的结构化 error 日志。
func TestRecordAuditLogsErrorInsteadOfSilentDiscard(t *testing.T) {
	var buffer bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buffer, nil)))
	defer slog.SetDefault(previous)

	service := &Service{audit: failingAuditRecorder{}}
	service.recordAudit(context.Background(), AuditEvent{
		TenantID:        "tenant-1",
		ApplicationCode: "contract_management",
		OperatorID:      "user-1",
		Action:          "PUT_ACCESS",
		ResourceID:      "subject-1",
		Result:          "SUCCESS",
		OccurredAt:      time.Now().UTC(),
	})

	output := buffer.String()
	for _, field := range []string{
		"record application access audit",
		"tenant_id=tenant-1",
		"application_code=contract_management",
		"action=PUT_ACCESS",
		"resource_id=subject-1",
		"audit ingest unavailable",
	} {
		if !strings.Contains(output, field) {
			t.Fatalf("log output = %q, want field %q", output, field)
		}
	}
}

// 未安装业务审计管道的进程保持无操作（既有可选契约不回归）。
func TestRecordAuditWithoutRecorderIsNoop(t *testing.T) {
	(&Service{}).recordAudit(context.Background(), AuditEvent{OccurredAt: time.Now().UTC()})
}
