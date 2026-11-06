// Package shutdown 提供进程内统一的有界停机骨架。
//
// 设计目标只有三个：按注册逆序执行清理钩子、在总预算内等待后台协程退出、
// 把超时与失败原样聚合上报，避免每个长驻进程各写一套互不一致的收尾逻辑。
// 该包不启动任何后台协程，也不接管信号；调用方仍用 signal.NotifyContext 得到
// 取消信号，再把该 context 交给 Run。
package shutdown

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"
)

// 默认总预算：用于调用方误传非正数时的兜底，保证 Run 不会因为 0 预算立即失败。
const defaultTotalBudget = 30 * time.Second

// ErrBudgetExhausted 表示总停机预算已耗尽，仍有钩子或后台协程未完成。
var ErrBudgetExhausted = errors.New("shutdown: total budget exhausted")

// ErrHookTimeout 表示单个清理钩子超出自身预算（或被总预算兜底截断）。
var ErrHookTimeout = errors.New("shutdown: hook timeout")

// Hook 是一个停机清理步骤。
//
//	Name 用于日志与聚合错误，必须可辨识；Budget 是该步骤自身的上限，
//	非正数表示只受总预算约束；Run 收到一个带自身预算的 context。
type Hook struct {
	Name   string
	Budget time.Duration
	Run    func(ctx context.Context) error
}

// Manager 协调一次停机：钩子逆序执行，随后等待被跟踪的后台协程退出。
// Manager 的字段在 Run 开始前后由互斥锁保护；所有导出方法都不 panic，
// 即使钩子 panic 也只记录为聚合错误，不阻断后续钩子。
type Manager struct {
	totalBudget time.Duration
	logger      *slog.Logger

	mutex   sync.Mutex
	hooks   []Hook
	tracked []*trackedGoroutine
	group   sync.WaitGroup
	ran     bool

	// lifecycle 传递给被跟踪的后台协程；Run 在收到停机信号后取消它，
	// 调用方因此不需要自己把进程 context 传进每一个后台循环。
	lifecycle context.Context
	cancel    context.CancelFunc
}

// trackedGoroutine 记录一个被跟踪后台协程的名称与完成信号。
type trackedGoroutine struct {
	name string
	done chan struct{}
}

// New 创建停机管理器。totalBudget 是"收到信号到进程退出"的总上限，
// 覆盖全部钩子与被跟踪协程；非正数回落到默认预算（见 defaultTotalBudget）。
func New(totalBudget time.Duration) *Manager {
	if totalBudget <= 0 {
		totalBudget = defaultTotalBudget
	}
	lifecycle, cancel := context.WithCancel(context.Background())
	return &Manager{
		totalBudget: totalBudget,
		logger:      slog.Default(),
		lifecycle:   lifecycle,
		cancel:      cancel,
	}
}

// WithLogger 指定日志输出，nil 表示沿用 slog.Default()。返回自身便于链式调用。
func (manager *Manager) WithLogger(logger *slog.Logger) *Manager {
	if manager == nil {
		return nil
	}
	if logger != nil {
		manager.logger = logger
	}
	return manager
}

// Register 登记一个清理钩子；Run 按登记逆序执行。Run 开始后登记无效（记录告警并忽略）。
func (manager *Manager) Register(hook Hook) {
	if manager == nil {
		return
	}
	if hook.Run == nil {
		manager.logger.Warn("ignore shutdown hook without run function", "hook", hook.Name)
		return
	}
	manager.mutex.Lock()
	defer manager.mutex.Unlock()
	if manager.ran {
		manager.logger.Warn("ignore shutdown hook registered after drain started", "hook", hook.Name)
		return
	}
	manager.hooks = append(manager.hooks, hook)
}

// Go 在独立协程里运行后台循环并跟踪它，fn 收到的是 Manager 的生命周期 context。
// Run 会在钩子执行完毕后等待这些协程；超时后点名报告未退出的协程。
func (manager *Manager) Go(name string, fn func(context.Context)) {
	if manager == nil || fn == nil {
		return
	}
	label := strings.TrimSpace(name)
	if label == "" {
		label = "anonymous"
	}
	stats := &trackedGoroutine{name: label, done: make(chan struct{})}
	manager.mutex.Lock()
	if manager.ran {
		manager.logger.Warn("track background goroutine after drain started", "goroutine", label)
	}
	manager.tracked = append(manager.tracked, stats)
	manager.mutex.Unlock()
	manager.group.Add(1)
	go func() {
		defer manager.group.Done()
		defer close(stats.done)
		fn(manager.lifecycle)
	}()
}

// Run 阻塞到 ctx 结束（通常是进程信号），然后：
//  1. 取消生命周期 context，通知后台协程开始退出；
//  2. 按注册逆序执行钩子，每个钩子受自身预算约束、整体受总预算兜底；
//  3. 在总预算剩余时间内等待被跟踪协程退出；
//  4. 返回 errors.Join 聚合的全部失败，若无失败返回 nil。
//
// 单个钩子失败或超时不会阻断后续钩子；总预算耗尽时立即返回并点名未完成项。
func (manager *Manager) Run(ctx context.Context) error {
	if manager == nil {
		return nil
	}
	if ctx == nil {
		ctx = context.Background()
	}

	manager.mutex.Lock()
	if manager.ran {
		manager.mutex.Unlock()
		return errors.New("shutdown: manager already drained")
	}
	manager.ran = true
	hooks := append([]Hook(nil), manager.hooks...)
	tracked := append([]*trackedGoroutine(nil), manager.tracked...)
	manager.mutex.Unlock()

	<-ctx.Done()

	// 生命周期 context 与调用方 context 解耦：后台协程只看取消信号，
	// 不依赖调用方 context 是否带 deadline。
	manager.cancel()
	manager.logger.Info("shutdown draining started",
		"hooks", len(hooks), "goroutines", len(tracked), "total_budget", manager.totalBudget.String())

	deadline := time.Now().Add(manager.totalBudget)
	budgetContext, cancelBudget := context.WithDeadline(context.Background(), deadline)
	defer cancelBudget()

	var errs []error

	// 逆序执行：后注册的钩子对应更靠近入口的资源，必须最先停止接收新工作。
	for index := len(hooks) - 1; index >= 0; index-- {
		hook := hooks[index]
		if budgetContext.Err() != nil {
			// 总预算已耗尽：剩余钩子不再启动，只把名称写进聚合错误。
			pending := hookNames(hooks[:index+1])
			errs = append(errs, fmt.Errorf("%w: hooks not started: %s", ErrBudgetExhausted, strings.Join(pending, ", ")))
			manager.logger.Error("shutdown total budget exhausted before remaining hooks", "hooks", strings.Join(pending, ", "))
			break
		}
		if err := runHook(budgetContext, hook); err != nil {
			manager.logger.Error("shutdown hook failed", "hook", hook.Name, "error", err)
			errs = append(errs, err)
		}
	}

	// 钩子结束后等待后台协程；同样受总预算约束，超时后点名报告。
	joined := make(chan struct{})
	go func() {
		manager.group.Wait()
		close(joined)
	}()
	select {
	case <-joined:
	case <-budgetContext.Done():
		// 使用实时快照：即便 Run 开始后又有人登记协程，这里也能点名。
		manager.mutex.Lock()
		live := append([]*trackedGoroutine(nil), manager.tracked...)
		manager.mutex.Unlock()
		unfinished := unfinishedNames(live)
		errs = append(errs, fmt.Errorf("%w: %d background goroutine(s) still running: %s",
			ErrBudgetExhausted, len(unfinished), strings.Join(unfinished, ", ")))
		manager.logger.Error("shutdown background goroutines did not exit",
			"count", len(unfinished), "goroutines", strings.Join(unfinished, ", "))
	}

	if len(errs) > 0 {
		err := errors.Join(errs...)
		manager.logger.Error("shutdown drained with errors", "errors", err.Error())
		return err
	}
	manager.logger.Info("shutdown drained cleanly", "hooks", len(hooks), "goroutines", len(tracked))
	return nil
}

// runHook 在独立协程里执行一个钩子，并用钩子预算与总预算中较小者兜底。
// 即使钩子忽略 context，也不会永久阻塞 Run。
func runHook(parent context.Context, hook Hook) error {
	budget := hook.Budget
	if budget <= 0 {
		// 未声明自身预算时只受总预算约束；父 context 无截止时间则回落到默认预算。
		if deadline, ok := parent.Deadline(); ok {
			budget = time.Until(deadline)
		} else {
			budget = defaultTotalBudget
		}
	}
	if budget <= 0 {
		// 总预算已耗尽，钩子无法获得任何执行窗口。
		budget = time.Nanosecond
	}
	hookContext, cancel := context.WithTimeout(parent, budget)
	defer cancel()

	hookErrors := make(chan error, 1)
	go func() {
		defer func() {
			if recovered := recover(); recovered != nil {
				hookErrors <- fmt.Errorf("shutdown: hook %q panicked: %v", hook.Name, recovered)
			}
		}()
		hookErrors <- hook.Run(hookContext)
	}()

	select {
	case err := <-hookErrors:
		if err == nil {
			return nil
		}
		return fmt.Errorf("shutdown: hook %q failed: %w", hook.Name, err)
	case <-hookContext.Done():
		if parent.Err() != nil {
			return fmt.Errorf("%w: hook %q aborted by total budget", ErrBudgetExhausted, hook.Name)
		}
		return fmt.Errorf("%w: hook %q exceeded budget %s", ErrHookTimeout, hook.Name, hook.Budget.String())
	}
}

// unfinishedNames 返回尚未退出的被跟踪协程名称（按登记顺序）。
func unfinishedNames(tracked []*trackedGoroutine) []string {
	names := make([]string, 0, len(tracked))
	for _, stats := range tracked {
		select {
		case <-stats.done:
		default:
			names = append(names, stats.name)
		}
	}
	if len(names) == 0 {
		names = append(names, "unknown")
	}
	return names
}

// hookNames 按原始登记顺序返回钩子名称。
func hookNames(hooks []Hook) []string {
	names := make([]string, 0, len(hooks))
	for _, hook := range hooks {
		names = append(names, hook.Name)
	}
	if len(names) == 0 {
		names = append(names, "unknown")
	}
	return names
}
