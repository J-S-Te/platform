// Package domain 定义平台持有的外部客户身份投影，外部系统只负责提出收敛请求，
// iam_user 主体、账号和角色绑定的最终所有权仍属于平台。
package domain

import "time"

const (
	// IdentityPendingActivation 表示稳定 OIDC subject 已预留，但平台尚未完成凭据初始化
	// 或上游身份激活；外部系统不能把该状态等同于可登录。
	IdentityPendingActivation = "PENDING_ACTIVATION"
	IdentityActive            = "ACTIVE"
	IdentityDisabled          = "DISABLED"
)

// Identity 是不含秘密的外部客户身份投影。PlatformUserID（iam_user.id）是权威 OIDC subject；
// AccountNo 仅供运营定位记录，不能作为登录凭据或跨系统主键。
type Identity struct {
	ID             string
	TenantID       string
	PlatformUserID string
	AccountNo      string
	EmailDigest    []byte
	MobileDigest   []byte
	Status         string
	CreatedAt      time.Time
	UpdatedAt      time.Time
}

// RoleResult is the stable response returned by Portal role convergence operations.
type RoleResult struct {
	PlatformUserID  string `json:"platform_user_id"`
	ApplicationCode string `json:"application_code"`
	RoleCode        string `json:"role_code"`
	Status          string `json:"status"`
}
