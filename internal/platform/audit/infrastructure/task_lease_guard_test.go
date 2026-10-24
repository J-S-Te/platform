package infrastructure

import (
	"os"
	"strings"
	"testing"
	"time"

	"github.com/J-S-Te/Basic-Platform/internal/platform/audit/domain"
	"gorm.io/driver/mysql"
	"gorm.io/gorm"
)

// 审计导出与保留任务在租约过期后会被其他 Worker 接管。完成/失败回写如果只校验
// RUNNING 状态、不校验租约持有者，被接管的旧执行者仍能覆盖新执行者的结果。

func newAuditDryRun(t *testing.T) *gorm.DB {
	t.Helper()
	database, err := gorm.Open(mysql.New(mysql.Config{DSN: "test:test@tcp(localhost:3306)/test?parseTime=true", SkipInitializeWithVersion: true}), &gorm.Config{DisableAutomaticPing: true, DryRun: true, SkipDefaultTransaction: true})
	if err != nil {
		t.Fatalf("open dry-run database: %v", err)
	}
	return database
}

func TestExportJobCompletionIsBoundToWorkerLease(t *testing.T) {
	database := newAuditDryRun(t)
	statement := database.Model(&asyncJobModel{}).
		Where("tenant_id = ? AND public_id = ? AND job_type = ? AND status = ? AND locked_by = ?", "tenant-1", "job-1", "AUDIT_EXPORT", "RUNNING", "worker-1").
		Updates(map[string]any{"status": "SUCCEEDED"}).Statement
	if !strings.Contains(statement.SQL.String(), "locked_by = ?") {
		t.Fatalf("导出完成回写缺少租约校验: %s", statement.SQL.String())
	}
	args := statement.Vars
	if len(args) != 6 || args[5] != "worker-1" {
		t.Fatalf("租约参数缺失或顺序错误: %#v", args)
	}
}

func TestExportWorkCarriesLeaseOwnerFromClaim(t *testing.T) {
	source := readRepositorySource(t, "gorm_repository.go")
	if !strings.Contains(source, "MaxAttempts: job.MaxAttempts, LockedBy: workerID}") {
		t.Error("ClaimExportJob 必须把 worker_id 带入 ExportWork，否则回写无法校验租约")
	}
	if !strings.Contains(source, `"RUNNING", work.LockedBy`) {
		t.Error("导出完成与失败回写必须使用 work.LockedBy")
	}
}

func TestRetentionTaskCarriesLeaseOwnerAndWritesCheckIt(t *testing.T) {
	task := domain.RetentionTask{TaskID: "task-1", Status: domain.RetentionTaskRunning, WorkerID: "worker-9", CutoffAt: time.Now().UTC()}
	if task.WorkerID != "worker-9" {
		t.Fatal("RetentionTask 必须携带 WorkerID")
	}
	source := readRepositorySource(t, "gorm_enhancement_repository.go")
	for _, fragment := range []string{
		"WorkerID: model.WorkerID",
		`"task_id = ? AND status = ? AND worker_id = ?", task.TaskID, domain.RetentionTaskRunning, task.WorkerID`,
	} {
		if !strings.Contains(source, fragment) {
			t.Errorf("保留任务回写缺少租约校验片段 %q", fragment)
		}
	}
	sql := newAuditDryRun(t).Model(&retentionTaskModel{}).
		Where("task_id = ? AND status = ? AND worker_id = ?", task.TaskID, domain.RetentionTaskRunning, task.WorkerID).
		Updates(map[string]any{"status": domain.RetentionTaskCompleted}).Statement.SQL.String()
	if !strings.Contains(sql, "worker_id = ?") {
		t.Fatalf("保留任务完成回写缺少租约校验: %s", sql)
	}
}

func readRepositorySource(t *testing.T, name string) string {
	t.Helper()
	content, err := os.ReadFile(name)
	if err != nil {
		t.Fatalf("read %s: %v", name, err)
	}
	return string(content)
}
