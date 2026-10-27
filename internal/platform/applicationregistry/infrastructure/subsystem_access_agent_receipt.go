package infrastructure

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"

	"github.com/J-S-Te/Basic-Platform/internal/platform/applicationregistry/application"
)

type accessDeploymentReceiptReader interface {
	ReadAccessDeploymentCompletion(context.Context, application.SubsystemAccessDeploymentReceipt) (bool, error)
}

// accessReceiptPath 用途：将非秘密绑定散列为固定文件名，不接受调用方路径。
// 参数：root 为受控部署根，receipt 为租户、目标、代次及清单绑定。
// 返回值：返回回执文件路径；绑定不完整时返回错误。
func accessReceiptPath(root string, receipt application.SubsystemAccessDeploymentReceipt) (string, error) {
	if receipt.TenantID == "" || receipt.OperationID == "" || len(receipt.OperationID) > 64 ||
		receipt.ApplicationCode == "" || receipt.Environment == "" || receipt.Generation == 0 || receipt.ManifestChecksum == "" {
		return "", provisioningError("deployment receipt binding is incomplete")
	}
	raw, err := json.Marshal(receipt)
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256(raw)
	return filepath.Join(root, "runtime", ".access-deployment-receipts", hex.EncodeToString(digest[:])+".json"), nil
}

// writeAccessDeploymentReceipt 用途：仅在 Agent 完成全部部署核验后原子保存成功事实。
// 参数：root 为受控根目录，input 为实际执行且包含回执绑定的输入。
// 返回值：无回执的遗留请求保持原行为；写入或绑定校验失败返回错误。
func writeAccessDeploymentReceipt(root string, input application.SubsystemProvisioningInput) error {
	if input.AccessReceipt == nil {
		return nil
	}
	receipt := *input.AccessReceipt
	if receipt.TenantID != input.TenantID || receipt.ApplicationCode != input.ApplicationCode ||
		receipt.Environment != input.Environment || receipt.ManifestChecksum != input.ManifestChecksum {
		return provisioningError("deployment receipt target mismatch")
	}
	path, err := accessReceiptPath(root, receipt)
	if err != nil {
		return err
	}
	if err = os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	file, err := os.CreateTemp(filepath.Dir(path), ".receipt-*")
	if err != nil {
		return err
	}
	defer os.Remove(file.Name())
	defer file.Close()
	if err = file.Chmod(0600); err != nil {
		return err
	}
	if err = json.NewEncoder(file).Encode(receipt); err != nil {
		return err
	}
	if err = file.Sync(); err != nil {
		return err
	}
	if err = file.Close(); err != nil {
		return err
	}
	if err = os.Rename(file.Name(), path); err != nil {
		return err
	}
	directory, err := os.Open(filepath.Dir(path))
	if err != nil {
		return err
	}
	defer directory.Close()
	return directory.Sync()
}

// readAccessDeploymentReceipt 用途：读取并精确比对 Agent 已持久化的成功回执。
// 参数：root 为受控根，receipt 为数据库绑定的预期值。
// 返回值：不存在返回 false；格式或绑定异常返回错误，禁止据此宣告成功。
func readAccessDeploymentReceipt(root string, receipt application.SubsystemAccessDeploymentReceipt) (bool, error) {
	path, err := accessReceiptPath(root, receipt)
	if err != nil {
		return false, err
	}
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 {
		return false, provisioningError("deployment receipt file is unsafe")
	}
	file, err := os.Open(path)
	if err != nil {
		return false, err
	}
	defer file.Close()
	var actual application.SubsystemAccessDeploymentReceipt
	if err := json.NewDecoder(io.LimitReader(file, 4096)).Decode(&actual); err != nil {
		return false, err
	}
	if actual != receipt {
		return false, provisioningError("deployment receipt binding mismatch")
	}
	return true, nil
}

// ReadAccessDeploymentCompletion 用途：从生产 Agent 读取白名单目标的成功回执。
// 参数：ctx 为上下文，receipt 为平台原代次绑定。
// 返回值：返回是否已完成；租户或目标越界返回错误。
func (p *ProductionComposeSubsystemProvisioner) ReadAccessDeploymentCompletion(ctx context.Context, receipt application.SubsystemAccessDeploymentReceipt) (bool, error) {
	target, err := p.target(receipt.ApplicationCode, receipt.Environment)
	if err != nil {
		return false, err
	}
	if err := target.validateTenant(receipt.TenantID); err != nil {
		return false, err
	}
	return readAccessDeploymentReceipt(target.config.DeployRoot, receipt)
}

// ReadAccessDeploymentCompletion 用途：通过受限 Unix 协议只读查询部署回执。
// 参数：ctx 为上下文，receipt 为待核对的原代次绑定。
// 返回值：返回匹配的完成事实；旧 Agent 或通信错误不作为完成。
func (p *UnixSocketSubsystemProvisioner) ReadAccessDeploymentCompletion(ctx context.Context, receipt application.SubsystemAccessDeploymentReceipt) (bool, error) {
	if !p.enabled {
		return false, provisioningError("automatic subsystem deployment is disabled")
	}
	operationCtx, cancel := context.WithTimeout(ctx, p.timeout)
	defer cancel()
	connection, err := dialProvisioningSocket(operationCtx, p.socketPath)
	if err != nil {
		return false, provisioningError("deployment helper is unavailable")
	}
	defer connection.Close()
	if deadline, ok := operationCtx.Deadline(); ok {
		_ = connection.SetDeadline(deadline)
	}
	watchProvisioningCancellation(operationCtx, connection)
	request := subsystemProvisioningRequest{Version: subsystemProvisioningProtocolVersion, Action: "access-deployment-receipt", Receipt: &receipt}
	if err := json.NewEncoder(connection).Encode(request); err != nil {
		return false, err
	}
	var reply subsystemProvisioningReply
	if err := json.NewDecoder(io.LimitReader(connection, 64*1024)).Decode(&reply); err != nil {
		return false, err
	}
	if !reply.Success {
		return false, provisioningError("deployment receipt query unavailable")
	}
	return reply.DeploymentCompleted, nil
}

// validateAccessReceipt 用途：在外部变更开始前拒绝不完整或跨目标的回执绑定。
// 参数：input 为已授权的部署输入。
// 返回值：不含回执的遗留请求允许继续；绑定非法时返回错误。
func validateAccessReceipt(input application.SubsystemProvisioningInput) error {
	if input.AccessReceipt == nil {
		return nil
	}
	receipt := *input.AccessReceipt
	if receipt.TenantID != input.TenantID || receipt.ApplicationCode != input.ApplicationCode || receipt.Environment != input.Environment || receipt.ManifestChecksum != input.ManifestChecksum {
		return provisioningError("deployment receipt target mismatch")
	}
	_, err := accessReceiptPath("", receipt)
	return err
}

// ReadAccessDeploymentCompletion 用途：读取本地受控目标的部署完成回执。
// 参数：ctx 为上下文，receipt 为当前任务绑定。
// 返回值：返回匹配结果；目标不在本地白名单时返回错误。
func (p *LocalDockerSubsystemProvisioner) ReadAccessDeploymentCompletion(ctx context.Context, receipt application.SubsystemAccessDeploymentReceipt) (bool, error) {
	if !p.config.Enabled || receipt.Environment != "dev" || !subsystemDirectoryCodePattern.MatchString(receipt.ApplicationCode) {
		return false, provisioningError("local receipt target is unavailable")
	}
	if !isIntegratedSubsystem(receipt.ApplicationCode) && len(localLicenseServices[receipt.ApplicationCode]) == 0 {
		return false, provisioningError("local receipt target is unavailable")
	}
	return readAccessDeploymentReceipt(p.localPlatformRoot(), receipt)
}
