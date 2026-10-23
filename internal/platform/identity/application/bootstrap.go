package application

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
)

const (
	// BootstrapTenantCode 是受控初始化使用的迁移种子租户。
	BootstrapTenantCode = "default"
	// BootstrapApplicationCode 是迁移种子的平台应用。
	BootstrapApplicationCode = "platform"
	// BootstrapSuperAdminRoleCode 是只能由该初始化流程分配的内置角色。
	BootstrapSuperAdminRoleCode = "platform-super-admin"
	// BootstrapRootOrganizationCode 是迁移种子的组织，持有首位平台管理员的主任职。
	BootstrapRootOrganizationCode = "ROOT"
	// BootstrapSuperAdminPositionCode 是迁移种子的岗位，供首位平台管理员使用。用编码而非
	// 显示名，使引导契约不依赖本地化的岗位标签。
	BootstrapSuperAdminPositionCode = "POS-01J00000000000000000000400"
)

var (
	// ErrBootstrapAlreadyInitialized 表示受控的首位管理员流程已经完成。
	ErrBootstrapAlreadyInitialized = errors.New("first super administrator is already initialized")
	// ErrBootstrapUnavailable 表示必需的迁移种子平台记录不可用。
	ErrBootstrapUnavailable = errors.New("first super administrator bootstrap is unavailable")
)

// PasswordHasher 生成密码摘要及其校验元数据，实现细节与明文密码都不暴露给身份应用服务。
type PasswordHasher interface {
	Hash(password string) (digest []byte, metadata []byte, err error)
}

// BootstrapRepository 原子创建首个本地账号、其主组织任职与平台超级管理员角色绑定。
// 实现必须对 default 租户的请求做串行化。
type BootstrapRepository interface {
	BootstrapFirstSuperAdmin(context.Context, BootstrapWrite) (BootstrapResult, error)
}

// BootstrapInput 是一次性流程接受的仅有的操作员输入。
type BootstrapInput struct {
	DisplayName string
	AccountName string
	Password    string
}

// BootstrapWrite 是已校验、不含秘密的持久化命令；PasswordDigest 与 AlgorithmParams
// 不包含明文密码。
type BootstrapWrite struct {
	BootstrapID     string
	UserID          string
	AccountID       string
	CredentialID    string
	MembershipID    string
	RoleBindingID   string
	DisplayName     string
	AccountName     string
	PasswordDigest  []byte
	AlgorithmParams []byte
	InitializedAt   time.Time
}

// BootstrapResult 可安全返回给 HTTP 适配层；有意排除密码、密码哈希、算法元数据与
// 引导令牌值。
type BootstrapResult struct {
	BootstrapID   string
	TenantID      string
	TenantCode    string
	UserID        string
	DisplayName   string
	AccountID     string
	AccountName   string
	RoleID        string
	RoleCode      string
	RoleName      string
	InitializedAt time.Time
}

// BootstrapService 编排受控且仅一次的首位超级管理员初始化。
type BootstrapService struct {
	repository BootstrapRepository
	hasher     PasswordHasher
	ids        IDGenerator
	clock      Clock
}

// NewBootstrapService 构造首位超级管理员用例；任一依赖为 nil 时返回错误。
func NewBootstrapService(repository BootstrapRepository, hasher PasswordHasher, ids IDGenerator, clock Clock) (*BootstrapService, error) {
	if repository == nil || hasher == nil || ids == nil || clock == nil {
		return nil, errors.New("identity bootstrap service dependencies must not be nil")
	}
	return &BootstrapService{repository: repository, hasher: hasher, ids: ids, clock: clock}, nil
}

// InitializeFirstSuperAdmin 校验输入，对操作员密码只做一次哈希，然后把全有或全无的状态
// 转移交给 GORM 仓储。密码既不记日志也不返回。
func (service *BootstrapService) InitializeFirstSuperAdmin(ctx context.Context, input BootstrapInput) (BootstrapResult, error) {
	if err := validateBootstrapInput(input); err != nil {
		return BootstrapResult{}, err
	}

	digest, metadata, err := service.hasher.Hash(input.Password)
	if err != nil {
		return BootstrapResult{}, fmt.Errorf("hash first super administrator password: %w", err)
	}
	now := service.clock.Now().UTC()
	bootstrapID, err := service.ids.New(now)
	if err != nil {
		return BootstrapResult{}, fmt.Errorf("generate bootstrap state ID: %w", err)
	}
	userID, err := service.ids.New(now)
	if err != nil {
		return BootstrapResult{}, fmt.Errorf("generate bootstrap user ID: %w", err)
	}
	accountID, err := service.ids.New(now)
	if err != nil {
		return BootstrapResult{}, fmt.Errorf("generate bootstrap account ID: %w", err)
	}
	credentialID, err := service.ids.New(now)
	if err != nil {
		return BootstrapResult{}, fmt.Errorf("generate bootstrap credential ID: %w", err)
	}
	membershipID, err := service.ids.New(now)
	if err != nil {
		return BootstrapResult{}, fmt.Errorf("generate bootstrap membership ID: %w", err)
	}
	roleBindingID, err := service.ids.New(now)
	if err != nil {
		return BootstrapResult{}, fmt.Errorf("generate bootstrap role binding ID: %w", err)
	}

	return service.repository.BootstrapFirstSuperAdmin(ctx, BootstrapWrite{
		BootstrapID: bootstrapID, UserID: userID, AccountID: accountID, CredentialID: credentialID,
		MembershipID: membershipID, RoleBindingID: roleBindingID, DisplayName: strings.TrimSpace(input.DisplayName),
		AccountName: strings.TrimSpace(input.AccountName), PasswordDigest: digest,
		AlgorithmParams: metadata, InitializedAt: now,
	})
}

func validateBootstrapInput(input BootstrapInput) error {
	if !validName(input.DisplayName, 100) {
		return ErrValidation
	}
	if !validAccountName(input.AccountName) || validateStrongPassword(input.Password) != nil {
		return ErrValidation
	}
	return nil
}
