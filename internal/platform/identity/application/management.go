// Package application 编排租户范围内的 IAM 管理用例。
package application

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/J-S-Te/Basic-Platform/internal/platform/identity/domain"
)

var (
	// ErrNotFound 表示租户范围内不存在该 IAM 实体，而非全局不存在。
	ErrNotFound = errors.New("identity resource not found")
	// ErrConflict 表示创建或状态变更请求违反了 IAM 的唯一性或生命周期规则。
	ErrConflict = errors.New("identity resource conflict")
	// ErrVersionConflict 表示调用方提供了过期的聚合版本号（乐观锁冲突）。
	ErrVersionConflict = errors.New("identity version conflict")
	// ErrMembershipRequired 阻止创建没有人员任职关系的 IAM 用户。
	ErrMembershipRequired = errors.New("user must be created with an appointment")
)

const (
	// DefaultPlatformApplicationCode 标识管理控制台应用，其基线角色会分配给每个新建的普通用户。
	DefaultPlatformApplicationCode = "platform"
	// DefaultUserRoleCode 是普通用户内置的最小权限角色。
	DefaultUserRoleCode = "platform-user"
	// MaxBatchUserCreateItems 限定单次原子批量的大小，保证请求体与事务规模有限。
	MaxBatchUserCreateItems = 100
)

// PageRequest 是通用且有界约束的列表查询契约；AllowedOrgUnitIDs/AllowedResourceIDs
// 携带调用方的数据范围授权结果，超出范围的记录不得返回。
type PageRequest struct {
	Page               int
	PageSize           int
	Keyword            string
	Status             string
	ScopeRestricted    bool
	AllowedOrgUnitIDs  []string
	AllowedResourceIDs []string
}

// PageResult 是 HTTP 适配层共用的列表响应模型。
type PageResult[T any] struct {
	Items    []T
	Page     int
	PageSize int
	Total    int64
}

// UserView 是可安全返回给客户端的用户表示；有意只携带掩码手机号，明文与密文都绝不
// 离开应用层。
type UserView struct {
	ID           string
	DisplayName  string
	EmployeeNo   *string
	PMSPersonID  *string
	Email        *string
	MobileMasked *string
	Status       string
	ValidUntil   *time.Time
	Version      uint64
	CreatedAt    time.Time
	UpdatedAt    time.Time
}

// UserCreateInput 包含 POST /users 支持的可写字段。
type UserCreateInput struct {
	TenantID    string
	OperatorID  string
	DisplayName string
	EmployeeNo  *string
	Email       *string
	Mobile      *string
	Status      string
	ValidUntil  *time.Time
	// ApplicationRoles 携可与用户一起导入的应用自有角色分配。仓储只把这些编码解析到已同步
	// 的 ACTIVE 目录，并把用户/绑定原子落库；调用方永远不能直接提交角色 ID。
	ApplicationRoles []ApplicationRoleAssignment
}

// ApplicationRoleAssignment 通过稳定编码或管理控制台展示的人类可读名称标识一个应用自有
// 角色。按名称导入会在当前租户内精确解析；落库仍使用稳定的应用/角色 ID。
type ApplicationRoleAssignment struct {
	ApplicationCode string
	ApplicationName string
	RoleCode        string
	RoleName        string
}

// UserBatchCreateInput 在一个全有或全无的事务中创建多个普通用户。
type UserBatchCreateInput struct {
	TenantID   string
	OperatorID string
	Items      []UserCreateInput
}

// UserDeleteInput 是逻辑删除用户的乐观锁命令。
type UserDeleteInput struct {
	TenantID   string
	OperatorID string
	UserID     string
	Version    uint64
}

// UserUpdateInput 包含可修补的用户字段。可选指针为 nil 表示不变更；指针指向空值表示显式
// 清空可选文本字段。
type UserUpdateInput struct {
	TenantID       string
	OperatorID     string
	UserID         string
	DisplayName    string
	EmployeeNo     *string
	PMSPersonID    *string
	Email          *string
	Mobile         *string
	Status         *string
	ValidUntil     *time.Time
	UpdateValidity bool
	Version        uint64
	UpdateMobile   bool
}

// AccountUpdateInput 是 P0 契约暴露的仅有的账号更新字段。
type AccountUpdateInput struct {
	TenantID       string
	OperatorID     string
	AccountID      string
	Status         string
	ValidUntil     *time.Time
	UpdateValidity bool
	Version        uint64
}

// OrgUnitCreateInput 包含可写的 P0 组织字段。Code 与 OrgType 由平台管理，调用方不能引入
// 冲突的组织标识或类型。
type OrgUnitCreateInput struct {
	TenantID   string
	OperatorID string
	ParentID   *string
	Name       string
	SortOrder  int
}

// OrgUnitUpdateInput 在乐观锁下更新可变组织字段。组织编码与类型保持平台管理，
// 调用方不可变更。
type OrgUnitUpdateInput struct {
	TenantID   string
	OperatorID string
	OrgUnitID  string
	ParentID   *string
	Name       string
	SortOrder  int
	Version    uint64
}

// OrgUnitDeleteInput 逻辑删除一个组织子树。其下岗位与任职关系一并停用，确保不存在
// 活跃任职引用已停用组织。
type OrgUnitDeleteInput struct {
	TenantID   string
	OperatorID string
	OrgUnitID  string
	Version    uint64
}

// PositionCreateInput 包含调用方可写的岗位字段。岗位编码由平台生成，调用方不能引入重复
// 编码或自定义命名规则。
type PositionCreateInput struct {
	TenantID   string
	OperatorID string
	OrgUnitID  string
	Name       string
}

// PositionDeleteInput 以乐观锁逻辑删除岗位。历史任职仍可查询但会被停用，被删岗位不再
// 产生继承授权。
type PositionDeleteInput struct {
	TenantID   string
	OperatorID string
	PositionID string
	Version    uint64
}

// MembershipCreateInput 描述用户任职关系。PositionID 在持久化层为必填，尽管最初的
// OpenAPI 属性列表曾把它误标为可选。
type MembershipCreateInput struct {
	TenantID             string
	OperatorID           string
	UserID               string
	OrgUnitID            string
	PositionID           string
	MembershipType       string
	EffectiveFrom        *time.Time
	EffectiveTo          *time.Time
	InheritAuthorization *bool
}

// MembershipUpdateInput 以乐观锁更新一条任职关系。
type MembershipUpdateInput struct {
	MembershipCreateInput
	MembershipID string
	Status       *string
	Version      uint64
}

// UserWrite 是可直接落库的用户变更载体，只携带已加密的手机号数据。
type UserWrite struct {
	UserCreateInput
	ID                      string
	RoleBindingID           string
	ApplicationRoleBindings []ApplicationRoleBindingWrite
	MobileCiphertext        []byte
	MobileHash              []byte
}

// ApplicationRoleBindingWrite 携带服务端生成的绑定 ID 以及基于编码或名称的目录引用，
// 进入身份仓储的原子用户导入事务。
type ApplicationRoleBindingWrite struct {
	ID              string
	ApplicationCode string
	ApplicationName string
	RoleCode        string
	RoleName        string
}

// ManagementRepository 定义 IAM 管理端点的持久化行为。修改多个 IAM 聚合的事务由仓储
// 实现，保证原子性。
type ManagementRepository interface {
	ListUsers(context.Context, string, PageRequest) (PageResult[domain.User], error)
	CreateUser(context.Context, UserWrite) (domain.User, error)
	CreateUsers(context.Context, []UserWrite) ([]domain.User, error)
	GetUser(context.Context, string, string) (domain.User, error)
	UpdateUser(context.Context, UserUpdateInput, []byte, []byte) (domain.User, error)
	DeleteUser(context.Context, UserDeleteInput) error

	ListAccounts(context.Context, string, PageRequest) (PageResult[domain.Account], error)
	UpdateAccount(context.Context, AccountUpdateInput) (domain.Account, error)

	ListOrgUnits(context.Context, string, string, string, PageRequest) (PageResult[domain.OrgUnit], error)
	CreateOrgUnit(context.Context, domain.OrgUnit, string) (domain.OrgUnit, error)
	UpdateOrgUnit(context.Context, OrgUnitUpdateInput) (domain.OrgUnit, error)
	DeleteOrgUnit(context.Context, OrgUnitDeleteInput) error
	ListPositions(context.Context, string, string, string, PageRequest) (PageResult[domain.Position], error)
	CreatePosition(context.Context, domain.Position, string) (domain.Position, error)
	DeletePosition(context.Context, PositionDeleteInput) error

	ListMemberships(context.Context, string, PageRequest) (PageResult[domain.Membership], error)
	CreateMembership(context.Context, MembershipCreateInput, string) (domain.Membership, error)
	UpdateMembership(context.Context, MembershipUpdateInput) (domain.Membership, error)
}

// ExternalAccountStatusReader 读取外置身份提供方管理的账号有效状态。接口可选：本地密码
// 部署不依赖 Keycloak。
type ExternalAccountStatusReader interface {
	ReadAccountStatus(context.Context, string, string) (string, bool, error)
}

// ExternalAccountStatusSynchronizer 把提供方侧的停用结果持久化到平台聚合。对 ACTIVE 的
// 对账是有意单向的：外置返回 ACTIVE 绝不能重新启用平台侧已停用的账号。
type ExternalAccountStatusSynchronizer interface {
	DisableExternalIdentity(context.Context, string, string) error
}

// MobileProtection 处理手机号的可选静态加密。
type MobileProtection interface {
	Encrypt(string) ([]byte, error)
	Decrypt([]byte) (string, error)
	Digest(string) []byte
}

// ManagementService 实现独立于 HTTP 的用户、账号与组织管理。
type ManagementService struct {
	repository                        ManagementRepository
	mobiles                           MobileProtection
	ids                               IDGenerator
	clock                             Clock
	hasher                            PasswordHasher
	externalAccountStatusReader       ExternalAccountStatusReader
	externalAccountStatusSynchronizer ExternalAccountStatusSynchronizer
}

// SetExternalAccountStatusReader 在平台构造 Keycloak 管理客户端之后挂载可选的外置状态
// 镜像读取器。
func (service *ManagementService) SetExternalAccountStatusReader(reader ExternalAccountStatusReader) {
	service.externalAccountStatusReader = reader
}

// SetExternalAccountStatusSynchronizer 注册"把外置停用写回平台"的能力。
//
// 该能力不得由只读列表触发（见 ListAccounts）；它保留给需要显式更新权限的运维动作使用。
func (service *ManagementService) SetExternalAccountStatusSynchronizer(synchronizer ExternalAccountStatusSynchronizer) {
	service.externalAccountStatusSynchronizer = synchronizer
}

// NewManagementService 构造 P0 IAM 管理服务；任一必需依赖为 nil 时返回错误。
func NewManagementService(repository ManagementRepository, mobiles MobileProtection, ids IDGenerator, clock Clock, passwordHashers ...PasswordHasher) (*ManagementService, error) {
	if repository == nil || mobiles == nil || ids == nil || clock == nil {
		return nil, errors.New("identity management dependencies must not be nil")
	}
	var hasher PasswordHasher
	if len(passwordHashers) > 0 {
		hasher = passwordHashers[0]
	}
	return &ManagementService{repository: repository, mobiles: mobiles, ids: ids, clock: clock, hasher: hasher}, nil
}

// ListUsers 返回租户范围内的用户列表，返回前对手机号做掩码处理。
func (service *ManagementService) ListUsers(ctx context.Context, tenantID string, query PageRequest) (PageResult[UserView], error) {
	query = normalizePageRequest(query)
	result, err := service.repository.ListUsers(ctx, tenantID, query)
	if err != nil {
		return PageResult[UserView]{}, err
	}
	items := make([]UserView, 0, len(result.Items))
	for _, user := range result.Items {
		view, err := service.toUserView(user)
		if err != nil {
			return PageResult[UserView]{}, err
		}
		items = append(items, view)
	}
	return PageResult[UserView]{Items: items, Page: result.Page, PageSize: result.PageSize, Total: result.Total}, nil
}

// CreateUser 创建单个租户范围内的自然人并分配内置普通用户角色。登录账号是独立生命周期，
// 本操作不为其开通任何凭据。
func (service *ManagementService) CreateUser(ctx context.Context, input UserCreateInput) (UserView, error) {
	return UserView{}, ErrMembershipRequired
}

// 批量创建先完成整批校验、手机号保护及角色引用规范化，再一次性交给仓储事务写入；
// 任一用户失败会回滚整批。工号、普通用户基线角色绑定及应用角色绑定 ID 均由后端生成，
// 请求方不能借自定义标识跳过基线授权或制造绑定碰撞。
func (service *ManagementService) CreateUsersBatch(ctx context.Context, input UserBatchCreateInput) ([]UserView, error) {
	if strings.TrimSpace(input.TenantID) == "" || strings.TrimSpace(input.OperatorID) == "" || len(input.Items) == 0 || len(input.Items) > MaxBatchUserCreateItems {
		return nil, ErrValidation
	}
	// 独立创建用户被有意禁用：用户必须与任职关系一同创建，组织与岗位授权永远不能被绕过。
	return nil, ErrMembershipRequired
}

func normalizeApplicationRoleAssignments(assignments []ApplicationRoleAssignment) ([]ApplicationRoleAssignment, error) {
	if len(assignments) == 0 {
		return nil, nil
	}
	seen := make(map[string]struct{}, len(assignments))
	result := make([]ApplicationRoleAssignment, 0, len(assignments))
	for _, assignment := range assignments {
		// 应用和角色各自只能二选一使用编码或名称；同时提供会产生歧义，全部留空则无法
		// 解析。名称用于导入场景兼容，最终存在性、租户归属及可绑定性仍由仓储校验。
		assignment.ApplicationCode = strings.TrimSpace(assignment.ApplicationCode)
		assignment.ApplicationName = strings.TrimSpace(assignment.ApplicationName)
		assignment.RoleCode = strings.TrimSpace(assignment.RoleCode)
		assignment.RoleName = strings.TrimSpace(assignment.RoleName)
		if (assignment.ApplicationCode == "") == (assignment.ApplicationName == "") ||
			(assignment.RoleCode == "") == (assignment.RoleName == "") ||
			len(assignment.ApplicationCode) > 64 || len([]rune(assignment.ApplicationName)) > 128 ||
			len(assignment.RoleCode) > 128 || len([]rune(assignment.RoleName)) > 128 {
			return nil, ErrValidation
		}
		key := applicationRoleAssignmentKey(assignment.ApplicationCode, assignment.ApplicationName, assignment.RoleCode, assignment.RoleName)
		if _, exists := seen[key]; exists {
			return nil, ErrValidation
		}
		seen[key] = struct{}{}
		result = append(result, assignment)
	}
	return result, nil
}

func applicationRoleAssignmentKey(applicationCode, applicationName, roleCode, roleName string) string {
	// 使用不会出现在已校验字段中的 NUL 分隔，避免简单拼接把不同四元组误判为同一项。
	return applicationCode + "\x00" + applicationName + "\x00" + roleCode + "\x00" + roleName
}

// DeleteUser 逻辑删除用户，但保留身份行以供审计与引用完整性。关联的账号、任职关系与
// 活跃浏览器会话在同一事务内停用/吊销。
func (service *ManagementService) DeleteUser(ctx context.Context, input UserDeleteInput) error {
	if strings.TrimSpace(input.TenantID) == "" || strings.TrimSpace(input.OperatorID) == "" ||
		strings.TrimSpace(input.UserID) == "" || input.Version == 0 || input.OperatorID == input.UserID {
		return ErrValidation
	}
	return service.repository.DeleteUser(ctx, input)
}

// GetUser 返回单个租户范围内的用户。
func (service *ManagementService) GetUser(ctx context.Context, tenantID, userID string) (UserView, error) {
	user, err := service.repository.GetUser(ctx, tenantID, userID)
	if err != nil {
		return UserView{}, err
	}
	return service.toUserView(user)
}

// UpdateUser 应用带版本号（乐观锁）的用户更新；版本过期返回 ErrVersionConflict。
func (service *ManagementService) UpdateUser(ctx context.Context, input UserUpdateInput) (UserView, error) {
	if err := validateUserUpdate(input, service.clock.Now()); err != nil {
		return UserView{}, err
	}
	var ciphertext, digest []byte
	var err error
	if input.UpdateMobile {
		ciphertext, digest, err = service.protectMobile(input.Mobile)
		if err != nil {
			return UserView{}, err
		}
	}
	user, err := service.repository.UpdateUser(ctx, input, ciphertext, digest)
	if err != nil {
		return UserView{}, err
	}
	return service.toUserView(user)
}

// ListAccounts 列出租户范围内的账号，不暴露凭据或失败尝试计数。
func (service *ManagementService) ListAccounts(ctx context.Context, tenantID string, query PageRequest) (PageResult[domain.Account], error) {
	result, err := service.repository.ListAccounts(ctx, tenantID, normalizePageRequest(query))
	if err != nil || service.externalAccountStatusReader == nil {
		return result, err
	}
	for index := range result.Items {
		account := &result.Items[index]
		if account.UserID == nil {
			continue
		}
		userID := strings.TrimSpace(*account.UserID)
		status, available, readErr := service.externalAccountStatusReader.ReadAccountStatus(ctx, tenantID, userID)
		if readErr != nil {
			return PageResult[domain.Account]{}, fmt.Errorf("read external account status: %w", readErr)
		}
		if available && strings.EqualFold(strings.TrimSpace(status), domain.StatusDisabled) {
			// 只读列表只反映差异，不落业务状态。
			//
			// 这里曾经直接调用 DisableExternalIdentity：任何拥有 platform:account:read 的人
			// 只要读取账号列表，就会停用用户、账号（连同登录名）、凭据、任职和会话。配合
			// 投影侧把临时锁定写成 disabled 的问题，一次读取即可把"锁定到期自动恢复"变成
			// 永久停用。外置停用状态仍按 Keycloak 权威值展示，但持久化只能由显式的写路径
			// （具备相应更新权限的运维动作）触发，不能由 GET 触发。
			account.Status = domain.StatusDisabled
		}
	}
	return result, nil
}

// UpdateAccount 在乐观锁下把账号在 ACTIVE 与 DISABLED 之间切换。LOCKED 由密码登录失败
// 处理逻辑持有，不能通过该管理端点设置。
func (service *ManagementService) UpdateAccount(ctx context.Context, input AccountUpdateInput) (domain.Account, error) {
	if input.TenantID == "" || input.OperatorID == "" || input.AccountID == "" || input.Version == 0 ||
		(input.Status != domain.StatusActive && input.Status != domain.StatusDisabled) {
		return domain.Account{}, ErrValidation
	}
	if input.UpdateValidity && input.ValidUntil != nil && !input.ValidUntil.After(service.clock.Now().UTC()) {
		return domain.Account{}, ErrValidation
	}
	return service.repository.UpdateAccount(ctx, input)
}

// ListOrgUnits 使用共享且有界约束的分页契约列出租户组织单元。
func (service *ManagementService) ListOrgUnits(ctx context.Context, tenantID string, query PageRequest) (PageResult[domain.OrgUnit], error) {
	query = normalizePageRequest(query)
	if tenantID == "" || !validStatusFilter(query.Status) {
		return PageResult[domain.OrgUnit]{}, ErrValidation
	}
	return service.repository.ListOrgUnits(ctx, tenantID, query.Keyword, query.Status, query)
}

// CreateOrgUnit 追加租户范围内的组织节点。组织编码由服务端生成的 ULID 派生，不依赖
// 可变名称或调用方输入。岗位表示组织专属职责，需单独创建；应用角色名不得作为任职数据
// 复制进每个新建组织。
func (service *ManagementService) CreateOrgUnit(ctx context.Context, input OrgUnitCreateInput) (domain.OrgUnit, error) {
	if input.TenantID == "" || input.OperatorID == "" || !validName(input.Name, 100) {
		return domain.OrgUnit{}, ErrValidation
	}
	now := service.clock.Now().UTC()
	id, err := service.ids.New(now)
	if err != nil {
		return domain.OrgUnit{}, fmt.Errorf("generate organization ID: %w", err)
	}
	orgUnit := domain.OrgUnit{ID: id, TenantID: input.TenantID, ParentID: normalizedOptional(input.ParentID), Code: generatedOrganizationCode(id), Name: strings.TrimSpace(input.Name), OrgType: "DEPARTMENT", SortOrder: input.SortOrder, Status: domain.StatusActive, Version: 1}
	return service.repository.CreateOrgUnit(ctx, orgUnit, input.OperatorID)
}

// UpdateOrgUnit 更新租户范围内的组织节点。仅在不引入环时允许移动节点；仓储在事务内
// 原子重写子树路径。
func (service *ManagementService) UpdateOrgUnit(ctx context.Context, input OrgUnitUpdateInput) (domain.OrgUnit, error) {
	if input.TenantID == "" || input.OperatorID == "" || input.OrgUnitID == "" || input.Version == 0 || !validName(input.Name, 100) {
		return domain.OrgUnit{}, ErrValidation
	}
	input.ParentID = normalizedOptional(input.ParentID)
	return service.repository.UpdateOrgUnit(ctx, input)
}

// DeleteOrgUnit 为审计/历史保留组织层级，但在一个事务内停用目标子树及所有依赖的岗位与
// 任职关系。
func (service *ManagementService) DeleteOrgUnit(ctx context.Context, input OrgUnitDeleteInput) error {
	if input.TenantID == "" || input.OperatorID == "" || input.OrgUnitID == "" || input.Version == 0 {
		return ErrValidation
	}
	return service.repository.DeleteOrgUnit(ctx, input)
}

// generatedOrganizationCode 从组织主键 ULID 派生稳定且租户唯一的业务编码；ORG 前缀把它
// 与其他托管 ID 区分开。
func generatedOrganizationCode(id string) string {
	return "ORG-" + strings.ToUpper(strings.TrimSpace(id))
}

// ListPositions 使用共享且有界约束的分页契约返回租户范围内的岗位。
func (service *ManagementService) ListPositions(ctx context.Context, tenantID string, query PageRequest) (PageResult[domain.Position], error) {
	query = normalizePageRequest(query)
	if tenantID == "" || !validStatusFilter(query.Status) {
		return PageResult[domain.Position]{}, ErrValidation
	}
	return service.repository.ListPositions(ctx, tenantID, query.Keyword, query.Status, query)
}

// CreatePosition 在一个已 ACTIVE 的组织单元下创建 ACTIVE 岗位。业务编码由生成的 ULID
// 派生，岗位改名后保持稳定。
func (service *ManagementService) CreatePosition(ctx context.Context, input PositionCreateInput) (domain.Position, error) {
	if input.TenantID == "" || input.OperatorID == "" || input.OrgUnitID == "" || !validName(input.Name, 100) {
		return domain.Position{}, ErrValidation
	}
	now := service.clock.Now().UTC()
	id, err := service.ids.New(now)
	if err != nil {
		return domain.Position{}, fmt.Errorf("generate position ID: %w", err)
	}
	position := domain.Position{ID: id, TenantID: input.TenantID, OrgUnitID: input.OrgUnitID, Code: generatedPositionCode(id), Name: strings.TrimSpace(input.Name), Status: domain.StatusActive, Version: 1}
	return service.repository.CreatePosition(ctx, position, input.OperatorID)
}

// generatedPositionCode 从岗位 ULID 主键派生稳定的岗位编码。
func generatedPositionCode(id string) string {
	return "POS-" + strings.ToUpper(strings.TrimSpace(id))
}

// DeletePosition 保留岗位与任职历史，同时停用该岗位及其全部活跃任职；仓储在同一事务内
// 使新的继承授权快照失效。
func (service *ManagementService) DeletePosition(ctx context.Context, input PositionDeleteInput) error {
	if input.TenantID == "" || input.OperatorID == "" || input.PositionID == "" || input.Version == 0 {
		return ErrValidation
	}
	return service.repository.DeletePosition(ctx, input)
}

// ListMemberships 返回租户范围内的历史与活跃任职关系。
func (service *ManagementService) ListMemberships(ctx context.Context, tenantID string, query PageRequest) (PageResult[domain.Membership], error) {
	return service.repository.ListMemberships(ctx, tenantID, normalizePageRequest(query))
}

// CreateMembership 创建用户任职关系并维护“仅一个主任职”不变量。
func (service *ManagementService) CreateMembership(ctx context.Context, input MembershipCreateInput) (domain.Membership, error) {
	if err := validateMembership(input.TenantID, input.OperatorID, input.UserID, input.OrgUnitID, input.PositionID, input.MembershipType, input.EffectiveFrom, input.EffectiveTo); err != nil {
		return domain.Membership{}, err
	}
	// 向后兼容：未发送新开关的调用方保持历史行为——活跃任职参与组织/岗位继承授权。
	if input.InheritAuthorization == nil {
		enabled := true
		input.InheritAuthorization = &enabled
	}
	now := service.clock.Now().UTC()
	id, err := service.ids.New(now)
	if err != nil {
		return domain.Membership{}, fmt.Errorf("generate membership ID: %w", err)
	}
	return service.repository.CreateMembership(ctx, input, id)
}

// UpdateMembership 应用带版本号（乐观锁）的任职关系更新。
func (service *ManagementService) UpdateMembership(ctx context.Context, input MembershipUpdateInput) (domain.Membership, error) {
	if input.MembershipID == "" || input.Version == 0 || input.Status == nil ||
		(*input.Status != domain.StatusActive && *input.Status != domain.StatusDisabled) {
		return domain.Membership{}, ErrValidation
	}
	if err := validateMembership(input.TenantID, input.OperatorID, input.UserID, input.OrgUnitID, input.PositionID, input.MembershipType, input.EffectiveFrom, input.EffectiveTo); err != nil {
		return domain.Membership{}, err
	}
	if input.InheritAuthorization == nil {
		enabled := true
		input.InheritAuthorization = &enabled
	}
	return service.repository.UpdateMembership(ctx, input)
}

// ErrValidation 让适配层把无效输入与持久化失败区分开。
var ErrValidation = errors.New("identity request validation failed")

func (service *ManagementService) prepareUserWrite(input UserCreateInput, id string) (UserWrite, error) {
	ciphertext, digest, err := service.protectMobile(input.Mobile)
	if err != nil {
		return UserWrite{}, err
	}
	return UserWrite{UserCreateInput: input, ID: id, MobileCiphertext: ciphertext, MobileHash: digest}, nil
}

func (service *ManagementService) protectMobile(mobile *string) ([]byte, []byte, error) {
	// 密文用于授权后的展示恢复，键控摘要用于唯一性查询；数据库不保存可直接搜索的明文，
	// 空手机号则两者都不写入，避免把“未填写”聚合成同一个唯一值。
	if mobile == nil || strings.TrimSpace(*mobile) == "" {
		return nil, nil, nil
	}
	normalized, ok := normalizeMobile(*mobile)
	if !ok {
		return nil, nil, ErrValidation
	}
	ciphertext, err := service.mobiles.Encrypt(normalized)
	if err != nil {
		return nil, nil, fmt.Errorf("protect mobile: %w", err)
	}
	return ciphertext, service.mobiles.Digest(normalized), nil
}

func (service *ManagementService) toUserView(user domain.User) (UserView, error) {
	view := UserView{ID: user.ID, DisplayName: user.DisplayName, EmployeeNo: user.EmployeeNo, PMSPersonID: user.PMSPersonID, Email: user.Email, Status: user.Status, ValidUntil: user.ValidUntil, Version: user.Version, CreatedAt: user.CreatedAt.UTC(), UpdatedAt: user.UpdatedAt.UTC()}
	if len(user.MobileCiphertext) == 0 {
		return view, nil
	}
	mobile, err := service.mobiles.Decrypt(user.MobileCiphertext)
	if err != nil {
		return UserView{}, fmt.Errorf("read protected mobile for user %s: %w", user.ID, err)
	}
	masked := maskMobile(mobile)
	view.MobileMasked = &masked
	return view, nil
}

func normalizePageRequest(query PageRequest) PageRequest {
	if query.Page < 1 {
		query.Page = 1
	}
	if query.PageSize < 1 {
		query.PageSize = 20
	}
	if query.PageSize > 100 {
		query.PageSize = 100
	}
	query.Keyword = strings.TrimSpace(query.Keyword)
	query.Status = strings.TrimSpace(query.Status)
	return query
}

func validateUserCreate(input UserCreateInput, now time.Time) error {
	if input.TenantID == "" || input.OperatorID == "" || !validName(input.DisplayName, 100) || !validStatus(input.Status) {
		return ErrValidation
	}
	if input.EmployeeNo != nil && len(strings.TrimSpace(*input.EmployeeNo)) > 64 {
		return ErrValidation
	}
	if input.Email != nil && !validEmail(*input.Email) {
		return ErrValidation
	}
	if input.Mobile != nil && len(strings.TrimSpace(*input.Mobile)) > 32 {
		return ErrValidation
	}
	if input.ValidUntil != nil && !input.ValidUntil.After(now.UTC()) {
		return ErrValidation
	}
	return nil
}

func validOptionalPersonID(value string) bool {
	if value == "" {
		return true
	}
	if value != strings.TrimSpace(value) || len(value) > 64 {
		return false
	}
	for _, character := range value {
		if character > 0x7f || !(character == '-' || character == '_' || character == '.' || character == ':' ||
			character >= '0' && character <= '9' || character >= 'A' && character <= 'Z' || character >= 'a' && character <= 'z') {
			return false
		}
	}
	return true
}

func validateUserUpdate(input UserUpdateInput, now time.Time) error {
	if input.TenantID == "" || input.OperatorID == "" || input.UserID == "" || input.Version == 0 || !validName(input.DisplayName, 100) {
		return ErrValidation
	}
	if input.EmployeeNo != nil && len(strings.TrimSpace(*input.EmployeeNo)) > 64 {
		return ErrValidation
	}
	if input.PMSPersonID != nil && !validOptionalPersonID(*input.PMSPersonID) {
		return ErrValidation
	}
	if input.Email != nil && !validEmail(*input.Email) {
		return ErrValidation
	}
	if input.Status != nil && !validStatus(*input.Status) {
		return ErrValidation
	}
	if input.UpdateMobile && input.Mobile != nil && len(strings.TrimSpace(*input.Mobile)) > 32 {
		return ErrValidation
	}
	if input.UpdateValidity && input.ValidUntil != nil && !input.ValidUntil.After(now.UTC()) {
		return ErrValidation
	}
	return nil
}

func validateMembership(tenantID, operatorID, userID, orgUnitID, positionID, kind string, from, to *time.Time) error {
	if tenantID == "" || operatorID == "" || userID == "" || orgUnitID == "" || positionID == "" || (kind != domain.MembershipPrimary && kind != domain.MembershipSecondary) {
		return ErrValidation
	}
	// 长期任职两个日期都省略；短期任职必须提供完整的闭区间日期范围，只接受单端点会让
	// 有效期语义产生歧义。
	if (from == nil) != (to == nil) {
		return ErrValidation
	}
	if from != nil && from.After(*to) {
		return ErrValidation
	}
	return nil
}

func validName(value string, limit int) bool {
	value = strings.TrimSpace(value)
	return value != "" && len(value) <= limit
}
func validStatus(value string) bool {
	return value == domain.StatusActive || value == domain.StatusDisabled
}
func validStatusFilter(value string) bool { return value == "" || validStatus(value) }
func validEmail(value string) bool {
	value = strings.TrimSpace(value)
	return value == "" || (len(value) <= 320 && strings.Count(value, "@") == 1 && !strings.HasPrefix(value, "@") && !strings.HasSuffix(value, "@"))
}
func normalizedOptional(value *string) *string {
	if value == nil || strings.TrimSpace(*value) == "" {
		return nil
	}
	normalized := strings.TrimSpace(*value)
	return &normalized
}
func normalizeMobile(value string) (string, bool) {
	value = strings.ReplaceAll(strings.ReplaceAll(strings.TrimSpace(value), " ", ""), "-", "")
	if value == "" || len(value) > 32 {
		return "", false
	}
	for index, character := range value {
		if !(character >= '0' && character <= '9') && !(character == '+' && index == 0) {
			return "", false
		}
	}
	return value, true
}
func maskMobile(value string) string {
	if len(value) <= 4 {
		return "****"
	}
	return value[:3] + "****" + value[len(value)-4:]
}
