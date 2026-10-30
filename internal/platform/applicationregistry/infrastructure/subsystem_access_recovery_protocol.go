package infrastructure

import (
	"context"
	"encoding/json"
	"io"
	"time"

	"github.com/J-S-Te/Basic-Platform/internal/platform/applicationregistry/application"
)

// exchangeAccessRecovery 仅发送固定人工恢复动作并附带当前受信发布摘要。
// 参数：ctx控制连接，action仅为内部常量，request包含原代次和核对决定。
// 返回值：原始有界应答；通信异常不能代表终止未发生，须查询原操作。
func (p *UnixSocketSubsystemProvisioner) exchangeAccessRecovery(ctx context.Context, action string,
	request application.SubsystemAccessAbandonRequest) (subsystemProvisioningReply, error) {
	var reply subsystemProvisioningReply
	if !p.enabled {
		return reply, application.ErrSubsystemAccessStageNotWired
	}
	if request.ReleaseDigest == "" {
		request.ReleaseDigest = p.releaseDigest
	}
	timeout := p.timeout
	if timeout <= 0 || timeout > 35*time.Second {
		timeout = 35 * time.Second
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	connection, err := dialProvisioningSocket(ctx, p.socketPath)
	if err != nil {
		return reply, provisioningError("deployment recovery connection unavailable")
	}
	defer connection.Close()
	if deadline, ok := ctx.Deadline(); ok {
		_ = connection.SetDeadline(deadline)
	}
	watchProvisioningCancellation(ctx, connection)
	payload := subsystemProvisioningRequest{Version: subsystemProvisioningProtocolVersion,
		Action: action, Recovery: &request}
	if err = json.NewEncoder(connection).Encode(payload); err != nil {
		return reply, err
	}
	if err = json.NewDecoder(io.LimitReader(connection, 64*1024)).Decode(&reply); err != nil {
		return reply, provisioningError("deployment recovery response unavailable; query original operation")
	}
	if !reply.Success {
		switch reply.ErrorCode {
		case "ACCESS_RECOVERY_CONFLICT":
			return reply, application.ErrSubsystemAccessOperationConflict
		case "ACCESS_RECOVERY_INVALID":
			return reply, application.ErrSubsystemAccessInvalidRequest
		default:
			return reply, provisioningError("deployment recovery unavailable; query original operation")
		}
	}
	return reply, nil
}

// CheckAccessDeploymentRecovery 经受限协议读取当前原代次的真实运行事实。
// 参数：ctx控制请求，request的目标由平台原代次生成，发布摘要由客户端补齐。
// 返回值：精确核验的事实；旧端、错绑定或通信失败时拒绝。
func (p *UnixSocketSubsystemProvisioner) CheckAccessDeploymentRecovery(ctx context.Context,
	request application.SubsystemAccessAbandonRequest) (application.SubsystemAccessRecoveryEvidence, error) {
	var value application.SubsystemAccessRecoveryEvidence
	reply, err := p.exchangeAccessRecovery(ctx, "access-deployment-recovery-check", request)
	if err != nil {
		return value, err
	}
	if reply.RecoveryEvidence == nil {
		return value, provisioningError("deployment recovery evidence missing")
	}
	value = *reply.RecoveryEvidence
	return value, validateRecoveryEvidence(value, request.Receipt)
}

// AbandonAccessDeployment 仅请求Agent保存已审查原代次的人工终止栅栏。
// 参数：request包含原绑定、摘要及可信操作者原因，ctx控制请求。
// 返回值：真实持久人工决定；错绑定或非人工终止结果失败关闭。
func (p *UnixSocketSubsystemProvisioner) AbandonAccessDeployment(ctx context.Context,
	request application.SubsystemAccessAbandonRequest) (application.SubsystemAccessDeploymentOutcome, error) {
	var value application.SubsystemAccessDeploymentOutcome
	reply, err := p.exchangeAccessRecovery(ctx, "access-deployment-abandon", request)
	if err != nil {
		return value, err
	}
	if reply.DeploymentOutcome == nil {
		return value, provisioningError("deployment abandonment evidence missing")
	}
	value = *reply.DeploymentOutcome
	if err = validateAccessOutcome(value, request.Receipt); err != nil {
		return value, err
	}
	if value.State != application.AccessDeploymentAbandoned || value.EvidenceDigest != request.EvidenceDigest {
		return value, application.ErrSubsystemAccessOperationConflict
	}
	return value, nil
}
