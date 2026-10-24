package application

import "strings"

// SubsystemUpdateServiceCredentialRequirement 描述受控更新时需要确保的服务凭据用途。
// 与既有实现对齐：Suffix 用于生成规范名；Rotate 表示该用途在普通更新时也要换新密钥。
type SubsystemUpdateServiceCredentialRequirement struct {
	Purpose string
	Suffix  string
	// ClientName 是面向管理员的用途显示名（自既有实现复制）。
	ClientName string
	Rotate     bool
	Scopes     []string
}

// ShouldRotateSubsystemUpdateSecret 判断本次受控更新是否需要为既有服务凭据签发新密钥。
// 与既有实现逐条一致：ADOPT 与 RETRY 总是重建（此前可能已在 Agent 失败前签发过），
// 普通更新仅在需求标记 rotate 时重建。
func ShouldRotateSubsystemUpdateSecret(operation string, rotate bool) bool {
	if rotate {
		return true
	}
	switch strings.ToUpper(strings.TrimSpace(operation)) {
	case "ADOPT", "RETRY":
		return true
	default:
		return false
	}
}

// ShouldRepairSubsystemUpdateScopes 判断既有客户端范围是否需要按期望整体替换。
// 既有行为：范围不一致时先整体替换再签发密钥，避免"密钥已下发但调用 403"的半成功状态。
func ShouldRepairSubsystemUpdateScopes(clientScopes, desiredScopes []string) bool {
	if len(desiredScopes) == 0 {
		return false
	}
	return !sameStringSetValues(clientScopes, desiredScopes)
}

// PrimaryScope 返回单一范围（既有消费侧按单范围使用；多范围时返回空串，避免猜测语义）。
func (requirement SubsystemUpdateServiceCredentialRequirement) PrimaryScope() string {
	if len(requirement.Scopes) == 1 {
		return requirement.Scopes[0]
	}
	return ""
}

// SubsystemUpdateServiceCredentialScopes 返回该用途的完整最小权限集合（与既有实现一致）：
// 文件网关写入器需要上传、绑定与按文件 ID 回读三个范围；其它用途保持单一范围。
func SubsystemUpdateServiceCredentialScopes(requirement SubsystemUpdateServiceCredentialRequirement) []string {
	scopes := []string{requirement.PrimaryScope()}
	if requirement.Purpose == ServiceCredentialFileGatewayWrite {
		scopes = append(scopes, "platform:file:bind", "platform:file:download")
	}
	return scopes
}
