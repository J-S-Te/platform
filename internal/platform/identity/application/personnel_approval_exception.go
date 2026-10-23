package application

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// 超管例外审批审计 outbox 的投递状态。审批状态与 outbox 行在同一数据库事务提交，
// 外部审计端由 dispatcher 异步投递并按稳定事件 ID 幂等重试。
const (
	ApprovalExceptionOutboxPending   = "PENDING"
	ApprovalExceptionOutboxDelivered = "DELIVERED"
)

// 重试退避参数：审计端短时不可用时避免高频重试，同时保证最终可以补投。
const (
	approvalExceptionRetryBase = 5 * time.Second
	approvalExceptionRetryCap  = 10 * time.Minute
	approvalExceptionBatchSize = 50
	approvalExceptionBatchMax  = 200
)

// ApprovalExceptionOutboxEntry 是本地 outbox 中一条超管例外审批审计事实及其投递进度。
type ApprovalExceptionOutboxEntry struct {
	Event    PersonnelChangeAuditEvent
	Attempts int
}

// PersonnelChangeApprovalExceptionWriter 是超管例外审批的原子持久化端口，由 GORM 仓储实现。
// UpdateStatusWithApprovalException 必须在同一数据库事务内推进审批状态、记录状态轨迹并
// 写入审计 outbox；任何一步失败都不得留下已提交的审批状态。
type PersonnelChangeApprovalExceptionWriter interface {
	UpdateStatusWithApprovalException(ctx context.Context, expected PersonnelChangeRequest, status, ref, operator string, now time.Time, exception PersonnelChangeAuditEvent) (PersonnelChangeRequest, error)
}

// PersonnelChangeApprovalExceptionStore 是 dispatcher 需要的完整 outbox 端口：
// 原子写入由 Writer 提供，读取与投递标记用于幂等异步重试。
type PersonnelChangeApprovalExceptionStore interface {
	PersonnelChangeApprovalExceptionWriter
	PendingApprovalExceptions(ctx context.Context, now time.Time, limit int) ([]ApprovalExceptionOutboxEntry, error)
	MarkApprovalExceptionDelivered(ctx context.Context, tenantID, eventID string, now time.Time) error
	MarkApprovalExceptionRetry(ctx context.Context, tenantID, eventID string, attempts int, nextAttemptAt time.Time, lastError string, now time.Time) error
}

// ApprovalExceptionDispatchReport 汇总一次 outbox 投递轮次的结果。
type ApprovalExceptionDispatchReport struct {
	Inspected int `json:"inspected"`
	Delivered int `json:"delivered"`
	Failed    int `json:"failed"`
}

// ApprovalExceptionDispatcher 把本地 outbox 中待投递的超管例外审计事件交给平台审计端。
// 投递失败只更新重试时间，不改变已提交的审批状态；重试复用同一事件 ID，接收端按
// (租户, 应用, 环境, event_id) 幂等去重。
type ApprovalExceptionDispatcher struct {
	store PersonnelChangeApprovalExceptionStore
	sink  PersonnelChangeAuditSink
	clock Clock
}

// NewApprovalExceptionDispatcher 构造幂等投递器；任一依赖为 nil 时返回错误，
// 避免构造出永远无法投递或无法记录进度的 dispatcher。
func NewApprovalExceptionDispatcher(store PersonnelChangeApprovalExceptionStore, sink PersonnelChangeAuditSink, clock Clock) (*ApprovalExceptionDispatcher, error) {
	if store == nil || sink == nil || clock == nil {
		return nil, errors.New("approval exception dispatcher dependencies must not be nil")
	}
	return &ApprovalExceptionDispatcher{store: store, sink: sink, clock: clock}, nil
}

// DispatchOnce 投递一批到期的待投递事件。单条失败不会中断整批，也不会删除 outbox 行；
// 仅当持久化读取或标记本身失败时返回错误，供 worker 记录并下一轮重试。
func (dispatcher *ApprovalExceptionDispatcher) DispatchOnce(ctx context.Context, limit int) (ApprovalExceptionDispatchReport, error) {
	report := ApprovalExceptionDispatchReport{}
	if limit <= 0 {
		limit = approvalExceptionBatchSize
	}
	if limit > approvalExceptionBatchMax {
		limit = approvalExceptionBatchMax
	}
	now := dispatcher.clock.Now().UTC()
	entries, err := dispatcher.store.PendingApprovalExceptions(ctx, now, limit)
	if err != nil {
		return report, fmt.Errorf("list approval exception outbox: %w", err)
	}
	var failures []error
	for _, entry := range entries {
		report.Inspected++
		if sinkErr := dispatcher.sink.RecordApprovalException(ctx, entry.Event); sinkErr != nil {
			report.Failed++
			attempts := entry.Attempts + 1
			nextAttemptAt := now.Add(approvalExceptionRetryBackoff(attempts))
			if markErr := dispatcher.store.MarkApprovalExceptionRetry(ctx, entry.Event.TenantID, entry.Event.EventID, attempts, nextAttemptAt, sinkErr.Error(), now); markErr != nil {
				failures = append(failures, fmt.Errorf("schedule approval exception retry %s: %w", entry.Event.EventID, markErr))
			}
			continue
		}
		if markErr := dispatcher.store.MarkApprovalExceptionDelivered(ctx, entry.Event.TenantID, entry.Event.EventID, now); markErr != nil {
			// 审计端已接收但本地未标记：事件保持待投递，下一轮用同一 event_id 重投，
			// 审计接收端按事件 ID 去重，不会产生重复审计事实。
			report.Failed++
			failures = append(failures, fmt.Errorf("mark approval exception delivered %s: %w", entry.Event.EventID, markErr))
			continue
		}
		report.Delivered++
	}
	return report, errors.Join(failures...)
}

// approvalExceptionRetryBackoff 返回下一次重试等待时长：5 秒起步指数退避，上限 10 分钟。
func approvalExceptionRetryBackoff(attempt int) time.Duration {
	if attempt <= 0 {
		return approvalExceptionRetryBase
	}
	backoff := approvalExceptionRetryBase
	for i := 0; i < attempt && backoff < approvalExceptionRetryCap; i++ {
		backoff *= 2
	}
	if backoff > approvalExceptionRetryCap {
		return approvalExceptionRetryCap
	}
	return backoff
}
