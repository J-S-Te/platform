package infrastructure

import (
	"strings"

	"github.com/J-S-Te/Basic-Platform/internal/platform/applicationregistry/application"
)

// accessCredentialOperation 用途：恢复尝试使用既有 RETRY 凭据重建规则而不改变持久操作动作。
// 参数：operation 为已领取的操作，AttemptCount 为仓库持久累加的领取次数。
// 返回值：首次尝试保留动作；后续尝试返回 RETRY，客户端归属与范围校验仍由凭据服务执行。
func accessCredentialOperation(operation application.SubsystemAccessOperation) string {
	if operation.AttemptCount > 1 {
		return string(application.SubsystemAccessActionRetry)
	}
	return strings.ToUpper(strings.TrimSpace(string(operation.Action)))
}
