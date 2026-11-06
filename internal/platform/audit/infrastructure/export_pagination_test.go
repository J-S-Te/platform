package infrastructure

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/J-S-Te/Basic-Platform/internal/platform/audit/domain"
	"gorm.io/gorm"
)

// 分页导出的 SQL 契约：首屏无游标条件，后续批次用 (occurred_at, id) 严格小于游标，
// 排序稳定，LIMIT 取批量+1 以判断是否还有更多。
func TestExportEventQueryUsesStableCursorPagination(t *testing.T) {
	first := exportEventQueryStatement(t, domain.ExportCursor{}, 1000)
	if !strings.Contains(first.SQL.String(), "ORDER BY audit_event.occurred_at DESC, audit_event.id DESC") {
		t.Fatalf("export query is not stably ordered: %s", first.SQL.String())
	}
	if strings.Contains(first.SQL.String(), "audit_event.id < ?") {
		t.Fatalf("first export batch must not carry a cursor predicate: %s", first.SQL.String())
	}
	if !strings.Contains(first.SQL.String(), "LIMIT ?") {
		t.Fatalf("export query is not paged: %s", first.SQL.String())
	}

	cursor := domain.ExportCursor{OccurredAt: time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC), ID: 42}
	next := exportEventQueryStatement(t, cursor, 1000)
	sql := next.SQL.String()
	if !strings.Contains(sql, "(audit_event.occurred_at < ? OR (audit_event.occurred_at = ? AND audit_event.id < ?))") {
		t.Fatalf("cursor predicate missing from export query: %s", sql)
	}
	args := fmt.Sprint(next.Vars)
	for _, want := range []string{"2026-01-02 03:04:05", "42", "1001"} {
		if !strings.Contains(args, want) {
			t.Fatalf("cursor query args %v missing %q", next.Vars, want)
		}
	}
	// LIMIT 是探测行上限：批量 1000 时取 1001 行，返回时再截断。
	if !strings.Contains(args, "1001") {
		t.Fatalf("export query must request one probe row beyond the batch: %v", next.Vars)
	}
}

func exportEventQueryStatement(t *testing.T, cursor domain.ExportCursor, limit int) *gorm.Statement {
	t.Helper()
	repository := &Repository{database: newAuditQueryDryRun(t)}
	return repository.exportEventQuery(contextBackground(), "tenant-1", domain.ExportQuery{}, cursor, limit).Find(&[]eventQueryRow{}).Statement
}

func TestExportEventPageTrimsProbeRowAndCarriesCursor(t *testing.T) {
	base := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	rows := []eventQueryRow{
		{Event: eventModel{ID: 30, EventID: "evt-30", OccurredAt: base}},
		{Event: eventModel{ID: 20, EventID: "evt-20", OccurredAt: base}},
		{Event: eventModel{ID: 10, EventID: "evt-10", OccurredAt: base.Add(-time.Second)}},
	}
	page := exportEventPage(rows, 2)
	if len(page.Events) != 2 || page.Events[0].EventID != "evt-30" || page.Events[1].EventID != "evt-20" {
		t.Fatalf("page events = %+v, want evt-30, evt-20", page.Events)
	}
	if page.NextCursor == nil || page.NextCursor.ID != 20 || !page.NextCursor.OccurredAt.Equal(base) {
		t.Fatalf("next cursor = %+v, want (base, 20)", page.NextCursor)
	}

	// 恰好一批（没有探测行）说明已读完：游标必须为空，调用方据此终止。
	last := exportEventPage(rows[:2], 2)
	if len(last.Events) != 2 || last.NextCursor != nil {
		t.Fatalf("last page = %+v cursor=%+v, want two events and nil cursor", last.Events, last.NextCursor)
	}

	empty := exportEventPage(nil, 2)
	if len(empty.Events) != 0 || empty.NextCursor != nil {
		t.Fatalf("empty page = %+v", empty)
	}
}

// 保留期清理选择必须限定：已完成、超过截止时间、job_type=AUDIT_EXPORT，且版本尚未删除；
// 因此未过期任务与其他 job_type 的文件不会被选中。
func TestExpiredExportArtifactSelectionIsScopedToAuditExports(t *testing.T) {
	cutoff := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	repository := &Repository{database: newAuditQueryDryRun(t)}
	statement := repository.expiredExportArtifactQuery(cutoff, 50).Find(&[]expiredExportArtifactRow{}).Statement
	sql := statement.SQL.String()

	for _, want := range []string{
		"async_job.job_type = ?",
		"async_job.status = ?",
		"async_job.completed_at IS NOT NULL",
		"async_job.completed_at < ?",
		"file_version.status NOT IN",
		"JOIN file_object ON file_object.id = async_job.result_file_id",
		"JOIN file_version ON file_version.id = file_object.current_version_id",
		"ORDER BY async_job.completed_at ASC, async_job.id ASC",
		"LIMIT ?",
	} {
		if !strings.Contains(sql, want) {
			t.Fatalf("selection query missing %q:\n%s", want, sql)
		}
	}
	args := fmt.Sprint(statement.Vars)
	for _, want := range []string{"AUDIT_EXPORT", "SUCCEEDED", "DELETED", "REMOVED", "2026-01-01", "50"} {
		if !strings.Contains(args, want) {
			t.Fatalf("selection args %v missing %q", statement.Vars, want)
		}
	}
}

// 标记删除只能作用于传入的版本 ID，并且跳过已处于删除态的版本；不得级联改动
// file_object/async_job，也不得按路径前缀或 job_type 批量误删。
func TestMarkExportArtifactsDeletedOnlyUpdatesSelectedVersions(t *testing.T) {
	repository := &Repository{database: newAuditQueryDryRun(t)}
	statement := repository.exportArtifactDeleteQuery([]string{"version-1", "version-2"}).Update("status", "DELETED").Statement
	sql := statement.SQL.String()
	if !strings.HasPrefix(sql, "UPDATE `file_version`") {
		t.Fatalf("mark query does not update file_version: %s", sql)
	}
	if !strings.Contains(sql, "id IN (?,?)") || !strings.Contains(sql, "status NOT IN (?,?)") {
		t.Fatalf("mark query is not scoped to selected active versions: %s", sql)
	}
	if strings.Contains(sql, "file_object") || strings.Contains(sql, "async_job") {
		t.Fatalf("mark query must not touch task or file object rows: %s", sql)
	}
	args := fmt.Sprint(statement.Vars)
	for _, want := range []string{"version-1", "version-2", "DELETED", "REMOVED"} {
		if !strings.Contains(args, want) {
			t.Fatalf("mark args %v missing %q", statement.Vars, want)
		}
	}
}
