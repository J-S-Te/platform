// Package worker 在人员变更到达生效时间后执行已排期请求。
package worker

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"time"

	"github.com/J-S-Te/Basic-Platform/internal/platform/identity/application"
	"github.com/J-S-Te/Basic-Platform/internal/platform/identity/domain"
)

// approvalExceptionDispatcher 是超管例外审计 outbox 的幂等投递端口（由应用层实现）。
type approvalExceptionDispatcher interface {
	DispatchOnce(context.Context, int) (application.ApprovalExceptionDispatchReport, error)
}

// approvalExceptionDispatchBatch 是单轮投递上限，避免积压时长时间占用 worker 循环。
const approvalExceptionDispatchBatch = 50

// PersonnelChangeWorker 是由数据库协调的轻量调度器；复用应用服务以共享状态机和全部校验闸门。
type PersonnelChangeWorker struct {
	service            *application.PersonnelChangeService
	logger             *slog.Logger
	workerID           string
	pollInterval       time.Duration
	approvalExceptions approvalExceptionDispatcher
}

// NewPersonnelChangeWorker 创建人员变更 Worker，完成依赖和参数校验。
// 参数 service/logger 必填，pollInterval 必须大于 0，workerID 为非空字符串；
// 返回错误表示启动参数无效，返回 *PersonnelChangeWorker 后可交由并发调度器运行。
func NewPersonnelChangeWorker(service *application.PersonnelChangeService, logger *slog.Logger, workerID string, pollInterval time.Duration) (*PersonnelChangeWorker, error) {
	// 启动时校验依赖和轮询参数，避免无效 worker 进入常驻循环。
	if service == nil || logger == nil {
		return nil, errors.New("personnel change worker dependencies must not be nil")
	}
	if strings.TrimSpace(workerID) == "" || pollInterval <= 0 {
		return nil, errors.New("personnel change worker configuration is invalid")
	}
	return &PersonnelChangeWorker{service: service, logger: logger, workerID: strings.TrimSpace(workerID), pollInterval: pollInterval}, nil
}

// SetApprovalExceptionDispatcher 注入超管例外审计 outbox 的投递器；未注入时跳过投递，
// 已提交的审批状态与 outbox 行不受影响，后续可安全补投。
func (w *PersonnelChangeWorker) SetApprovalExceptionDispatcher(dispatcher approvalExceptionDispatcher) {
	w.approvalExceptions = dispatcher
}

// Run 按固定间隔执行到期人员变更、补偿已执行离职的批次完成，随后补投超管例外审计。
// 每轮先执行一次处理，随后等待 ctx 或 ticker 触发；context 取消时立刻退出循环，防止服务关闭阻塞。
func (w *PersonnelChangeWorker) Run(ctx context.Context) {
	// 先立即处理一次，再按固定间隔轮询；取消上下文会停止后续轮询。
	ticker := time.NewTicker(w.pollInterval)
	defer ticker.Stop()
	for {
		w.ProcessDue(ctx)
		w.CompleteExecutedHandovers(ctx)
		w.DispatchApprovalExceptions(ctx)
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// CompleteExecutedHandovers 每轮只补偿最多 50 个已提交离职的远端完成操作。
func (w *PersonnelChangeWorker) CompleteExecutedHandovers(ctx context.Context) int {
	completed, err := w.service.CompleteExecutedHandovers(ctx, 50)
	if err != nil {
		w.logger.Error("complete executed personnel handovers", "worker", w.workerID, "completed", completed, "error", err)
	}
	return completed
}

// DispatchApprovalExceptions 投递一轮超管例外审计 outbox，返回成功投递条数。
// 投递失败只记录重试进度并写日志，不影响已提交的审批状态；未注入投递器时是安全 no-op。
func (w *PersonnelChangeWorker) DispatchApprovalExceptions(ctx context.Context) int {
	if w.approvalExceptions == nil {
		return 0
	}
	report, err := w.approvalExceptions.DispatchOnce(ctx, approvalExceptionDispatchBatch)
	if err != nil {
		w.logger.Error("dispatch personnel approval exception audit outbox", "worker", w.workerID, "error", err)
	}
	if report.Delivered > 0 {
		w.logger.Info("delivered personnel approval exception audit events", "worker", w.workerID, "delivered", report.Delivered)
	}
	return report.Delivered
}

// ProcessDue 执行当前已到期的 SCHEDULED 请求；仓储事务保证 Execute 幂等，多副本可安全并行。
func (w *PersonnelChangeWorker) ProcessDue(ctx context.Context) int {
	requests, err := w.service.List(ctx, "", domain.PersonnelChangeScheduled, "", "")
	if err != nil {
		w.logger.Error("list due personnel changes", "error", err)
		return 0
	}
	now := time.Now().UTC()
	processed := 0
	for _, request := range requests {
		// 列表可能包含尚未到期的排期项，时间条件在 worker 侧再次防守校验。
		if request.EffectiveAt == nil || request.EffectiveAt.After(now) {
			continue
		}
		// 使用稳定的系统 ULID 作为 updated_by；worker ID 是运维标签，可能不符合 CHAR(26) 身份列。
		_, err := w.service.Transition(ctx, application.PersonnelChangeTransitionInput{
			TenantID: request.TenantID, OperatorID: "01J00000000000000000000000",
			ID: request.ID, ToStatus: domain.PersonnelChangeExecuted,
		})
		if err != nil {
			w.logger.Error("execute due personnel change", "change_id", request.ID, "tenant_id", request.TenantID, "error", err)
			continue
		}
		processed++
	}
	return processed
}
