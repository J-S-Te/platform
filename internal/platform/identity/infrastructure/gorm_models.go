package infrastructure

import "time"

// 本文件的持久化模型有意与领域类型保持分离，只映射当前迁移所管的 schema；
// GORM 的 schema 演进 API 不得改动该 schema。

type tenantModel struct {
	ID     string `gorm:"column:id;primaryKey"`
	Code   string `gorm:"column:code"`
	Name   string `gorm:"column:name"`
	Status string `gorm:"column:status"`
}

// TableName 指定 GORM 行映射的租户表名。
func (tenantModel) TableName() string { return "iam_tenant" }

type userModel struct {
	ID                string     `gorm:"column:id;primaryKey"`
	TenantID          string     `gorm:"column:tenant_id"`
	EmployeeNo        *string    `gorm:"column:employee_no"`
	PMSPersonID       *string    `gorm:"column:pms_person_id"`
	DisplayName       string     `gorm:"column:display_name"`
	LegalName         *string    `gorm:"column:legal_name"`
	Email             *string    `gorm:"column:email"`
	MobileCiphertext  []byte     `gorm:"column:mobile_ciphertext"`
	MobileHash        []byte     `gorm:"column:mobile_hash"`
	AvatarFileID      *string    `gorm:"column:avatar_file_id"`
	PrimaryOrgID      *string    `gorm:"column:primary_org_id"`
	ManagerUserID     *string    `gorm:"column:manager_user_id"`
	EmploymentStatus  string     `gorm:"column:employment_status"`
	Status            string     `gorm:"column:status"`
	ValidUntil        *time.Time `gorm:"column:valid_until"`
	ExpiryProcessedAt *time.Time `gorm:"column:expiry_processed_at"`
	DeletedAt         *time.Time `gorm:"column:deleted_at"`
	DeletedBy         *string    `gorm:"column:deleted_by"`
	Version           uint64     `gorm:"column:version"`
	CreatedAt         time.Time  `gorm:"column:created_at"`
	CreatedBy         *string    `gorm:"column:created_by"`
	UpdatedAt         time.Time  `gorm:"column:updated_at"`
	UpdatedBy         *string    `gorm:"column:updated_by"`
}

// TableName 指定 GORM 行映射的用户表名。
func (userModel) TableName() string { return "iam_user" }

type accountModel struct {
	ID                  string     `gorm:"column:id;primaryKey"`
	TenantID            string     `gorm:"column:tenant_id"`
	UserID              *string    `gorm:"column:user_id"`
	Username            *string    `gorm:"column:username"`
	AccountType         string     `gorm:"column:account_type"`
	AuthSource          string     `gorm:"column:auth_source"`
	ExternalSubjectID   *string    `gorm:"column:external_subject_id"`
	PasswordInitialized bool       `gorm:"column:password_initialized;->"`
	LockedUntil         *time.Time `gorm:"column:locked_until"`
	LastLoginAt         *time.Time `gorm:"column:last_login_at"`
	ValidUntil          *time.Time `gorm:"column:valid_until"`
	ExpiryProcessedAt   *time.Time `gorm:"column:expiry_processed_at"`
	Status              string     `gorm:"column:status"`
	Version             uint64     `gorm:"column:version"`
	CreatedAt           time.Time  `gorm:"column:created_at"`
	CreatedBy           *string    `gorm:"column:created_by"`
	UpdatedAt           time.Time  `gorm:"column:updated_at"`
	UpdatedBy           *string    `gorm:"column:updated_by"`
}

// TableName 指定 GORM 行映射的账号表名。
func (accountModel) TableName() string { return "iam_account" }

type passwordCredentialModel struct {
	ID                string     `gorm:"column:id;primaryKey"`
	AccountID         string     `gorm:"column:account_id"`
	PasswordHash      []byte     `gorm:"column:password_hash"`
	HashAlgorithm     string     `gorm:"column:hash_algorithm"`
	AlgorithmParams   []byte     `gorm:"column:algorithm_params"`
	ExpiresAt         *time.Time `gorm:"column:expires_at"`
	MustChange        bool       `gorm:"column:must_change"`
	FailedAttempts    uint       `gorm:"column:failed_attempts"`
	LastFailedAt      *time.Time `gorm:"column:last_failed_at"`
	Status            string     `gorm:"column:status"`
	PasswordChangedAt time.Time  `gorm:"column:password_changed_at"`
	CreatedAt         time.Time  `gorm:"column:created_at"`
	UpdatedAt         time.Time  `gorm:"column:updated_at"`
}

// TableName 指定 GORM 行映射的密码凭据表名。
func (passwordCredentialModel) TableName() string { return "iam_password_credential" }

type sessionModel struct {
	ID                string     `gorm:"column:id;primaryKey"`
	TenantID          string     `gorm:"column:tenant_id"`
	AccountID         string     `gorm:"column:account_id"`
	IPAddress         []byte     `gorm:"column:ip_address"`
	UserAgent         *string    `gorm:"column:user_agent"`
	CreatedAt         time.Time  `gorm:"column:created_at"`
	LastSeenAt        time.Time  `gorm:"column:last_seen_at"`
	LastInteractiveAt time.Time  `gorm:"column:last_interactive_at"`
	ExpiresAt         time.Time  `gorm:"column:expires_at"`
	RevokedAt         *time.Time `gorm:"column:revoked_at"`
	RevokeReason      *string    `gorm:"column:revoke_reason"`
	Status            string     `gorm:"column:status"`
}

// TableName 指定 GORM 行映射的会话表名。
func (sessionModel) TableName() string { return "iam_session" }

type orgUnitModel struct {
	ID        string    `gorm:"column:id;primaryKey"`
	TenantID  string    `gorm:"column:tenant_id"`
	ParentID  *string   `gorm:"column:parent_id"`
	Code      string    `gorm:"column:code"`
	Name      string    `gorm:"column:name"`
	OrgType   string    `gorm:"column:org_type"`
	Path      string    `gorm:"column:path"`
	Depth     uint      `gorm:"column:depth"`
	SortOrder int       `gorm:"column:sort_order"`
	Status    string    `gorm:"column:status"`
	Version   uint64    `gorm:"column:version"`
	CreatedAt time.Time `gorm:"column:created_at"`
	CreatedBy *string   `gorm:"column:created_by"`
	UpdatedAt time.Time `gorm:"column:updated_at"`
	UpdatedBy *string   `gorm:"column:updated_by"`
}

// TableName 指定 GORM 行映射的组织单元表名。
func (orgUnitModel) TableName() string { return "iam_org_unit" }

type positionModel struct {
	ID        string    `gorm:"column:id;primaryKey"`
	TenantID  string    `gorm:"column:tenant_id"`
	OrgUnitID string    `gorm:"column:org_unit_id"`
	Code      string    `gorm:"column:code"`
	Name      string    `gorm:"column:name"`
	Status    string    `gorm:"column:status"`
	Version   uint64    `gorm:"column:version"`
	CreatedAt time.Time `gorm:"column:created_at"`
	CreatedBy *string   `gorm:"column:created_by"`
	UpdatedAt time.Time `gorm:"column:updated_at"`
	UpdatedBy *string   `gorm:"column:updated_by"`
}

// TableName 指定 GORM 行映射的岗位表名。
func (positionModel) TableName() string { return "iam_position" }

type membershipModel struct {
	ID                   string     `gorm:"column:id;primaryKey"`
	TenantID             string     `gorm:"column:tenant_id"`
	UserID               string     `gorm:"column:user_id"`
	OrgUnitID            string     `gorm:"column:org_unit_id"`
	PositionID           string     `gorm:"column:position_id"`
	MembershipType       string     `gorm:"column:membership_type"`
	IsPrimary            bool       `gorm:"column:is_primary"`
	InheritAuthorization bool       `gorm:"column:inherit_authorization"`
	ValidFrom            *time.Time `gorm:"column:valid_from"`
	ValidUntil           *time.Time `gorm:"column:valid_until"`
	Status               string     `gorm:"column:status"`
	Version              uint64     `gorm:"column:version"`
	CreatedAt            time.Time  `gorm:"column:created_at"`
	CreatedBy            *string    `gorm:"column:created_by"`
	UpdatedAt            time.Time  `gorm:"column:updated_at"`
	UpdatedBy            *string    `gorm:"column:updated_by"`
}

// TableName 指定 GORM 行映射的成员关系表名。
func (membershipModel) TableName() string { return "iam_membership" }

// 投影模型是只读的联表结果集，不代表聚合领域对象；由下方的显式映射函数转换。
type loginAccountProjection struct {
	TenantID           string     `gorm:"column:tenant_id"`
	TenantName         string     `gorm:"column:tenant_name"`
	TenantCode         string     `gorm:"column:tenant_code"`
	TenantStatus       string     `gorm:"column:tenant_status"`
	UserID             string     `gorm:"column:user_id"`
	UserName           string     `gorm:"column:user_name"`
	UserStatus         string     `gorm:"column:user_status"`
	AccountID          string     `gorm:"column:account_id"`
	AccountName        string     `gorm:"column:account_name"`
	AccountStatus      string     `gorm:"column:account_status"`
	LockedUntil        *time.Time `gorm:"column:locked_until"`
	PasswordHash       []byte     `gorm:"column:password_hash"`
	HashAlgorithm      string     `gorm:"column:hash_algorithm"`
	AlgorithmParams    []byte     `gorm:"column:algorithm_params"`
	CredentialStatus   string     `gorm:"column:credential_status"`
	CredentialExpiry   *time.Time `gorm:"column:credential_expiry"`
	MustChangePassword bool       `gorm:"column:must_change_password"`
}

type principalProjection struct {
	SessionID      string `gorm:"column:session_id"`
	LoginIPAddress []byte `gorm:"column:login_ip_address"`
	TenantID       string `gorm:"column:tenant_id"`
	TenantName     string `gorm:"column:tenant_name"`
	TenantCode     string `gorm:"column:tenant_code"`
	UserID         string `gorm:"column:user_id"`
	UserName       string `gorm:"column:user_name"`
	AccountID      string `gorm:"column:account_id"`
	AccountName    string `gorm:"column:account_name"`
}

type roleProjection struct {
	ID   string `gorm:"column:id"`
	Name string `gorm:"column:name"`
	Code string `gorm:"column:code"`
}

type permissionProjection struct {
	Code string `gorm:"column:code"`
}

type membershipProjection struct {
	ID                   string     `gorm:"column:id"`
	TenantID             string     `gorm:"column:tenant_id"`
	UserID               string     `gorm:"column:user_id"`
	UserName             string     `gorm:"column:user_name"`
	OrgUnitID            string     `gorm:"column:org_unit_id"`
	OrgUnitName          string     `gorm:"column:org_unit_name"`
	PositionID           string     `gorm:"column:position_id"`
	PositionName         string     `gorm:"column:position_name"`
	MembershipType       string     `gorm:"column:membership_type"`
	ValidFrom            *time.Time `gorm:"column:valid_from"`
	ValidUntil           *time.Time `gorm:"column:valid_until"`
	Status               string     `gorm:"column:status"`
	Version              uint64     `gorm:"column:version"`
	IsPrimary            bool       `gorm:"column:is_primary"`
	InheritAuthorization bool       `gorm:"column:inherit_authorization"`
}

// bootstrapStateModel 是迁移所管的一次性初始化标记。
type bootstrapStateModel struct {
	ID                       string    `gorm:"column:id;primaryKey"`
	TenantID                 string    `gorm:"column:tenant_id"`
	FirstSuperAdminUserID    string    `gorm:"column:first_super_admin_user_id"`
	FirstSuperAdminAccountID string    `gorm:"column:first_super_admin_account_id"`
	InitializedAt            time.Time `gorm:"column:initialized_at"`
}

// TableName 指定 GORM 行映射的初始化标记表名。
func (bootstrapStateModel) TableName() string { return "iam_bootstrap_state" }

// bootstrapApplicationModel 只投影引导边界所需的应用列。
type bootstrapApplicationModel struct {
	ID       string `gorm:"column:id;primaryKey"`
	TenantID string `gorm:"column:tenant_id"`
	Code     string `gorm:"column:code"`
	Name     string `gorm:"column:name"`
	Status   string `gorm:"column:status"`
}

// TableName 指定 GORM 行映射的应用登记表名。
func (bootstrapApplicationModel) TableName() string { return "platform_application" }

// bootstrapRoleModel 投影初始化流程解析出的内置角色。
type bootstrapRoleModel struct {
	ID            string `gorm:"column:id;primaryKey"`
	TenantID      string `gorm:"column:tenant_id"`
	ApplicationID string `gorm:"column:application_id"`
	Code          string `gorm:"column:code"`
	Name          string `gorm:"column:name"`
	RoleType      string `gorm:"column:role_type"`
	Status        string `gorm:"column:status"`
}

// TableName 指定 GORM 行映射的内置角色表名。
func (bootstrapRoleModel) TableName() string { return "authz_role" }

// bootstrapRoleBindingModel 映射不可变的超级管理员角色授予。
type bootstrapRoleBindingModel struct {
	ID            string     `gorm:"column:id;primaryKey"`
	TenantID      string     `gorm:"column:tenant_id"`
	ApplicationID string     `gorm:"column:application_id"`
	RoleID        string     `gorm:"column:role_id"`
	SubjectType   string     `gorm:"column:subject_type"`
	SubjectID     string     `gorm:"column:subject_id"`
	ScopeType     string     `gorm:"column:scope_type"`
	ScopeID       string     `gorm:"column:scope_id"`
	ValidFrom     *time.Time `gorm:"column:valid_from"`
	ValidUntil    *time.Time `gorm:"column:valid_until"`
	Status        string     `gorm:"column:status"`
	GrantOrigin   string     `gorm:"column:grant_origin"`
	OriginID      string     `gorm:"column:origin_id"`
	OriginItemID  string     `gorm:"column:origin_item_id"`
	Version       uint64     `gorm:"column:version"`
	CreatedAt     time.Time  `gorm:"column:created_at"`
	CreatedBy     *string    `gorm:"column:created_by"`
	UpdatedAt     time.Time  `gorm:"column:updated_at"`
	UpdatedBy     *string    `gorm:"column:updated_by"`
}

// TableName 指定 GORM 行映射的角色授予表名。
func (bootstrapRoleBindingModel) TableName() string { return "authz_role_binding" }

// identityPolicyRevisionModel 投影身份管理自动分配角色时更新的授权缓存修订号。它保留在本
// 限界上下文内，避免身份持久化耦合授权基础设施的实现类型。
type identityPolicyRevisionModel struct {
	TenantID      string    `gorm:"column:tenant_id;primaryKey"`
	ApplicationID string    `gorm:"column:application_id;primaryKey"`
	Revision      uint64    `gorm:"column:revision"`
	ChangedAt     time.Time `gorm:"column:changed_at"`
	ChangeReason  string    `gorm:"column:change_reason"`
}

// TableName 指定 GORM 行映射的授权策略修订表名。
func (identityPolicyRevisionModel) TableName() string { return "authz_policy_revision" }

// personnelChangeRequestModel 是人员异动的控制面持久记录。它有意不包含计算出的权限：
// 权限在预览/执行时由当前角色目录生成。
type personnelChangeRequestModel struct {
	ID                 string     `gorm:"column:id;primaryKey"`
	TenantID           string     `gorm:"column:tenant_id"`
	UserID             string     `gorm:"column:user_id"`
	SourceMembershipID *string    `gorm:"column:source_membership_id"`
	TargetOrgUnitID    *string    `gorm:"column:target_org_unit_id"`
	TargetPositionID   *string    `gorm:"column:target_position_id"`
	ChangeType         string     `gorm:"column:change_type"`
	Status             string     `gorm:"column:status"`
	Reason             string     `gorm:"column:reason"`
	ApprovalReference  *string    `gorm:"column:approval_reference"`
	EffectiveAt        time.Time  `gorm:"column:effective_at"`
	SubmittedBy        string     `gorm:"column:submitted_by"`
	ApprovedBy         *string    `gorm:"column:approved_by"`
	ApprovedAt         *time.Time `gorm:"column:approved_at"`
	ExecutedAt         *time.Time `gorm:"column:executed_at"`
	CancelledAt        *time.Time `gorm:"column:cancelled_at"`
	Version            uint64     `gorm:"column:version"`
	CreatedAt          time.Time  `gorm:"column:created_at"`
	UpdatedAt          time.Time  `gorm:"column:updated_at"`
}

// TableName 指定 GORM 行映射的人员异动请求表名。
func (personnelChangeRequestModel) TableName() string { return "iam_personnel_change_request" }
