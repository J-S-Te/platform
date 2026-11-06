package bootstrap

import (
	"context"
	"strings"
	"testing"
	"time"
)

// cancelAwareRunner 模拟现有后台循环：启动后阻塞在 ctx.Done()，收到取消即返回。
type cancelAwareRunner struct {
	name    string
	started chan<- string
}

func (runner *cancelAwareRunner) Run(ctx context.Context) {
	if runner.started != nil {
		runner.started <- runner.name
	}
	<-ctx.Done()
}

// stubbornRunner 模拟忽略取消信号（例如卡在不可中断的 IO）的后台循环。
type stubbornRunner struct {
	release chan struct{}
}

func (runner *stubbornRunner) Run(context.Context) {
	<-runner.release
}

// 取消后必须等待全部 runner 退出，且不得触发 join 预算告警。
func TestConcurrentRunnerWaitsForRunnersAfterCancellation(t *testing.T) {
	started := make(chan string, 2)
	runner := newConcurrentRunner([]ProcessRunner{
		&cancelAwareRunner{name: "alpha", started: started},
		&cancelAwareRunner{name: "beta", started: started},
	}, testLogger(), 500*time.Millisecond)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	returned := make(chan struct{})
	go func() {
		runner.Run(ctx)
		close(returned)
	}()

	for index := 0; index < 2; index++ {
		select {
		case <-started:
		case <-time.After(2 * time.Second):
			t.Fatal("runner 未在预算内启动")
		}
	}
	cancel()

	select {
	case <-returned:
	case <-time.After(2 * time.Second):
		t.Fatal("concurrentRunner.Run 未在取消后返回")
	}
	if err := runner.JoinError(); err != nil {
		t.Fatalf("正常退出仍报告 join 失败: %v", err)
	}
}

// join 预算只在取消之后计时：取消前即使超过预算也不能误报。
func TestConcurrentRunnerJoinBudgetStartsOnlyAfterCancellation(t *testing.T) {
	started := make(chan string, 1)
	runner := newConcurrentRunner([]ProcessRunner{
		&cancelAwareRunner{name: "alpha", started: started},
	}, testLogger(), 50*time.Millisecond)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	returned := make(chan struct{})
	go func() {
		runner.Run(ctx)
		close(returned)
	}()
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("runner 未启动")
	}

	select {
	case <-returned:
		t.Fatal("取消前 runner 仍在运行，concurrentRunner.Run 不应返回")
	case <-time.After(120 * time.Millisecond):
	}
	cancel()
	select {
	case <-returned:
	case <-time.After(2 * time.Second):
		t.Fatal("取消后 concurrentRunner.Run 未返回")
	}
	if err := runner.JoinError(); err != nil {
		t.Fatalf("取消后正常退出仍报告 join 失败: %v", err)
	}
}

// 超出 join 预算仍未退出的 runner 必须被点名报告，而不是静默等待或吞掉。
func TestConcurrentRunnerReportsRunnersThatIgnoreCancellation(t *testing.T) {
	release := make(chan struct{})
	defer close(release)
	runner := newConcurrentRunner([]ProcessRunner{
		&stubbornRunner{release: release},
	}, testLogger(), 40*time.Millisecond)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	start := time.Now()
	runner.Run(ctx)
	elapsed := time.Since(start)

	if elapsed > time.Second {
		t.Fatalf("join 预算 40ms 未生效，Run 阻塞 %s", elapsed)
	}
	joinErr := runner.JoinError()
	if joinErr == nil {
		t.Fatal("未退出的 runner 未被报告")
	}
	if !strings.Contains(joinErr.Error(), "stubbornRunner") {
		t.Fatalf("join 错误未包含未退出 runner 的名称: %v", joinErr)
	}
	if !strings.Contains(joinErr.Error(), "1") {
		t.Fatalf("join 错误未包含未退出数量: %v", joinErr)
	}
}
