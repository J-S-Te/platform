package infrastructure

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/J-S-Te/Basic-Platform/internal/platform/applicationregistry/application"
)

// TestAccessCredentialRejectionRetainsTypedCause 验证真实凭据拒绝保留可核验类别。
// 参数：t为隔离运行文件的测试上下文。
// 返回值：无，普通错误不能代替结构化的部署前拒绝。
func TestAccessCredentialRejectionRetainsTypedCause(t *testing.T) {
	target, input := reviewedP1PortalFixture(t)
	if err := target.writeRuntimeConfiguration(input); err != nil {
		t.Fatal(err)
	}
	for index := range input.ServiceCredentials {
		input.ServiceCredentials[index].PlaintextSecret = ""
	}
	path := filepath.Join(target.config.DeployRoot, "runtime/portal-report-worker.env")
	if err := updateProductionSubsystemEnvironment(path,
		map[string]string{"PORTAL_REPORT_SOURCE_CLIENT_SECRET": ""}); err != nil {
		t.Fatal(err)
	}
	err := target.writeRuntimeFixedValues(input)
	if !errors.Is(err, application.ErrServiceCredentialRetryRequired) {
		t.Fatalf("known credential rejection lost its typed cause: %v", err)
	}
}

// TestAccessOutcomeRejectsOversizedTrailingContent 验证读取上限不会掩盖尾随载荷。
// 参数：t为隔离文件测试上下文。
// 返回值：无，超大文件必须失败关闭。
func TestAccessOutcomeRejectsOversizedTrailingContent(t *testing.T) {
	root := t.TempDir()
	receipt := application.SubsystemAccessDeploymentReceipt{TenantID: "tenant-1",
		OperationID: "operation-1", ApplicationCode: "settlement", Environment: "dev",
		Generation: 1, ManifestChecksum: "manifest-1"}
	value := application.SubsystemAccessDeploymentOutcome{Receipt: receipt,
		State: application.AccessDeploymentRejected, Code: application.AccessCredentialRetryRequired,
		RecordedAt: time.Now().UTC()}
	if err := writeAccessDeploymentOutcome(root, value); err != nil {
		t.Fatal(err)
	}
	path, _ := accessOutcomePath(root, receipt)
	raw, _ := json.Marshal(value)
	raw = append(raw, []byte(strings.Repeat(" ", 60000)+"{}")...)
	if err := os.WriteFile(path, raw, 0600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := readAccessDeploymentOutcome(root, receipt); err == nil {
		t.Fatal("limited reader hid oversized trailing payload")
	}
}

// TestAccessOutcomeRejectsWritableParent 验证私有结果不能建立在不可信可写父目录上。
// 参数：t为隔离结果目录的测试上下文。
// 返回值：无，外部可写父目录必须失败关闭。
func TestAccessOutcomeRejectsWritableParent(t *testing.T) {
	root := t.TempDir()
	receipt := application.SubsystemAccessDeploymentReceipt{TenantID: "tenant-1",
		OperationID: "operation-1", ApplicationCode: "settlement", Environment: "dev",
		Generation: 1, ManifestChecksum: "manifest-1"}
	value := application.SubsystemAccessDeploymentOutcome{Receipt: receipt,
		State: application.AccessDeploymentRejected, Code: application.AccessCredentialRetryRequired,
		RecordedAt: time.Now().UTC()}
	if err := writeAccessDeploymentOutcome(root, value); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(root, 0777); err != nil {
		t.Fatal(err)
	}
	if _, _, err := readAccessDeploymentOutcome(root, receipt); err == nil {
		t.Fatal("private outcome accepted through externally writable parent")
	}
}

// TestAccessOutcomeRejectsEmptyManualEvidence 验证没有文件及服务事实不能充当人工证明。
// 参数：t为隔离结果目录的上下文。
// 返回值：无，不完整证明不得写入持久终止栅栏。
func TestAccessOutcomeRejectsEmptyManualEvidence(t *testing.T) {
	root := t.TempDir()
	receipt := application.SubsystemAccessDeploymentReceipt{TenantID: "tenant-1",
		OperationID: "operation-1", ApplicationCode: "settlement", Environment: "dev",
		Generation: 1, ManifestChecksum: "manifest-1"}
	digest, _ := accessRecoveryEvidenceDigest(receipt, nil, nil)
	value := application.SubsystemAccessDeploymentOutcome{Receipt: receipt,
		State: application.AccessDeploymentAbandoned, Code: application.AccessOperatorAbandoned,
		OperatorID: "operator-1", Reason: "reviewed but missing all runtime evidence",
		EvidenceDigest: digest, RecordedAt: time.Now().UTC()}
	if err := writeAccessDeploymentOutcome(root, value); err == nil {
		t.Fatal("empty runtime evidence became a manual fence")
	}
}

// TestAccessLegacyReceiptHashIsStable 验证公开DTO变化不改变旧回执的持久文件身份。
// 参数：t为固定旧协议样本的测试上下文。
// 返回值：无，字段序列化或顺序漂移必须显式失败。
func TestAccessLegacyReceiptHashIsStable(t *testing.T) {
	receipt := application.SubsystemAccessDeploymentReceipt{TenantID: "tenant-1",
		OperationID: "operation-1", ApplicationCode: "settlement", Environment: "dev",
		Generation: 1, ManifestChecksum: "manifest-1"}
	path, err := accessReceiptPath(t.TempDir(), receipt)
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Base(path) != "b9a5e7c3c7d71c9df98f841f07f66bd8bcdd6a9cbecf96a38b4b4aca57bcd258.json" {
		t.Fatal("legacy durable receipt hash changed")
	}
}

// TestAccessSuccessCannotOverwriteRejectedOutcome 验证同代成功不能覆盖真实拒绝。
// 参数：t为隔离回执与结果文件的上下文。
// 返回值：无，同代结果互斥。
func TestAccessSuccessCannotOverwriteRejectedOutcome(t *testing.T) {
	root := t.TempDir()
	receipt := application.SubsystemAccessDeploymentReceipt{TenantID: "tenant-1",
		OperationID: "operation-1", ApplicationCode: "settlement", Environment: "dev",
		Generation: 1, ManifestChecksum: "manifest-1"}
	value := application.SubsystemAccessDeploymentOutcome{Receipt: receipt,
		State: application.AccessDeploymentRejected, Code: application.AccessCredentialRetryRequired,
		RecordedAt: time.Now().UTC()}
	if err := writeAccessDeploymentOutcome(root, value); err != nil {
		t.Fatal(err)
	}
	input := application.SubsystemProvisioningInput{TenantID: receipt.TenantID,
		ApplicationCode: receipt.ApplicationCode, Environment: receipt.Environment,
		ManifestChecksum: receipt.ManifestChecksum, AccessReceipt: &receipt}
	if err := writeAccessDeploymentReceipt(root, input); err == nil {
		t.Fatal("same generation became successful after durable rejection")
	}
}

// TestAccessOutcomeDirectorySyncFailure 验证父目录同步失败不能宣告持久化。
// 参数：t为真实私有目录及显式fsync故障注入的测试上下文。
// 返回值：无，任一父目录无法同步都必须使创建操作失败。
func TestAccessOutcomeDirectorySyncFailure(t *testing.T) {
	root := t.TempDir()
	directory := filepath.Join(root, "runtime", ".access-deployment-outcomes")
	unavailable := errors.New("simulated host directory fsync failure")
	err := privateAccessDirectoryWithSync(root, directory, true, func(path string) error {
		if path == filepath.Join(root, "runtime") {
			return unavailable
		}
		return nil
	})
	if !errors.Is(err, unavailable) {
		t.Fatalf("directory sync error discarded: %v", err)
	}
	if _, err := os.Stat(directory); err != nil {
		t.Fatal("fault did not reach the newly created result directory", err)
	}
}

// TestAccessOutcomeRejectsContradictoryReceipts 验证磁盘上并存的正负证明必须失败关闭。
// 参数：t为真实隔离文件系统的损坏状态测试上下文。
// 返回值：无，不允许择一回执把矛盾状态当成成功或失败。
func TestAccessOutcomeRejectsContradictoryReceipts(t *testing.T) {
	root := t.TempDir()
	receipt := application.SubsystemAccessDeploymentReceipt{TenantID: "tenant-1", OperationID: "operation-1",
		ApplicationCode: "settlement", Environment: "dev", Generation: 1, ManifestChecksum: "manifest-1"}
	value := application.SubsystemAccessDeploymentOutcome{Receipt: receipt, State: application.AccessDeploymentRejected,
		Code: application.AccessCredentialRetryRequired, RecordedAt: time.Now().UTC()}
	if err := writeAccessDeploymentOutcome(root, value); err != nil {
		t.Fatal(err)
	}
	path, err := accessReceiptPath(root, receipt)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(receipt)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(path, raw, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err = readAccessDeploymentReceipt(root, receipt); err == nil {
		t.Fatal("contradictory success accepted")
	}
	if _, _, err = readAccessDeploymentOutcome(root, receipt); err == nil {
		t.Fatal("contradictory rejection accepted")
	}
}

// TestAccessOutcomeRetryRequiresDurableDirectory 验证失败的目录同步不能由读取存在绕过。
// 参数：t为真实rename文件及显式目录fsync故障的测试上下文。
// 返回值：无，持续同步失败时不能确认，主机恢复后才可读取持久证明。
func TestAccessOutcomeRetryRequiresDurableDirectory(t *testing.T) {
	root := t.TempDir()
	receipt := application.SubsystemAccessDeploymentReceipt{TenantID: "tenant-1", OperationID: "operation-1",
		ApplicationCode: "settlement", Environment: "dev", Generation: 1, ManifestChecksum: "manifest-1"}
	value := application.SubsystemAccessDeploymentOutcome{Receipt: receipt, State: application.AccessDeploymentRejected,
		Code: application.AccessCredentialRetryRequired, RecordedAt: time.Now().UTC()}
	path, err := accessOutcomePath(root, receipt)
	if err != nil {
		t.Fatal(err)
	}
	if err = privateAccessDirectory(root, filepath.Dir(path), true); err != nil {
		t.Fatal(err)
	}
	unavailable := errors.New("simulated final directory fsync failure")
	failSync := func(string) error { return unavailable }
	if err = writePrivateAccessJSONWithSync(path, value, failSync); !errors.Is(err, unavailable) {
		t.Fatal("final sync error lost", err)
	}
	if _, err = os.Stat(path); err != nil {
		t.Fatal("test did not reach post-rename failure", err)
	}
	if _, found, err := readAccessDeploymentOutcomeWithSync(root, receipt, failSync); err == nil || found {
		t.Fatal("readable but unsynced proof promoted to durable success")
	}
	if _, found, err := readAccessDeploymentOutcome(root, receipt); err != nil || !found {
		t.Fatal("healthy host could not confirm proof", err)
	}
}
