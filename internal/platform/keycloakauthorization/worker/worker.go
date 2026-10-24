// Package worker 消费持久化的 Keycloak 授权投影事件。
package worker

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"
)

// Event 是一条待投影的 Keycloak 授权 outbox 事件；Attempts 记录已执行次数，
// 供重试预算与退避计算使用。
type Event struct {
	ID, TenantID, IdentityID, ApplicationID, EnvironmentID, EventType string
	Attempts                                                          uint
}

// Queue 是投影事件的持久化队列契约：支持回收崩溃遗留锁、按 worker 领取、
// 成功完成、退避重试和转入死信。
type Queue interface {
	RecoverStale(context.Context, time.Time, time.Time) error
	Claim(context.Context, string, time.Time) (Event, bool, error)
	Complete(context.Context, Event) error
	Retry(context.Context, Event, string, string, time.Time) error
	Fail(context.Context, Event, string, string) error
}

// Synchronizer 持有对 Keycloak Admin API 的调用职责。它必须创建/更新稳定的
// identity_id 用户映射、组织组，以及仅限目标应用的 Client Roles。
type Synchronizer interface {
	SyncAuthorization(context.Context, Event) error
}

// EligibilityReconciler 检测账号过期及角色、任职有效窗口。这类状态随时间自然变化，
// 不一定伴随写事件，需要周期对账。
type EligibilityReconciler interface {
	Reconcile(context.Context, time.Time) error
}

// Clock 提供当前时间；测试可注入确定性时钟。
type Clock interface{ Now() time.Time }

type systemClock struct{}

// Now 返回当前 UTC 时间。
func (systemClock) Now() time.Time { return time.Now().UTC() }

// Worker 消费授权投影 outbox 事件并驱动 Keycloak 同步；轮询、过期锁回收、
// 重试退避与死信转移均以队列契约为准。
type Worker struct {
	queue        Queue
	synchronizer Synchronizer
	logger       *slog.Logger
	workerID     string
	poll         time.Duration
	clock        Clock
	eligibility  EligibilityReconciler
	// StaleLockTimeout 是事件允许停留在 RUNNING 状态的最长时间；超过后其他
	// worker 将判定其属主已崩溃，把事件退回 PENDING。
	StaleLockTimeout time.Duration
	// MaxAttempts 限制 Keycloak 控制面失败的重复次数。重试预算耗尽后事件转为
	// FAILED（死信状态），持续阻塞认证提供方切换，直到运维经受控恢复路径重放。
	MaxAttempts uint
}

// SetEligibilityReconciler 挂接可选的账号资格对账器；必须在启动 Run 循环之前调用。
func (worker *Worker) SetEligibilityReconciler(reconciler EligibilityReconciler) {
	worker.eligibility = reconciler
}

// New 构建 Keycloak 权限投影 worker，初始化并校验必要依赖与参数。
// queue 与 synchronizer 为外部服务抽象，logger 和 workerID 为可观测性依赖；
// poll 为轮询周期，clocks 允许测试桩替换时钟，超出 1 个时钟实例视为配置错误。
func New(queue Queue, synchronizer Synchronizer, logger *slog.Logger, workerID string, poll time.Duration, clocks ...Clock) (*Worker, error) {
	if queue == nil || synchronizer == nil || logger == nil || strings.TrimSpace(workerID) == "" || poll <= 0 || len(clocks) > 1 {
		return nil, errors.New("Keycloak authorization worker configuration is invalid")
	}
	clock := Clock(systemClock{})
	if len(clocks) == 1 {
		if clocks[0] == nil {
			return nil, errors.New("Keycloak authorization worker clock must not be nil")
		}
		clock = clocks[0]
	}
	return &Worker{queue: queue, synchronizer: synchronizer, logger: logger, workerID: strings.TrimSpace(workerID), poll: poll, clock: clock, StaleLockTimeout: 5 * time.Minute, MaxAttempts: 5}, nil
}

// Run 持续轮询并处理事件，依赖 context 终止信号退出。
// 每次循环都先执行 RunOnce，再等待 poll 周期；RunOnce 返回错误不会终止主循环，仅记录日志。
func (worker *Worker) Run(ctx context.Context) {
	ticker := time.NewTicker(worker.poll)
	defer ticker.Stop()
	for {
		if err := worker.RunOnce(ctx); err != nil && !errors.Is(err, context.Canceled) {
			worker.logger.Error("Keycloak authorization projection cycle failed", "error", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// RunOnce 执行一次“回收过期锁-领取-同步-成功/重试/死信”的完整闭环。
// 返回 nil 表示本轮已正确结束（包含重试排队）；返回错误表示无法安全推进，例如依赖回收或领取失败。
func (worker *Worker) RunOnce(ctx context.Context) error {
	// 每轮先回收崩溃遗留锁，再完成一次“领取—投影—成功或退避重试”的闭环。
	now := worker.clock.Now().UTC()
	if worker.StaleLockTimeout <= 0 {
		return errors.New("Keycloak authorization worker stale lock timeout must be positive")
	}
	if err := worker.queue.RecoverStale(ctx, now.Add(-worker.StaleLockTimeout), now); err != nil {
		return fmt.Errorf("recover stale Keycloak authorization outbox: %w", err)
	}
	if worker.eligibility != nil {
		if err := worker.eligibility.Reconcile(ctx, now); err != nil {
			return fmt.Errorf("reconcile Keycloak account eligibility: %w", err)
		}
	}
	event, claimed, err := worker.queue.Claim(ctx, worker.workerID, now)
	if err != nil || !claimed {
		return err
	}
	if err := worker.synchronizer.SyncAuthorization(ctx, event); err != nil {
		if worker.MaxAttempts == 0 {
			return errors.New("Keycloak authorization worker max attempts must be positive")
		}
		if event.Attempts >= worker.MaxAttempts {
			if failErr := worker.queue.Fail(ctx, event, "KEYCLOAK_SYNC_RETRY_EXHAUSTED", trimError(err)); failErr != nil {
				return fmt.Errorf("sync Keycloak authorization: %v; dead-letter event: %w", err, failErr)
			}
			worker.logger.Error("Keycloak authorization projection moved to dead-letter state", "event_id", event.ID, "attempts", event.Attempts, "error", err)
			return nil
		}
		retryAt := worker.clock.Now().UTC().Add(retryDelay(event.Attempts))
		if retryErr := worker.queue.Retry(ctx, event, "KEYCLOAK_SYNC_FAILED", trimError(err), retryAt); retryErr != nil {
			return fmt.Errorf("sync Keycloak authorization: %v; schedule retry: %w", err, retryErr)
		}
		return nil
	}
	return worker.queue.Complete(ctx, event)
}

func retryDelay(attempts uint) time.Duration {
	if attempts > 6 {
		attempts = 6
	}
	return time.Second * time.Duration(1<<attempts)
}

func trimError(err error) string {
	message := strings.TrimSpace(err.Error())
	if len(message) > 1000 {
		return message[:1000]
	}
	return message
}
