// Package domain 定义平台持有的外部客户身份投影，外部系统只负责提出收敛请求，
// iam_user 主体、账号和角色绑定的最终所有权仍属于平台。
package domain

import "time"

const (
	// IdentityPendingActivation 表示稳定 OIDC subject 已预留，但平台尚未完成凭据初始化
	// 或上游身份激活；外部系统不能把该状态等同于可登录。
	IdentityPendingActivation = "PENDING_ACTIVATION"
	// IdentityActive 表示身份已激活，可正常登录。
	IdentityActive = "ACTIVE"
	// IdentityDisabled 表示身份被停用，登录被阻断但映射保留。
	IdentityDisabled = "DISABLED"
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

// RoleResult 是 Portal 角色收敛操作返回的稳定响应结构。
type RoleResult struct {
	PlatformUserID  string `json:"platform_user_id"`
	ApplicationCode string `json:"application_code"`
	RoleCode        string `json:"role_code"`
	Status          string `json:"status"`
}

const (
	// BindingActive 表示外部客户绑定正在为 subject 授予所绑定应用的访问权；
	// BindingDisabled 冻结授权但保留持久映射历史，不做物理删除。
	BindingActive = "ACTIVE"
	// BindingDisabled 冻结授权但保留持久映射历史，不做物理删除。
	BindingDisabled = "DISABLED"
)

// BindingResult 是客户绑定操作返回的稳定且不含秘密的响应。
// 明文客户引用绝不回显给调用方。
type BindingResult struct {
	PlatformUserID  string `json:"platform_user_id"`
	ApplicationCode string `json:"application_code"`
	Status          string `json:"status"`
}

// CustomerBinding 是授权解析器读取的持久绑定记录。密文是唯一允许离开存储的形态；
// 解密只发生在应用服务中、且仅在即将签发受保护 claim 之前。
type CustomerBinding struct {
	ApplicationCode   string
	CustomerRefCipher []byte
	CustomerRefDigest []byte
	Status            string
}
