package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/J-S-Te/Basic-Platform/internal/platform/applicationregistry/application"
	"github.com/J-S-Te/Basic-Platform/internal/shared/shutdown"
)

// fakeSubsystemProvisioner 是本文件的专用 fake：只模拟部署执行器的阻塞与返回，
// 不接触 Docker、宿主机目录或运行中的子系统，因此装配测试可以跑在普通单测环境里。
type fakeSubsystemProvisioner struct {
	started chan struct{}
	release chan struct{}
	// ignoreCancellation 为 true 时执行器不响应 ctx 取消，用于验证超预算路径。
	ignoreCancellation bool

	startOnce   sync.Once
	releaseOnce sync.Once
}

func newFakeSubsystemProvisioner(ignoreCancellation bool) *fakeSubsystemProvisioner {
	return &fakeSubsystemProvisioner{
		started:            make(chan struct{}),
		release:            make(chan struct{}),
		ignoreCancellation: ignoreCancellation,
	}
}

func (fake *fakeSubsystemProvisioner) Preflight(ctx context.Context, _ application.SubsystemPreflightInput) error {
	fake.startOnce.Do(func() { close(fake.started) })
	if !fake.ignoreCancellation {
		// 模拟一个正在执行的部署操作：收到停机取消后立刻响应，但真正退场时刻由用例控制，
		// 这样才能观测到 run 是否真的在等这个处理协程，而不是提前返回。
		<-ctx.Done()
	}
	<-fake.release
	if fake.ignoreCancellation {
		return nil
	}
	return ctx.Err()
}

func (fake *fakeSubsystemProvisioner) Provision(context.Context, application.SubsystemProvisioningInput) error {
	return nil
}

func (fake *fakeSubsystemProvisioner) Update(context.Context, application.SubsystemProvisioningInput) error {
	return nil
}

func (fake *fakeSubsystemProvisioner) Teardown(context.Context, string, string, string) error {
	return nil
}

// unblock 释放卡在执行器里的在途请求；可重复调用，避免用例提前失败时泄漏协程。
func (fake *fakeSubsystemProvisioner) unblock() {
	fake.releaseOnce.Do(func() { close(fake.release) })
}

// TestRunDrainsInFlightProvisioningRequestBeforeExit 验证部署 Agent 的停机把在途连接处理
// 纳入了 join 预算：run 必须被在途处理协程挡住，且返回时回执已经写完，而不是提前返回后被
// 进程退出截断。
func TestRunDrainsInFlightProvisioningRequestBeforeExit(t *testing.T) {
	executor := newFakeSubsystemProvisioner(false)
	connection, cancel, done := startProvisioner(t, 5*time.Second, executor)

	cancel() // 等价于收到 SIGTERM。

	// 处理协程只在用例放行后才退场：run 若在放行前就返回，说明在途连接没有被 join。
	select {
	case runErr := <-done:
		t.Fatalf("run returned before the in-flight handler finished: err=%v", runErr)
	case <-time.After(100 * time.Millisecond):
	}

	executor.unblock()

	var runErr error
	select {
	case runErr = <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("run did not return after the in-flight request finished")
	}
	if runErr != nil {
		t.Fatalf("drain returned error: %v", runErr)
	}

	// run 返回时处理协程已退出，回执必须已经落在连接上。
	_ = connection.SetReadDeadline(time.Now().Add(time.Second))
	var reply struct {
		Success bool   `json:"success"`
		Message string `json:"message"`
	}
	if err := json.NewDecoder(connection).Decode(&reply); err != nil {
		t.Fatalf("in-flight reply was not drained before exit: %v", err)
	}
	if reply.Success {
		t.Fatalf("in-flight request canceled by shutdown reported success: %#v", reply)
	}
}

// TestRunReportsBudgetExhaustionWhenInFlightRequestIgnoresCancellation 验证超预算路径：
// 执行器不响应取消时，run 必须在预算内以点名未退出组件的错误返回（对应非零退出码）。
func TestRunReportsBudgetExhaustionWhenInFlightRequestIgnoresCancellation(t *testing.T) {
	executor := newFakeSubsystemProvisioner(true)
	const budget = 200 * time.Millisecond
	_, cancel, done := startProvisioner(t, budget, executor)
	t.Cleanup(executor.unblock)

	cancel()

	var runErr error
	select {
	case runErr = <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("run ignored the shutdown budget")
	}
	if runErr == nil {
		t.Fatal("drain returned nil although the in-flight request never exited")
	}
	if !errors.Is(runErr, shutdown.ErrBudgetExhausted) {
		t.Fatalf("drain error = %v, want budget exhaustion", runErr)
	}
	if !strings.Contains(runErr.Error(), "provisioning-server") {
		t.Fatalf("drain error does not name the unfinished component: %v", runErr)
	}

	// 释放卡住的请求，让监听协程也能收尾，避免测试结束时留下仍在运行的协程。
	executor.unblock()
}

// startProvisioner 在临时 socket 上启动 run，返回已进入在途状态的连接、停机触发函数
// 和 run 的结果通道。返回时 preflight 请求已确认到达执行器，后续 cancel 面对的是真正的在途请求。
func startProvisioner(t *testing.T, budget time.Duration, executor *fakeSubsystemProvisioner) (net.Conn, context.CancelFunc, <-chan error) {
	t.Helper()
	// Unix socket 路径长度受平台限制（Linux 约 108 字节），/tmp 下的短目录最稳妥。
	directory, err := os.MkdirTemp("/tmp", "bp-provisioner-cmd-")
	if err != nil {
		t.Fatalf("create socket directory: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(directory) })
	socketPath := filepath.Join(directory, "provisioner.sock")
	t.Setenv("SUBSYSTEM_PROVISIONING_SOCKET_PATH", socketPath)

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	done := make(chan error, 1)
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	go func() {
		done <- run(ctx, logger, budget, executor)
	}()

	waitForSocket(t, socketPath)

	connection, err := net.Dial("unix", socketPath)
	if err != nil {
		t.Fatalf("dial provisioning socket: %v", err)
	}
	t.Cleanup(func() { _ = connection.Close() })
	// 请求字段与 infrastructure.subsystemProvisioningRequest 对齐；fake 执行器收到 preflight
	// 后立即阻塞，因此这条连接在整个停机过程中保持在途状态。
	if err := json.NewEncoder(connection).Encode(map[string]any{
		"version":    2,
		"action":     "preflight",
		"request_id": "01KZ42MPYY9168FKFPBXVTX677",
		"preflight": map[string]any{
			"tenant_id": "tenant-1", "application_code": "contract_management", "environment": "dev",
		},
	}); err != nil {
		t.Fatalf("send preflight request: %v", err)
	}
	select {
	case <-executor.started:
	case <-time.After(2 * time.Second):
		t.Fatal("in-flight provisioning request never reached the executor")
	}
	return connection, cancel, done
}

func waitForSocket(t *testing.T, socketPath string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(socketPath); err == nil {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("provisioning socket %q was not created", socketPath)
}
