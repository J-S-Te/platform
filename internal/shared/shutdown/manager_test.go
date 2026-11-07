package shutdown

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// testLogger 丢弃日志，避免测试输出干扰，同时保留生产环境的调用路径。
func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// canceledContext 模拟进程已收到 SIGTERM：Manager.Run 应立即进入排空。
func canceledContext() context.Context {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	return ctx
}

// 注册逆序：后注册的钩子代表更靠近请求路径的资源，必须最先释放。
func TestRunExecutesHooksInReverseRegistrationOrder(t *testing.T) {
	manager := New(2 * time.Second).WithLogger(testLogger())
	var mutex sync.Mutex
	var order []string
	for _, name := range []string{"first", "second", "third"} {
		hookName := name
		manager.Register(Hook{
			Name:   hookName,
			Budget: time.Second,
			Run: func(context.Context) error {
				mutex.Lock()
				defer mutex.Unlock()
				order = append(order, hookName)
				return nil
			},
		})
	}

	if err := manager.Run(canceledContext()); err != nil {
		t.Fatalf("Run 返回错误: %v", err)
	}

	want := []string{"third", "second", "first"}
	if len(order) != len(want) {
		t.Fatalf("钩子执行次数 = %d，期望 %d（实际顺序 %v）", len(order), len(want), order)
	}
	for index, name := range want {
		if order[index] != name {
			t.Fatalf("钩子执行顺序 = %v，期望 %v", order, want)
		}
	}
}

// 单个钩子超时必须只影响自己：后续钩子照常执行，错误进入聚合结果。
func TestRunContinuesAfterHookTimeout(t *testing.T) {
	manager := New(2 * time.Second).WithLogger(testLogger())
	release := make(chan struct{})
	defer close(release)
	manager.Register(Hook{
		Name:   "slow",
		Budget: 20 * time.Millisecond,
		Run: func(context.Context) error {
			<-release // 故意忽略 ctx：验证 Manager 用自身预算兜底
			return nil
		},
	})
	var fastRan atomic.Bool
	manager.Register(Hook{
		Name:   "fast",
		Budget: time.Second,
		Run: func(context.Context) error {
			fastRan.Store(true)
			return nil
		},
	})

	start := time.Now()
	err := manager.Run(canceledContext())
	elapsed := time.Since(start)

	if elapsed > time.Second {
		t.Fatalf("钩子超时后 Run 仍阻塞 %s，预算兜底失效", elapsed)
	}
	if err == nil {
		t.Fatal("钩子超时未返回错误")
	}
	if !errors.Is(err, ErrHookTimeout) {
		t.Fatalf("错误未包装 ErrHookTimeout: %v", err)
	}
	if !strings.Contains(err.Error(), "slow") {
		t.Fatalf("错误未包含超时钩子名称: %v", err)
	}
	if !fastRan.Load() {
		t.Fatal("前一个钩子超时阻断了后续钩子")
	}
}

// 钩子 panic 不能击穿 Manager，也不能阻断后续钩子。
func TestRunRecoversHookPanicAndContinues(t *testing.T) {
	manager := New(2 * time.Second).WithLogger(testLogger())
	manager.Register(Hook{
		Name:   "panicky",
		Budget: time.Second,
		Run: func(context.Context) error {
			panic("boom")
		},
	})
	var survivorRan atomic.Bool
	manager.Register(Hook{
		Name:   "survivor",
		Budget: time.Second,
		Run: func(context.Context) error {
			survivorRan.Store(true)
			return nil
		},
	})

	err := manager.Run(canceledContext())

	if err == nil || !strings.Contains(err.Error(), "panicky") {
		t.Fatalf("钩子 panic 未聚合为含名称的错误: %v", err)
	}
	if !survivorRan.Load() {
		t.Fatal("钩子 panic 阻断了后续钩子")
	}
}

// 多个钩子失败时用 errors.Join 聚合，调用方可用 errors.Is 识别每一个原因。
func TestRunAggregatesHookErrors(t *testing.T) {
	manager := New(2 * time.Second).WithLogger(testLogger())
	boomOne := errors.New("boom-one")
	boomTwo := errors.New("boom-two")
	for name, cause := range map[string]error{"one": boomOne, "two": boomTwo} {
		hookName, hookCause := name, cause
		manager.Register(Hook{
			Name:   hookName,
			Budget: time.Second,
			Run: func(context.Context) error {
				return hookCause
			},
		})
	}

	err := manager.Run(canceledContext())

	if !errors.Is(err, boomOne) || !errors.Is(err, boomTwo) {
		t.Fatalf("聚合错误丢失原因: %v", err)
	}
	if !strings.Contains(err.Error(), "one") || !strings.Contains(err.Error(), "two") {
		t.Fatalf("聚合错误未标明失败钩子名称: %v", err)
	}
}

// Run 必须等待被跟踪的后台协程完成清理，而不是只等钩子返回。
func TestRunWaitsForTrackedGoroutines(t *testing.T) {
	manager := New(2 * time.Second).WithLogger(testLogger())
	var drained atomic.Bool
	manager.Go("background-loop", func(ctx context.Context) {
		<-ctx.Done()
		time.Sleep(20 * time.Millisecond) // 模拟在途工作排空
		drained.Store(true)
	})
	manager.Register(Hook{
		Name:   "noop",
		Budget: time.Second,
		Run: func(context.Context) error {
			return nil
		},
	})

	if err := manager.Run(canceledContext()); err != nil {
		t.Fatalf("Run 返回错误: %v", err)
	}
	if !drained.Load() {
		t.Fatal("Run 未等待被跟踪的后台协程退出")
	}
}

// 总预算耗尽：立即返回，报告未完成的钩子与后台协程名称，且不再启动剩余钩子。
func TestRunReturnsWhenTotalBudgetExhaustedAndReportsUnfinishedWork(t *testing.T) {
	manager := New(80 * time.Millisecond).WithLogger(testLogger())
	release := make(chan struct{})
	defer close(release)
	var skippedRan atomic.Bool
	manager.Register(Hook{
		Name:   "early-skipped",
		Budget: time.Second,
		Run: func(context.Context) error {
			skippedRan.Store(true)
			return nil
		},
	})
	manager.Register(Hook{
		Name:   "stubborn-hook",
		Budget: 5 * time.Second,
		Run: func(context.Context) error {
			<-release // 故意忽略 ctx 与自身预算
			return nil
		},
	})
	manager.Go("stubborn-goroutine", func(context.Context) {
		<-release // 故意忽略取消信号
	})

	start := time.Now()
	err := manager.Run(canceledContext())
	elapsed := time.Since(start)

	if elapsed > time.Second {
		t.Fatalf("总预算 80ms 耗尽后 Run 仍阻塞 %s", elapsed)
	}
	if !errors.Is(err, ErrBudgetExhausted) {
		t.Fatalf("错误未包装 ErrBudgetExhausted: %v", err)
	}
	for _, name := range []string{"stubborn-hook", "stubborn-goroutine", "early-skipped"} {
		if !strings.Contains(err.Error(), name) {
			t.Fatalf("错误未报告未完成项 %q: %v", name, err)
		}
	}
	if skippedRan.Load() {
		t.Fatal("总预算耗尽后仍启动了剩余钩子")
	}
}

// 零预算按默认值处理，避免误传 0 立刻判定超时。
func TestNewAppliesDefaultBudgetForNonPositiveValue(t *testing.T) {
	manager := New(0)
	if manager.totalBudget <= 0 {
		t.Fatalf("totalBudget = %s，期望正数默认值", manager.totalBudget)
	}
	if err := manager.Run(canceledContext()); err != nil {
		t.Fatalf("空 Manager Run 返回错误: %v", err)
	}
}
