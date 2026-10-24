package application

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"
)

// HandoverSystemClient 是业务系统机器协议的客户端端口；4c 的四个业务仓库实现服务端。
type HandoverSystemClient interface {
	Freeze(context.Context, HandoverFreezeRequest) (HandoverFreezeResponse, error)
	Responsibilities(context.Context, string, string, int) (HandoverResponsibilityPage, error)
	Transfer(context.Context, HandoverTransferRequest) (HandoverTransferReceipt, error)
	Receipt(context.Context, string, string) (HandoverTransferReceipt, error)
	Finish(context.Context, string, string, string) error
}

// HandoverSystemInventory 提供控制面的系统适用性事实（应用注册/安装与授权状态）。
type HandoverSystemInventory interface {
	Systems(context.Context, string) ([]HandoverSystemState, error)
}

// HandoverOrchestrationRepository 是编排所需的最小持久化端口（由 GORM 仓储实现）。
type HandoverOrchestrationRepository interface {
	EnsureBatch(context.Context, string, string, string, time.Time) (HandoverBatch, error)
	GetBatch(context.Context, string, string) (HandoverBatch, error)
	GetBatchByID(context.Context, string, string) (HandoverBatch, error)
	UpsertSystemState(context.Context, string, string, string, string, string, time.Time) error
	MarkFrozen(context.Context, string, string, string, time.Time, int) error
	InsertResponsibilityItems(context.Context, string, string, string, string, string, []HandoverResponsibility, time.Time) (int, error)
	AssignTarget(context.Context, string, string, string, time.Time) error
	ClaimNextItem(context.Context, string, string, string, time.Time, time.Time) (HandoverItem, bool, error)
	RecordReceipt(context.Context, string, string, uint64, HandoverTransferReceipt, time.Time) (bool, error)
	CompleteBatch(context.Context, string, string, string, time.Time) error
	RequestCancellation(context.Context, string, string, time.Time) error
	ListSystemStates(context.Context, string, string) ([]HandoverSystemState, error)
	ListItems(context.Context, string, string) ([]HandoverItem, error)
	// ListStaleBatches 列出超过截止时间仍未推进的批次，供重新发现使用。
	ListStaleBatches(context.Context, string, time.Time, int) ([]HandoverBatch, error)
}

// HandoverOrchestrator 驱动"冻结 → 发现 → 逐项转移 → 完成/取消"的跨系统交接。
// 它只依据真实回执推进状态：没有回执就不标完成，取消也不反向修改已完成的转移。
type HandoverOrchestrator struct {
	repository       HandoverOrchestrationRepository
	inventory        HandoverSystemInventory
	clients          map[string]HandoverSystemClient
	ids              IDGenerator
	clock            Clock
	completionMu     sync.Mutex
	completionCursor string
}

// HandoverPendingCompletionRepository 只查询已执行离职关联的待完成批次，按 ID 游标有界读取。
type HandoverPendingCompletionRepository interface {
	ListPendingCompletionBatches(context.Context, string, int) ([]HandoverBatch, error)
}

// CompleteExecuted 在 worker 每轮补偿少量已执行批次。失败也推进游标，避免一个坏批次
// 饿死后续任务；到末页后下一轮重新开始。游标只是公平性优化，重启不丢持久待办。
func (orchestrator *HandoverOrchestrator) CompleteExecuted(ctx context.Context, limit int) (int, error) {
	if limit <= 0 || limit > 100 {
		return 0, ErrValidation
	}
	repository, ok := orchestrator.repository.(HandoverPendingCompletionRepository)
	if !ok {
		return 0, fmt.Errorf("handover completion queue is unavailable: %w", ErrConflict)
	}
	orchestrator.completionMu.Lock()
	defer orchestrator.completionMu.Unlock()
	batches, err := repository.ListPendingCompletionBatches(ctx, orchestrator.completionCursor, limit)
	if err != nil {
		return 0, err
	}
	if len(batches) == 0 {
		orchestrator.completionCursor = ""
		return 0, nil
	}
	completed := 0
	var failures []error
	for _, batch := range batches {
		if err := ctx.Err(); err != nil {
			return completed, errors.Join(append(failures, err)...)
		}
		orchestrator.completionCursor = batch.ID
		if err := orchestrator.Complete(ctx, batch.TenantID, batch.ID); err != nil {
			failures = append(failures, fmt.Errorf("complete handover batch %s: %w", batch.ID, err))
			continue
		}
		completed++
	}
	return completed, errors.Join(failures...)
}

const (
	handoverPageSize    = 100
	handoverLeaseWindow = 2 * time.Minute
	handoverFinishDone  = "COMPLETED"
	handoverFinishAbort = "CANCELLED"
)

// NewHandoverOrchestrator 构造交接编排器。核心依赖与任一系统客户端为 nil 时返回错误；
// 客户端映射会复制为独立副本，调用方后续改动不影响编排器使用的协议端口集合。
func NewHandoverOrchestrator(repository HandoverOrchestrationRepository, inventory HandoverSystemInventory, clients map[string]HandoverSystemClient, ids IDGenerator, clock Clock) (*HandoverOrchestrator, error) {
	if repository == nil || inventory == nil || ids == nil || clock == nil {
		return nil, errors.New("handover orchestrator dependencies must not be nil")
	}
	copied := make(map[string]HandoverSystemClient, len(clients))
	for system, client := range clients {
		if client == nil {
			return nil, fmt.Errorf("handover client for %s must not be nil", system)
		}
		copied[system] = client
	}
	return &HandoverOrchestrator{repository: repository, inventory: inventory, clients: copied, ids: ids, clock: clock}, nil
}

// Start 创建批次、逐系统冻结并发现权威责任；未安装系统标为不适用，阻断系统立即失败。
func (orchestrator *HandoverOrchestrator) Start(ctx context.Context, tenantID, requestID, userID string) (HandoverBatch, error) {
	now := orchestrator.clock.Now().UTC()
	batch, err := orchestrator.repository.EnsureBatch(ctx, tenantID, requestID, userID, now)
	if err != nil {
		return HandoverBatch{}, err
	}
	if batch.Status == HandoverBatchCompleted || batch.Status == HandoverBatchCancelled {
		return batch, nil
	}
	if batch.CancelRequestedAt != nil {
		return batch, ErrConflict
	}
	// FROZEN 是完整发现的检查点；任何中途失败都必须留在 DISCOVERING。
	if err := orchestrator.repository.CompleteBatch(ctx, tenantID, batch.ID, HandoverBatchDiscovering, now); err != nil {
		return batch, err
	}
	systems, err := orchestrator.inventory.Systems(ctx, tenantID)
	if err != nil {
		return batch, fmt.Errorf("read handover system inventory: %w", err)
	}
	var blocking []string
	for _, state := range systems {
		blockingSystem, notApplicable := HandoverSystemDisposition(state.Capability)
		reason := ""
		if blockingSystem {
			reason = state.Capability
			blocking = append(blocking, state.System)
		}
		if err := orchestrator.repository.UpsertSystemState(ctx, tenantID, batch.ID, state.System, state.Capability, reason, now); err != nil {
			return batch, err
		}
		if blockingSystem || notApplicable {
			continue
		}
		client, ok := orchestrator.clients[state.System]
		if !ok {
			// 已安装但平台没有该系统的协议客户端：按不可达处理，阻断而不是静默跳过。
			if err := orchestrator.repository.UpsertSystemState(ctx, tenantID, batch.ID, state.System, HandoverSystemUnreachable, "CLIENT_UNAVAILABLE", now); err != nil {
				return batch, err
			}
			blocking = append(blocking, state.System)
			continue
		}
		freeze, freezeErr := client.Freeze(ctx, HandoverFreezeRequest{TenantID: tenantID, BatchID: batch.ID, RequestID: requestID, UserID: userID, RequestedAt: now})
		if freezeErr != nil || !freeze.Frozen {
			if err := orchestrator.repository.UpsertSystemState(ctx, tenantID, batch.ID, state.System, HandoverSystemUnreachable, "FREEZE_FAILED", now); err != nil {
				return batch, err
			}
			blocking = append(blocking, state.System)
			continue
		}
		// 冻结必须先落库，之后才允许读取责任清单。
		if err := orchestrator.repository.MarkFrozen(ctx, tenantID, batch.ID, state.System, now, freeze.ResponsibilityCount); err != nil {
			return batch, err
		}
		discovered, discoverErr := orchestrator.discover(ctx, tenantID, batch, state.System, userID)
		if discoverErr != nil {
			return batch, discoverErr
		}
		if err := orchestrator.repository.MarkFrozen(ctx, tenantID, batch.ID, state.System, now, discovered); err != nil {
			return batch, err
		}
	}
	if len(blocking) > 0 {
		if err := orchestrator.repository.CompleteBatch(ctx, tenantID, batch.ID, HandoverBatchBlocked, now); err != nil {
			return batch, err
		}
		return orchestrator.repository.GetBatch(ctx, tenantID, requestID)
	}
	if err := orchestrator.repository.CompleteBatch(ctx, tenantID, batch.ID, HandoverBatchFrozen, now); err != nil {
		return batch, err
	}
	return orchestrator.repository.GetBatch(ctx, tenantID, requestID)
}

// discover 分页读取权威责任并幂等落库。
func (orchestrator *HandoverOrchestrator) discover(ctx context.Context, tenantID string, batch HandoverBatch, system, userID string) (int, error) {
	cursor, total := "", 0
	for {
		page, err := orchestrator.clients[system].Responsibilities(ctx, batch.RequestID, cursor, handoverPageSize)
		if err != nil {
			return total, fmt.Errorf("discover responsibilities from %s: %w", system, err)
		}
		items := make([]HandoverResponsibility, 0, len(page.Items))
		for _, item := range page.Items {
			item.System = system
			items = append(items, item)
		}
		// 发现阶段只登记权威责任；承接人由管理员随后逐一指定（见 AssignTarget）。
		inserted, err := orchestrator.repository.InsertResponsibilityItems(ctx, tenantID, batch.ID, batch.RequestID, userID, "", items, orchestrator.clock.Now().UTC())
		if err != nil {
			return total, err
		}
		total += inserted
		if strings.TrimSpace(page.NextCursor) == "" {
			return total, nil
		}
		cursor = page.NextCursor
	}
}

// HandoverRediscoveryReport 汇总一次历史批次重新发现的结果。
type HandoverRediscoveryReport struct {
	Inspected  int `json:"inspected"`
	Refreshed  int `json:"refreshed"`
	Blocked    int `json:"blocked"`
	ItemsAdded int `json:"items_added"`
}

// HandoverCompletionVerification 是"本地标记不算数"的权威核对结果：
// 只有全部责任项结清、没有阻断系统且批次仍冻结时，CrossSystemComplete 才为真。
type HandoverCompletionVerification struct {
	BatchFound          bool     `json:"batch_found"`
	BatchID             string   `json:"batch_id,omitempty"`
	BatchStatus         string   `json:"batch_status,omitempty"`
	BlockingSystems     []string `json:"blocking_systems,omitempty"`
	UnresolvedItems     []string `json:"unresolved_items,omitempty"`
	CrossSystemComplete bool     `json:"cross_system_complete"`
}

// RediscoverStaleBatches 重新发现历史离职申请：对滞留批次重新冻结并补发现责任。
// 冻结与落项都是幂等的，因此重复执行安全；发现到阻断系统时批次转为 BLOCKED。
func (orchestrator *HandoverOrchestrator) RediscoverStaleBatches(ctx context.Context, tenantID string, olderThan time.Time, limit int) (HandoverRediscoveryReport, error) {
	report := HandoverRediscoveryReport{}
	if limit <= 0 {
		limit = 50
	}
	batches, err := orchestrator.repository.ListStaleBatches(ctx, tenantID, olderThan.UTC(), limit)
	if err != nil {
		return report, err
	}
	for _, batch := range batches {
		report.Inspected++
		if batch.CancelRequestedAt != nil {
			continue
		}
		before, err := orchestrator.repository.ListItems(ctx, tenantID, batch.RequestID)
		if err != nil {
			return report, err
		}
		refreshed, err := orchestrator.Start(ctx, tenantID, batch.RequestID, batch.UserID)
		if err != nil {
			return report, err
		}
		after, err := orchestrator.repository.ListItems(ctx, tenantID, batch.RequestID)
		if err != nil {
			return report, err
		}
		report.ItemsAdded += len(after) - len(before)
		if refreshed.Status == HandoverBatchBlocked {
			report.Blocked++
			continue
		}
		report.Refreshed++
	}
	return report, nil
}

// 编译期确认编排器具备排期闸门要求的跨系统完成核验能力。
var _ HandoverCompletionVerifier = (*HandoverOrchestrator)(nil)

// VerifyCrossSystemCompletion 核对一次离职是否真的跨系统完成：
// 本地人员异动标记"已执行"不能作为证据，必须所有责任项结清且无阻断系统。
func (orchestrator *HandoverOrchestrator) VerifyCrossSystemCompletion(ctx context.Context, tenantID, requestID string) (HandoverCompletionVerification, error) {
	verification := HandoverCompletionVerification{}
	batch, err := orchestrator.repository.GetBatch(ctx, tenantID, requestID)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return verification, nil
		}
		return verification, err
	}
	verification.BatchFound, verification.BatchID, verification.BatchStatus = true, batch.ID, batch.Status
	if batch.CancelRequestedAt != nil || (batch.Status != HandoverBatchFrozen && batch.Status != HandoverBatchTransferring) {
		return verification, nil
	}
	items, err := orchestrator.repository.ListItems(ctx, tenantID, requestID)
	if err != nil {
		return verification, err
	}
	for _, item := range items {
		// 没有批次的责任项是平台本地人工交接（例如强制的 PERSONNEL_RESPONSIBILITY），
		// 不在跨系统编排范围内；它是否完成由本地 HandoverManager 闸门负责。只有编排
		// 管理的远端责任项才必须凭真实回执达到 TRANSFERRED/SKIPPED。
		if strings.TrimSpace(item.BatchID) == "" {
			continue
		}
		if item.Status != HandoverItemTransferred && item.Status != HandoverItemSkipped {
			verification.UnresolvedItems = append(verification.UnresolvedItems, item.ID)
		}
	}
	systems, err := orchestrator.repository.ListSystemStates(ctx, tenantID, batch.ID)
	if err != nil {
		return verification, err
	}
	for _, state := range systems {
		if blocked, notApplicable := HandoverSystemDisposition(state.Capability); blocked || (!notApplicable && !state.Frozen) {
			verification.BlockingSystems = append(verification.BlockingSystems, state.System)
		}
	}
	verification.CrossSystemComplete = len(verification.UnresolvedItems) == 0 && len(verification.BlockingSystems) == 0 && batch.Status != HandoverBatchBlocked && batch.Status != HandoverBatchCancelled
	return verification, nil
}

// AssignTarget 指定某个责任项的承接人；未指定承接人的项不会被派发。
func (orchestrator *HandoverOrchestrator) AssignTarget(ctx context.Context, tenantID, itemID, targetUserID string) error {
	return orchestrator.repository.AssignTarget(ctx, tenantID, itemID, targetUserID, orchestrator.clock.Now().UTC())
}

// DispatchOnce 派发一批待处理项：领取即持租约；超时或网络错误时先查回执，绝不盲目重发。
func (orchestrator *HandoverOrchestrator) DispatchOnce(ctx context.Context, tenantID, batchID string, maxItems int) (int, error) {
	if maxItems <= 0 {
		return 0, ErrValidation
	}
	batch, err := orchestrator.repository.GetBatchByID(ctx, tenantID, batchID)
	if err != nil {
		return 0, err
	}
	if batch.CancelRequestedAt != nil || (batch.Status != HandoverBatchFrozen && batch.Status != HandoverBatchTransferring) {
		return 0, fmt.Errorf("batch %s is %s and cannot dispatch", batchID, batch.Status)
	}
	dispatched := 0
	for i := 0; i < maxItems; i++ {
		now := orchestrator.clock.Now().UTC()
		leaseID, err := orchestrator.ids.New(now)
		if err != nil {
			return dispatched, err
		}
		item, ok, err := orchestrator.repository.ClaimNextItem(ctx, tenantID, batchID, leaseID, now.Add(handoverLeaseWindow), now)
		if err != nil {
			return dispatched, err
		}
		if !ok {
			return dispatched, nil
		}
		client, ok := orchestrator.clients[item.System]
		if !ok {
			return dispatched, fmt.Errorf("handover client for %s is unavailable", item.System)
		}
		request := HandoverTransferRequest{
			TenantID: tenantID, BatchID: batchID, ItemID: item.ID, RequestID: batch.RequestID, System: item.System,
			ResourceType: item.ResourceType, ResourceID: item.ResourceID,
			FromUserID: item.CurrentOwnerID, ToUserID: item.TargetOwnerID,
			ExpectedVersion: item.ExpectedVersion, IdempotencyKey: item.IdempotencyKey, LeaseID: leaseID,
			Deadline: now.Add(handoverLeaseWindow),
		}
		var receipt HandoverTransferReceipt
		var transferErr error
		// 重启或退避后的领取必须先查旧键的回执，确认没有成功结果再重发。
		if item.Attempts > 1 {
			receipt, transferErr = client.Receipt(ctx, item.ID, item.IdempotencyKey)
			if errors.Is(transferErr, ErrNotFound) {
				receipt, transferErr = client.Transfer(ctx, request)
			} else if transferErr != nil {
				receipt = HandoverTransferReceipt{Status: HandoverReceiptRetryable, ReasonCode: "TRANSFER_RESULT_UNKNOWN"}
			} else if terminal, _ := HandoverReceiptTerminal(receipt.Status); !terminal {
				receipt, transferErr = client.Transfer(ctx, request)
			}
		} else {
			receipt, transferErr = client.Transfer(ctx, request)
		}
		if transferErr != nil {
			// 结果未知：先查回执，再决定是否安排重试。
			receipt, transferErr = client.Receipt(ctx, item.ID, item.IdempotencyKey)
			if transferErr != nil {
				receipt = HandoverTransferReceipt{
					System: item.System, ItemID: item.ID, IdempotencyKey: item.IdempotencyKey,
					Status: HandoverReceiptRetryable, ReasonCode: "TRANSFER_RESULT_UNKNOWN", ReceivedAt: orchestrator.clock.Now().UTC(),
				}
			}
		}
		if receipt.ItemID == "" {
			receipt.ItemID = item.ID
		}
		if receipt.IdempotencyKey == "" {
			receipt.IdempotencyKey = item.IdempotencyKey
		}
		if receipt.System == "" {
			receipt.System = item.System
		}
		if _, err := orchestrator.repository.RecordReceipt(ctx, tenantID, batchID, item.Version, receipt, orchestrator.clock.Now().UTC()); err != nil {
			return dispatched, err
		}
		dispatched++
	}
	return dispatched, nil
}

// Complete 供人员服务在离职已提交后调用；补偿入口通过持久队列筛选已执行请求。
// 只在没有阻断系统且所有责任项都已结清时通知各系统解除冻结并推进批次完成。
func (orchestrator *HandoverOrchestrator) Complete(ctx context.Context, tenantID, batchID string) error {
	batch, err := orchestrator.repository.GetBatchByID(ctx, tenantID, batchID)
	if err != nil {
		return err
	}
	if batch.Status == HandoverBatchCompleted {
		return nil
	}
	verification, err := orchestrator.VerifyCrossSystemCompletion(ctx, tenantID, batch.RequestID)
	if err != nil {
		return err
	}
	if !verification.CrossSystemComplete {
		return fmt.Errorf("handover discovery or transfer is incomplete: %w", ErrConflict)
	}
	items, err := orchestrator.repository.ListItems(ctx, tenantID, batch.RequestID)
	if err != nil {
		return err
	}
	systems, err := orchestrator.repository.ListSystemStates(ctx, tenantID, batchID)
	if err != nil {
		return err
	}
	remoteItems := make([]HandoverItem, 0, len(items))
	for _, item := range items {
		if item.BatchID != "" {
			remoteItems = append(remoteItems, item)
		}
	}
	if !HandoverBatchCompletable(remoteItems, systems) {
		return fmt.Errorf("handover is not complete: unresolved items or blocking systems: %w", ErrConflict)
	}
	now := orchestrator.clock.Now().UTC()
	for _, state := range systems {
		if _, notApplicable := HandoverSystemDisposition(state.Capability); notApplicable {
			continue
		}
		if client, ok := orchestrator.clients[state.System]; ok {
			if err := client.Finish(ctx, batchID, batch.RequestID, handoverFinishDone); err != nil {
				return fmt.Errorf("finish handover on %s: %w", state.System, err)
			}
		} else {
			return fmt.Errorf("handover client for %s is unavailable: %w", state.System, ErrConflict)
		}
	}
	return orchestrator.repository.CompleteBatch(ctx, tenantID, batchID, HandoverBatchCompleted, now)
}

// CancelByRequest 按人员异动请求定位批次后取消；批次不存在时视为无需取消。
func (orchestrator *HandoverOrchestrator) CancelByRequest(ctx context.Context, tenantID, requestID string) error {
	batch, err := orchestrator.repository.GetBatch(ctx, tenantID, requestID)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return nil
		}
		return err
	}
	if batch.Status == HandoverBatchCancelled || batch.Status == HandoverBatchCompleted {
		return nil
	}
	return orchestrator.Cancel(ctx, tenantID, batch.ID)
}

// Cancel 先确认没有在途派发，再逐系统解除冻结；已完成转移保留，不反向改回。
func (orchestrator *HandoverOrchestrator) Cancel(ctx context.Context, tenantID, batchID string) error {
	batch, err := orchestrator.repository.GetBatchByID(ctx, tenantID, batchID)
	if err != nil {
		return err
	}
	if batch.Status == HandoverBatchCompleted || batch.Status == HandoverBatchCancelled {
		return nil
	}
	now := orchestrator.clock.Now().UTC()
	// 先持久化停止派发，重试取消时仍保留此意图，避免查回执期间又领取新项。
	if err := orchestrator.repository.RequestCancellation(ctx, tenantID, batchID, now); err != nil {
		return err
	}
	items, err := orchestrator.repository.ListItems(ctx, tenantID, batch.RequestID)
	if err != nil {
		return err
	}
	for _, item := range items {
		if item.LeaseDeadline != nil && !HandoverLeaseExpired(*item.LeaseDeadline, now) {
			return fmt.Errorf("handover item %s is still in flight; retry after the lease expires: %w", item.ID, ErrConflict)
		}
		if item.BatchID == batchID && item.Attempts > 0 && item.Status == HandoverItemPending {
			client, ok := orchestrator.clients[item.System]
			if !ok {
				return ErrConflict
			}
			receipt, err := client.Receipt(ctx, item.ID, item.IdempotencyKey)
			if errors.Is(err, ErrNotFound) {
				continue
			}
			if err != nil {
				return fmt.Errorf("handover result is unknown: %w", ErrConflict)
			}
			if terminal, _ := HandoverReceiptTerminal(receipt.Status); !terminal {
				return fmt.Errorf("handover result is not terminal: %w", ErrConflict)
			}
			if receipt.ItemID != item.ID || receipt.IdempotencyKey != item.IdempotencyKey || receipt.System != item.System {
				return ErrConflict
			}
			if _, err := orchestrator.repository.RecordReceipt(ctx, tenantID, batchID, item.Version, receipt, now); err != nil {
				return err
			}
		}
	}
	systems, err := orchestrator.repository.ListSystemStates(ctx, tenantID, batchID)
	if err != nil {
		return err
	}
	for _, state := range systems {
		if _, notApplicable := HandoverSystemDisposition(state.Capability); notApplicable && !state.Frozen {
			continue
		}
		if client, ok := orchestrator.clients[state.System]; ok {
			if err := client.Finish(ctx, batchID, batch.RequestID, handoverFinishAbort); err != nil {
				return fmt.Errorf("cancel handover on %s: %w", state.System, err)
			}
		} else {
			return fmt.Errorf("cannot release frozen system %s: %w", state.System, ErrConflict)
		}
	}
	return orchestrator.repository.CompleteBatch(ctx, tenantID, batchID, HandoverBatchCancelled, now)
}
