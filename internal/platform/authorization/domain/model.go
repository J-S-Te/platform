// Package domain 定义不带基础设施标记的授权聚合。
package domain

import "time"

const (
	// StatusActive 表示聚合处于启用状态，参与授权判定。
	StatusActive = "ACTIVE"
	// StatusDisabled 表示聚合已停用，授权判定按无权处理。
	StatusDisabled = "DISABLED"
)

// Reference 以 API 安全形态标识一个关联聚合。
type Reference struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	Code string `json:"code,omitempty"`
}

// Resource 是应用所有的受保护资源。
type Resource struct {
	ID              string
	ApplicationCode string
	Code            string
	Name            string
	ResourceType    string
	Version         uint64
}

// Permission 是对资源的一个被允许动作。
type Permission struct {
	ID       string
	Code     string
	Name     string
	Resource Reference
	Action   string
	Version  uint64
}

// Role 是租户范围内的 ALLOW 权限集合。
type Role struct {
	ID            string
	ApplicationID string
	Code          string
	Name          string
	// RoleType 标识归属：PLATFORM、CUSTOM 或 APPLICATION。APPLICATION 角色由子系统
	// 目录同步而来，经通用平台角色管理 API 只读。
	RoleType    string
	Description *string
	Status      string
	BuiltIn     bool
	Permissions []Reference
	Version     uint64
}

// RoleBinding 把角色指派给某个作用域内的主体，同时描述“谁获得角色”和“该角色可在
// 哪个范围生效”。ScopeID 为空只对 TENANT 合法；ORG_UNIT/RESOURCE 必须由服务端验证
// 其租户归属后才能参与决策。
type RoleBinding struct {
	ID          string
	Role        Reference
	GrantOrigin string
	SubjectType string
	Subject     Reference
	ScopeType   string
	ScopeID     *string
	Status      string
	ExpiresAt   *time.Time
	Version     uint64
}

// Decision 是一次权限评估的结论；ReasonCode 说明放行或拒绝的来源。
type Decision struct {
	Allowed        bool
	PermissionCode string
	PolicyVersion  uint64
	ReasonCode     string
}

// AccessSource 描述授予某角色或权限的有效角色绑定路径。作用域只作为元数据返回：
// 调用方仍须结合具体资源或组织上下文，才能把带作用域的权限视为允许。
type AccessSource struct {
	BindingID   string
	SubjectType string
	Subject     Reference
	ScopeType   string
	ScopeID     *string
}

// EffectiveRole 是当前生效的角色，附带使其生效的全部绑定来源。
type EffectiveRole struct {
	Role    Reference
	Sources []AccessSource
}

// EffectivePermission 是当前生效的权限，附带其角色绑定来源。
type EffectivePermission struct {
	Permission Permission
	Sources    []AccessSource
}

// EffectiveAccessPreview 为选定账号暴露与授权检查同一套有效绑定路径。
// 它是管理端解释界面，不是客户端决策 API。
type EffectiveAccessPreview struct {
	User                      Reference
	Account                   Reference
	LoginEligible             bool
	PolicyVersion             uint64
	GeneratedAt               time.Time
	Roles                     []EffectiveRole
	Permissions               []EffectivePermission
	ExternalIdentityProviders []Reference
}

// RoleBindingImpactPreview 描述将立即获得拟议绑定的在册用户。用户样本由仓储封顶，
// TotalAffectedUsers 保持精确计数。
type RoleBindingImpactPreview struct {
	Role               Reference
	Permissions        []Reference
	SubjectType        string
	Subject            Reference
	ScopeType          string
	ScopeID            *string
	ExpiresAt          *time.Time
	TotalAffectedUsers int64
	Users              []Reference
	Truncated          bool
	GeneratedAt        time.Time
}
