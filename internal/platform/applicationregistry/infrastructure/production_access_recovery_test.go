package infrastructure

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/J-S-Te/Basic-Platform/internal/platform/applicationregistry/application"
)

type accessRecoveryRunner struct{ recordingSubsystemRunner }

// RunOutput 返回隔离Docker替身的实际容器清单，不执行外部命令。
// 参数：其余参数沿用既有命令接口，arguments仅用于识别固定compose ps。
// 返回值：固定测试容器事实；非该只读命令返回空输出。
func (r *accessRecoveryRunner) RunOutput(ctx context.Context, directory string,
	environment []string, name string, arguments ...string) ([]byte, error) {
	if err := r.Run(ctx, directory, environment, name, arguments...); err != nil {
		return nil, err
	}
	if strings.Contains(strings.Join(arguments, " "), "ps -a --format json") {
		return []byte(`[{"Service":"contract-api","ID":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","Image":"example/contract@sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb","State":"running","Health":"healthy"}]`), nil
	}
	return nil, nil
}

// TestAccessRecoveryRejectsLinkedRuntimeBeforeCreatingLock 验证拒绝目录链接前不写外部锁。
// 参数：t为隔离部署根及外部目录的上下文。
// 返回值：无，不安全目录不得产生任何新文件。
func TestAccessRecoveryRejectsLinkedRuntimeBeforeCreatingLock(t *testing.T) {
	p, target, _, request := accessRecoveryFixture(t)
	runtime := filepath.Join(target.config.DeployRoot, "runtime")
	if err := os.Rename(runtime, runtime+"-original"); err != nil {
		t.Fatal(err)
	}
	external := t.TempDir()
	if err := os.Symlink(external, runtime); err != nil {
		t.Fatal(err)
	}
	if _, err := p.CheckAccessDeploymentRecovery(context.Background(), request); err == nil {
		t.Fatal("linked runtime accepted")
	}
	if _, err := os.Lstat(filepath.Join(external, ".deploy.lock")); !os.IsNotExist(err) {
		t.Fatal("rejected runtime link created an external deployment lock", err)
	}
}

// accessRecoveryFixture 创建明确隔离的受控目标和原代次。
// 参数：t为测试上下文，运行文件仅位于本测试临时目录。
// 返回值：生产Agent替身、受控目标、源文件路径和人工恢复请求。
func accessRecoveryFixture(t *testing.T) (*ProductionComposeSubsystemProvisioner,
	*productionComposeTarget, string, application.SubsystemAccessAbandonRequest) {
	t.Helper()
	p, path := productionProvisionerFixtureWithRunner(t, false, &accessRecoveryRunner{})
	target, err := p.target("contract_management", "prod")
	if err != nil {
		t.Fatal(err)
	}
	request := application.SubsystemAccessAbandonRequest{
		Receipt: application.SubsystemAccessDeploymentReceipt{TenantID: "tenant-1",
			OperationID: "operation-1", ApplicationCode: "contract_management", Environment: "prod",
			Generation: 5, ManifestChecksum: target.config.Profile.Checksum},
		OperatorID: "operator-1", Reason: "verified original failure and reviewed current runtime facts"}
	return p, target, path, request
}

// TestAccessManualFencePreservesRuntime 验证人工终止只保存真实决定并阻止旧代次。
// 参数：t为隔离Agent、运行文件和容器替身的上下文。
// 返回值：无；运行配置必须不变，原代次不得重放。
func TestAccessManualFencePreservesRuntime(t *testing.T) {
	p, target, path, request := accessRecoveryFixture(t)
	before, _ := os.ReadFile(path)
	evidence, err := p.CheckAccessDeploymentRecovery(context.Background(), request)
	if err != nil || len(evidence.EvidenceDigest) != 64 || len(evidence.Containers) != 1 {
		t.Fatalf("recovery evidence: %+v %v", evidence, err)
	}
	request.EvidenceDigest = evidence.EvidenceDigest
	value, err := p.AbandonAccessDeployment(context.Background(), request)
	if err != nil || value.State != application.AccessDeploymentAbandoned || value.OperatorID != request.OperatorID {
		t.Fatalf("abandon: %+v %v", value, err)
	}
	replayed, err := p.AbandonAccessDeployment(context.Background(), request)
	if err != nil || !replayed.RecordedAt.Equal(value.RecordedAt) {
		t.Fatal("lost response retry replaced the original decision", err)
	}
	input := application.SubsystemProvisioningInput{TenantID: request.Receipt.TenantID,
		ApplicationCode: request.Receipt.ApplicationCode, Environment: request.Receipt.Environment,
		ManifestChecksum: request.Receipt.ManifestChecksum, AccessReceipt: &request.Receipt}
	if err := target.Update(context.Background(), input); err == nil {
		t.Fatal("old fenced generation was deployed")
	}
	after, _ := os.ReadFile(path)
	if string(before) != string(after) {
		t.Fatal("manual termination changed runtime configuration")
	}
	input.AccessReceipt = nil
	if closed, err := target.accessDeploymentAlreadyClosed(input); err != nil || closed {
		t.Fatal("fence affected an unrelated legacy request", err)
	}
}

// TestAccessManualFenceRejectsDriftAndActiveRequest 验证活动部署和预检漂移不可终止。
// 参数：t为隔离目标锁及运行文件的测试上下文。
// 返回值：无，拒绝路径不能保存人工决定。
func TestAccessManualFenceRejectsDriftAndActiveRequest(t *testing.T) {
	p, target, path, request := accessRecoveryFixture(t)
	target.mutex.Lock()
	_, err := p.CheckAccessDeploymentRecovery(context.Background(), request)
	target.mutex.Unlock()
	if !errors.Is(err, application.ErrSubsystemAccessOperationConflict) {
		t.Fatal("live deployment did not block manual recovery", err)
	}
	evidence, err := p.CheckAccessDeploymentRecovery(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	request.EvidenceDigest = evidence.EvidenceDigest
	if err := os.WriteFile(path, []byte("OIDC_CLIENT_ID=changed-after-review\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := p.AbandonAccessDeployment(context.Background(), request); !errors.Is(err, application.ErrSubsystemAccessOperationConflict) {
		t.Fatal("reviewed facts drift was ignored", err)
	}
	if _, found, err := readAccessDeploymentOutcome(target.config.DeployRoot, request.Receipt); err != nil || found {
		t.Fatal("rejected manual decision created a fence", err)
	}
}
