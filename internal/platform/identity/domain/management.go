package domain

import "time"

const (
	// StatusActive 表示实体可正常使用。
	StatusActive = "ACTIVE"
	// StatusDisabled 表示实体保留但不可用于新操作。
	StatusDisabled = "DISABLED"
	// AccountStatusLocked 由认证模块在密码账号锁定期间维护。
	AccountStatusLocked = "LOCKED"
	// MembershipPrimary 标识用户的主组织任职。
	MembershipPrimary = "PRIMARY"
	// MembershipSecondary 标识用户的非主组织任职。
	MembershipSecondary = "SECONDARY"
)

// User 是单个租户内的自然人。MobileCiphertext 绝不由 HTTP 适配器返回；
// 仅由应用服务用于构造脱敏 DTO。
type User struct {
	ID               string
	TenantID         string
	EmployeeNo       *string
	PMSPersonID      *string
	DisplayName      string
	Email            *string
	MobileCiphertext []byte
	Status           string
	ValidUntil       *time.Time
	Version          uint64
	CreatedAt        time.Time
	UpdatedAt        time.Time
}

// Account 是与用户（如有关联）绑定的登录主体。
type Account struct {
	ID       string
	TenantID string
	UserID   *string
	// User 是关联自然人的展示引用。UserID 仍保留用于兼容与写操作；管理侧
	// 读取使用该引用，避免客户端为每个账号单独发一次请求。
	User        *ReferenceName
	AccountName string
	AccountType string
	AuthSource  string
	// PasswordInitialized 仅为管理元数据：不暴露凭据哈希或失败状态。它让管理员
	// 能区分"无凭据的预留外部账号"与"可重置密码的账号"。
	PasswordInitialized bool
	Status              string
	LastLoginAt         *time.Time
	LockedUntil         *time.Time
	ValidUntil          *time.Time
	Version             uint64
	CreatedAt           time.Time
	UpdatedAt           time.Time
}

// OrgUnit 表示租户内组织树中的一个节点。
type OrgUnit struct {
	ID        string
	TenantID  string
	ParentID  *string
	Code      string
	Name      string
	OrgType   string
	Path      string
	Depth     uint
	SortOrder int
	Status    string
	Version   uint64
}

// Position 只隶属一个组织单元。
type Position struct {
	ID        string
	TenantID  string
	OrgUnitID string
	Code      string
	Name      string
	Status    string
	Version   uint64
}

// Membership 表示用户的组织任职，含主职与兼职。
type Membership struct {
	ID                   string
	TenantID             string
	User                 ReferenceName
	OrgUnit              ReferenceName
	Position             ReferenceName
	MembershipType       string
	EffectiveFrom        *time.Time
	EffectiveTo          *time.Time
	Status               string
	Version              uint64
	IsPrimary            bool
	InheritAuthorization bool
}
