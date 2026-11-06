package worker

import (
	"context"
	"io"
	"log/slog"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/J-S-Te/Basic-Platform/internal/platform/audit/application"
	"github.com/J-S-Te/Basic-Platform/internal/platform/audit/domain"
)

// exportRepositoryStub 是审计导出 worker 测试用的内存仓储替身：只需实现分页读取、
// 任务完成/失败与保留期清理，其余 Repository 方法保持失败关闭的空实现。
// 分页读取按 (occurred_at DESC, id DESC) 的稳定顺序模拟游标语义，因此测试能验证
// worker 是否自己造成重复或遗漏。
type exportRepositoryStub struct {
	mu sync.Mutex
	// events 必须已按 occurred_at DESC, id DESC 排序。
	events  []domain.Event
	listErr error
	// failAtCall 在指定批次（从 1 开始）返回 listErr，用于模拟中途读取失败。
	failAtCall int
	listCalls  []domain.ExportCursor
	listLimits []int

	work     domain.ExportWork
	hasWork  bool
	claimErr error

	completed    *domain.ExportFile
	completedFor *domain.ExportWork
	completeErr  error
	completedAt  *time.Time

	failedCode    string
	failedMessage string

	artifacts     []domain.ExportArtifact
	artifactErr   error
	listedBefore  []time.Time
	listedLimits  []int
	marked        []string
	markErr       error
	markCallCount int
}

func (stub *exportRepositoryStub) ListExportEvents(_ context.Context, _ string, _ domain.ExportQuery, cursor domain.ExportCursor, limit int) (domain.ExportEventPage, error) {
	stub.mu.Lock()
	defer stub.mu.Unlock()
	stub.listCalls = append(stub.listCalls, cursor)
	stub.listLimits = append(stub.listLimits, limit)
	if stub.listErr != nil && (stub.failAtCall == 0 || len(stub.listCalls) >= stub.failAtCall) {
		return domain.ExportEventPage{}, stub.listErr
	}
	start := 0
	if !cursor.OccurredAt.IsZero() || cursor.ID != 0 {
		for start < len(stub.events) && !exportAfterCursor(stub.events[start], cursor) {
			start++
		}
	}
	if start >= len(stub.events) {
		return domain.ExportEventPage{}, nil
	}
	end := start + limit
	hasMore := end < len(stub.events)
	if !hasMore {
		end = len(stub.events)
	}
	page := domain.ExportEventPage{Events: append([]domain.Event(nil), stub.events[start:end]...)}
	if hasMore {
		last := page.Events[len(page.Events)-1]
		id, _ := strconv.ParseUint(last.ID, 10, 64)
		page.NextCursor = &domain.ExportCursor{OccurredAt: last.OccurredAt, ID: id}
	}
	return page, nil
}

// exportAfterCursor 判断事件是否严格排在游标之后（同一 occurred_at 下 id 更小），
// 与仓储 (occurred_at DESC, id DESC) 的游标谓词一致。
func exportAfterCursor(event domain.Event, cursor domain.ExportCursor) bool {
	if event.OccurredAt.Before(cursor.OccurredAt) {
		return true
	}
	if !event.OccurredAt.Equal(cursor.OccurredAt) {
		return false
	}
	id, _ := strconv.ParseUint(event.ID, 10, 64)
	return id < cursor.ID
}

func (stub *exportRepositoryStub) ClaimExportJob(context.Context, string, time.Time, time.Time) (domain.ExportWork, bool, error) {
	stub.mu.Lock()
	defer stub.mu.Unlock()
	if stub.claimErr != nil {
		return domain.ExportWork{}, false, stub.claimErr
	}
	if !stub.hasWork {
		return domain.ExportWork{}, false, nil
	}
	stub.hasWork = false
	return stub.work, true, nil
}

func (stub *exportRepositoryStub) CompleteExportJob(_ context.Context, work domain.ExportWork, file domain.ExportFile, completedAt time.Time) error {
	stub.mu.Lock()
	defer stub.mu.Unlock()
	if stub.completeErr != nil {
		return stub.completeErr
	}
	copiedWork, copiedFile := work, file
	stub.completedFor, stub.completed = &copiedWork, &copiedFile
	stub.completedAt = &completedAt
	return nil
}

func (stub *exportRepositoryStub) FailExportJob(_ context.Context, _ domain.ExportWork, code, message string, _, _ time.Time) error {
	stub.mu.Lock()
	defer stub.mu.Unlock()
	stub.failedCode, stub.failedMessage = code, message
	return nil
}

func (stub *exportRepositoryStub) ListExpiredExportArtifacts(_ context.Context, completedBefore time.Time, limit int) ([]domain.ExportArtifact, error) {
	stub.mu.Lock()
	defer stub.mu.Unlock()
	stub.listedBefore = append(stub.listedBefore, completedBefore)
	stub.listedLimits = append(stub.listedLimits, limit)
	if stub.artifactErr != nil {
		return nil, stub.artifactErr
	}
	artifacts := stub.artifacts
	stub.artifacts = nil // 单轮只返回一批，模拟清理后不再被选中。
	return artifacts, nil
}

func (stub *exportRepositoryStub) MarkExportArtifactsDeleted(_ context.Context, fileVersionIDs []string) error {
	stub.mu.Lock()
	defer stub.mu.Unlock()
	stub.markCallCount++
	if stub.markErr != nil {
		return stub.markErr
	}
	stub.marked = append(stub.marked, fileVersionIDs...)
	return nil
}

func (stub *exportRepositoryStub) Ingest(context.Context, string, application.EventInput, time.Time) (domain.Receipt, error) {
	return domain.Receipt{}, nil
}
func (stub *exportRepositoryStub) IngestBatch(context.Context, string, application.BatchDeliveryInput, []application.EventInput, time.Time) ([]domain.Receipt, error) {
	return nil, nil
}
func (stub *exportRepositoryStub) List(context.Context, string, application.PageRequest) (application.PageResult[domain.Event], error) {
	return application.PageResult[domain.Event]{}, nil
}
func (stub *exportRepositoryStub) Get(context.Context, string, string) (domain.Event, error) {
	return domain.Event{}, nil
}
func (stub *exportRepositoryStub) IngestionHealth(context.Context, string, time.Time) (application.IngestionHealth, error) {
	return application.IngestionHealth{}, nil
}
func (stub *exportRepositoryStub) CreateExportJob(context.Context, string, string, application.PageRequest, string, time.Time) (domain.ExportJob, error) {
	return domain.ExportJob{}, nil
}
func (stub *exportRepositoryStub) GetExportJob(context.Context, string, string) (domain.ExportJob, error) {
	return domain.ExportJob{}, nil
}
func (stub *exportRepositoryStub) GetExportFile(context.Context, string, string) (domain.ExportFile, error) {
	return domain.ExportFile{}, application.ErrNotFound
}
func (stub *exportRepositoryStub) VerifyPayloadHash(context.Context, string, string, []byte) (domain.PayloadHashVerification, error) {
	return domain.PayloadHashVerification{}, nil
}
func (stub *exportRepositoryStub) VerifyEventChain(context.Context, string, string, string, uint64, int) (domain.ChainVerification, error) {
	return domain.ChainVerification{}, nil
}

// newDiscardLogger 让 worker 的告警/错误日志不污染测试输出。
func newDiscardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))
}

// exportTestIDs 与 exportTestClock 让服务构造不依赖真实 ULID/时间源。
type exportTestIDs struct{}

func (exportTestIDs) New(time.Time) (string, error) { return "01J00000000000000000000TST", nil }

type exportTestClock struct{}

func (exportTestClock) Now() time.Time { return time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC) }

func newExportTestService(t *testing.T, repository application.Repository) *application.Service {
	t.Helper()
	service, err := application.NewService(repository, exportTestIDs{}, exportTestClock{})
	if err != nil {
		t.Fatalf("new audit service: %v", err)
	}
	return service
}

// sortedExportEvents 生成按 occurred_at DESC, id DESC 排好序的事件：每 3 条共享同一个
// occurred_at，覆盖“同一时间戳靠 id 去重”的游标边界。
func sortedExportEvents(total int) []domain.Event {
	base := time.Date(2026, 1, 2, 3, 0, 0, 0, time.UTC)
	events := make([]domain.Event, 0, total)
	for index := 0; index < total; index++ {
		id := total - index
		events = append(events, domain.Event{
			ID:              strconv.Itoa(id),
			EventID:         "evt-" + strconv.Itoa(id),
			OccurredAt:      base.Add(-time.Duration(index/3) * time.Minute),
			ApplicationCode: "platform",
			EnvironmentCode: "prod",
			Action:          "GET /api/v1/audit/events",
			Result:          "SUCCESS",
			ResourceType:    "AUDIT",
			RiskLevel:       "LOW",
		})
	}
	return events
}

func newExportTestWorker(t *testing.T, repository application.Repository, storageRoot string) *ExportWorker {
	t.Helper()
	return &ExportWorker{
		service:          newExportTestService(t, repository),
		logger:           newDiscardLogger(),
		storageRoot:      storageRoot,
		workerID:         "worker-1",
		pollInterval:     time.Second,
		staleLockTimeout: time.Minute,
	}
}
