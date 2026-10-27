package infrastructure

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/J-S-Te/Basic-Platform/internal/platform/applicationregistry/application"
)

// TestAccessAgentReceiptBinding 用途：验证回执持久化、权限及代次精确绑定。
// 参数：t 为隔离文件测试上下文。
// 返回值：无。
func TestAccessAgentReceiptBinding(t *testing.T) {
	root := t.TempDir()
	receipt := application.SubsystemAccessDeploymentReceipt{TenantID: "tenant-1", OperationID: "operation-1", ApplicationCode: "settlement", Environment: "dev", Generation: 2, ManifestChecksum: "manifest-1"}
	input := application.SubsystemProvisioningInput{TenantID: receipt.TenantID, ApplicationCode: receipt.ApplicationCode, Environment: receipt.Environment, ManifestChecksum: receipt.ManifestChecksum, AccessReceipt: &receipt}
	if ok, err := readAccessDeploymentReceipt(root, receipt); err != nil || ok {
		t.Fatalf("missing receipt: %v %v", ok, err)
	}
	if err := writeAccessDeploymentReceipt(root, input); err != nil {
		t.Fatal(err)
	}
	if ok, err := readAccessDeploymentReceipt(root, receipt); err != nil || !ok {
		t.Fatalf("persisted receipt: %v %v", ok, err)
	}
	receipt.Generation++
	if ok, err := readAccessDeploymentReceipt(root, receipt); err != nil || ok {
		t.Fatalf("old receipt matched newer generation: %v %v", ok, err)
	}
	receipt.Generation--
	path, err := accessReceiptPath(root, receipt)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := readAccessDeploymentReceipt(root, receipt); err == nil {
		t.Fatal("unsafe receipt accepted")
	}
	input.TenantID = "other-tenant"
	if err := writeAccessDeploymentReceipt(root, input); err == nil {
		t.Fatal("cross-tenant receipt written")
	}
}

type recoveryReceiptState struct {
	accessLifecycleStub
	receipt application.SubsystemAccessDeploymentReceipt
}

// ReadAccessDeploymentReceipt 用途：提供已派发任务的原始绑定。
// 参数：ctx 为上下文，operation 为任务。
// 返回值：返回原清单和原配置摘要。
func (s *recoveryReceiptState) ReadAccessDeploymentReceipt(context.Context, application.SubsystemAccessOperation) (application.SubsystemAccessDeploymentReceipt, string, error) {
	return s.receipt, "old-candidate", nil
}

type recoveryReceiptAgent struct {
	deploymentProvisionerStub
	root string
}

// ReadAccessDeploymentCompletion 用途：读取真实临时目录中 Agent 的持久成功回执。
// 参数：ctx 为上下文，receipt 为待确认绑定。
// 返回值：返回是否精确命中。
func (a *recoveryReceiptAgent) ReadAccessDeploymentCompletion(_ context.Context, receipt application.SubsystemAccessDeploymentReceipt) (bool, error) {
	return readAccessDeploymentReceipt(a.root, receipt)
}

// TestAccessRecoveryAfterAgentCompletion 用途：验证 Agent 完成后平台重启不会重新部署或伪造候选摘要。
// 参数：t 为测试上下文。
// 返回值：无。
func TestAccessRecoveryAfterAgentCompletion(t *testing.T) {
	op := onboardingOperation()
	receipt := application.SubsystemAccessDeploymentReceipt{TenantID: op.TenantID, OperationID: op.ID, ApplicationCode: op.ApplicationCode, Environment: op.EnvironmentCode, Generation: 1, ManifestChecksum: "manifest-1"}
	root := t.TempDir()
	input := application.SubsystemProvisioningInput{TenantID: op.TenantID, ApplicationCode: op.ApplicationCode, Environment: op.EnvironmentCode, ManifestChecksum: receipt.ManifestChecksum, AccessReceipt: &receipt}
	if err := writeAccessDeploymentReceipt(root, input); err != nil {
		t.Fatal(err)
	}
	state := &recoveryReceiptState{accessLifecycleStub: accessLifecycleStub{phase: "DISPATCHED"}, receipt: receipt}
	agent := &recoveryReceiptAgent{root: root}
	adapter, err := NewSubsystemAccessDeploymentAdapter(&deploymentOnboardingStub{}, agent, accessCatalogWithTarget(), relatedClientsStub{}, state, issuerStub{}, nil, nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	outcome, err := adapter.ProvisionSubsystemAccess(context.Background(), op, nil)
	if err != nil || outcome.Status != application.SubsystemAccessStatusSucceeded || !state.completed || agent.provisionCalls != 0 {
		t.Fatalf("recovery: %+v %v", outcome, err)
	}
	if outcome.EvidenceRef != "candidate:v1:old-candidate;deployment:durable-completion" {
		t.Fatal("recovery replaced deployed candidate", outcome.EvidenceRef)
	}
}

// TestAccessReceiptUnixProtocol 用途：验证真实 Unix 连接能精确查询成功回执且不重新派发部署。
// 参数：t 为测试上下文，服务器执行器只访问本次临时回执目录。
// 返回值：无。
func TestAccessReceiptUnixProtocol(t *testing.T) {
	root := t.TempDir()
	receipt := application.SubsystemAccessDeploymentReceipt{TenantID: "tenant-1", OperationID: "operation-1", ApplicationCode: "settlement", Environment: "dev", Generation: 1, ManifestChecksum: "manifest-1"}
	input := application.SubsystemProvisioningInput{TenantID: receipt.TenantID, ApplicationCode: receipt.ApplicationCode, Environment: receipt.Environment, ManifestChecksum: receipt.ManifestChecksum, AccessReceipt: &receipt}
	if err := writeAccessDeploymentReceipt(root, input); err != nil {
		t.Fatal(err)
	}
	// macOS Unix socket 地址长度限制较短，socket 单独放置在本次短临时目录。
	socketDir, err := os.MkdirTemp("/tmp", "access-receipt-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(socketDir)
	socket := filepath.Join(socketDir, "agent.sock")
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	agent := &recoveryReceiptAgent{root: root}
	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < 2; i++ {
			connection, err := listener.Accept()
			if err != nil {
				return
			}
			handleSubsystemProvisioningConnection(context.Background(), connection, agent)
		}
	}()
	client := &UnixSocketSubsystemProvisioner{enabled: true, socketPath: socket, timeout: time.Second}
	if ok, err := client.ReadAccessDeploymentCompletion(context.Background(), receipt); err != nil || !ok {
		t.Fatalf("socket receipt: %v %v", ok, err)
	}
	receipt.Generation++
	if ok, err := client.ReadAccessDeploymentCompletion(context.Background(), receipt); err != nil || ok {
		t.Fatalf("socket generation binding: %v %v", ok, err)
	}
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("receipt server did not finish")
	}
	if agent.provisionCalls != 0 {
		t.Fatal("read-only receipt query redeployed subsystem")
	}
}
