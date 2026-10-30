package infrastructure

import (
	"context"
	"encoding/json"
	"io"

	"github.com/J-S-Te/Basic-Platform/internal/platform/applicationregistry/application"
)

// ReadAccessDeploymentOutcome 读取受控目标的真实持久拒绝或终止结果。
// 参数：ctx为读取上下文，receipt为原任务绑定。
// 返回值：返回结果及存在性；跨租户、文件或目标异常返回错误。
func (p *ProductionComposeSubsystemProvisioner) ReadAccessDeploymentOutcome(ctx context.Context,
	receipt application.SubsystemAccessDeploymentReceipt) (application.SubsystemAccessDeploymentOutcome, bool, error) {
	target, err := p.target(receipt.ApplicationCode, receipt.Environment)
	if err != nil {
		return application.SubsystemAccessDeploymentOutcome{}, false, err
	}
	if err = target.validateTenant(receipt.TenantID); err != nil {
		return application.SubsystemAccessDeploymentOutcome{}, false, err
	}
	return readAccessDeploymentOutcome(target.config.DeployRoot, receipt)
}

// ReadAccessDeploymentOutcome 经既有受限socket查询Agent持久结果。
// 参数：ctx控制连接，receipt限定原操作、租户和代次。
// 返回值：旧端或通信异常返回错误，缺失不会伪造失败。
func (p *UnixSocketSubsystemProvisioner) ReadAccessDeploymentOutcome(ctx context.Context,
	receipt application.SubsystemAccessDeploymentReceipt) (application.SubsystemAccessDeploymentOutcome, bool, error) {
	var value application.SubsystemAccessDeploymentOutcome
	if !p.enabled {
		return value, false, provisioningError("automatic subsystem deployment is disabled")
	}
	ctx, cancel := context.WithTimeout(ctx, p.timeout)
	defer cancel()
	connection, err := dialProvisioningSocket(ctx, p.socketPath)
	if err != nil {
		return value, false, provisioningError("deployment outcome query unavailable")
	}
	defer connection.Close()
	if deadline, ok := ctx.Deadline(); ok {
		_ = connection.SetDeadline(deadline)
	}
	watchProvisioningCancellation(ctx, connection)
	request := subsystemProvisioningRequest{Version: subsystemProvisioningProtocolVersion,
		Action: "access-deployment-outcome", Receipt: &receipt}
	if err = json.NewEncoder(connection).Encode(request); err != nil {
		return value, false, err
	}
	var reply subsystemProvisioningReply
	if err = json.NewDecoder(io.LimitReader(connection, 64*1024)).Decode(&reply); err != nil {
		return value, false, err
	}
	if !reply.Success {
		return value, false, provisioningError("deployment outcome query unavailable")
	}
	if reply.DeploymentOutcome == nil {
		return value, false, nil
	}
	value = *reply.DeploymentOutcome
	return value, true, validateAccessOutcome(value, receipt)
}
