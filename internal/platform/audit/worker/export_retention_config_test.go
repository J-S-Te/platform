package worker

import (
	"context"
	"testing"
	"time"
)

// 保留期从环境变量读取，fail-closed：未设置/空/非整数/<=0 一律回落默认 30 天，
// 超过硬上限 3650 天按上限截断，避免误配把审计证据永久留在磁盘上。
func TestParseExportRetention(t *testing.T) {
	for _, testCase := range []struct {
		name     string
		raw      string
		wantDays int
	}{
		{name: "unset falls back to default", raw: "", wantDays: 30},
		{name: "blank falls back to default", raw: "   ", wantDays: 30},
		{name: "valid value is honored", raw: "7", wantDays: 7},
		{name: "padded valid value is honored", raw: " 90 ", wantDays: 90},
		{name: "zero falls back to default", raw: "0", wantDays: 30},
		{name: "negative falls back to default", raw: "-30", wantDays: 30},
		{name: "non integer falls back to default", raw: "30d", wantDays: 30},
		{name: "float falls back to default", raw: "1.5", wantDays: 30},
		{name: "above hard cap clamps to cap", raw: "4000", wantDays: 3650},
		{name: "hard cap boundary is preserved", raw: "3650", wantDays: 3650},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			days, period := ParseExportRetention(testCase.raw)
			if days != testCase.wantDays {
				t.Fatalf("days = %d, want %d", days, testCase.wantDays)
			}
			if want := time.Duration(testCase.wantDays) * 24 * time.Hour; period != want {
				t.Fatalf("period = %v, want %v", period, want)
			}
		})
	}
}

// 环境变量读取必须使用 AUDIT_EXPORT_RETENTION_DAYS 这一唯一契约名。
func TestExportRetentionFromEnvReadsRetentionDays(t *testing.T) {
	if ExportRetentionDaysEnv != "AUDIT_EXPORT_RETENTION_DAYS" {
		t.Fatalf("ExportRetentionDaysEnv = %q, want AUDIT_EXPORT_RETENTION_DAYS", ExportRetentionDaysEnv)
	}
	t.Setenv(ExportRetentionDaysEnv, "14")
	days, period := ExportRetentionFromEnv()
	if days != 14 || period != 14*24*time.Hour {
		t.Fatalf("days = %d, period = %v, want 14 days", days, period)
	}
	t.Setenv(ExportRetentionDaysEnv, "0")
	if days, period := ExportRetentionFromEnv(); days != 30 || period != ExportRetentionPeriod {
		t.Fatalf("days = %d, period = %v, want default 30 days", days, period)
	}
}

// 清理窗口必须使用注入的保留期，而不是写死的 30 天。
func TestCleanupWindowUsesConfiguredRetention(t *testing.T) {
	repository := &exportRepositoryStub{}
	worker := newExportTestWorker(t, repository, t.TempDir())
	worker.retention = 72 * time.Hour

	before := time.Now().UTC()
	worker.cleanupExpiredExports(context.Background())
	after := time.Now().UTC()

	if len(repository.listedBefore) != 1 {
		t.Fatalf("cleanup list calls = %d, want 1", len(repository.listedBefore))
	}
	cutoff := repository.listedBefore[0]
	earliest := before.Add(-72 * time.Hour)
	latest := after.Add(-72 * time.Hour)
	if cutoff.Before(earliest) || cutoff.After(latest) {
		t.Fatalf("cleanup cutoff = %v, want between %v and %v (72h retention)", cutoff, earliest, latest)
	}
}

// newExportWorkerForConfigTest 用真实构造函数装配 worker，验证保留期注入链路。
func newExportWorkerForConfigTest(t *testing.T, retention time.Duration) *ExportWorker {
	t.Helper()
	worker, err := NewExportWorker(newExportTestService(t, &exportRepositoryStub{}), newDiscardLogger(), t.TempDir(), "worker-1", time.Second, time.Minute, retention)
	if err != nil {
		t.Fatalf("new export worker: %v", err)
	}
	return worker
}

// 构造函数必须把装配侧传入的保留期落到 worker 上，零值仍回落默认。
func TestNewExportWorkerAppliesRetention(t *testing.T) {
	worker := newExportWorkerForConfigTest(t, 10*24*time.Hour)
	if got := worker.retentionPeriod(); got != 10*24*time.Hour {
		t.Fatalf("retentionPeriod = %v, want 10 days", got)
	}
	worker = newExportWorkerForConfigTest(t, 0)
	if got := worker.retentionPeriod(); got != ExportRetentionPeriod {
		t.Fatalf("retentionPeriod = %v, want default %v", got, ExportRetentionPeriod)
	}
}
