package application

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
)

const (
	loginTargetStatusDisabled  = "DISABLED"
	defaultLoginTargetFallback = "/"
)

// LoginTargetManagementItem 是单个已批准登录后落地目标的租户范围控制面表示。
// 刻意不包含任何 OAuth redirect_uri 数据，防止控制面与授权回调白名单混用。
type LoginTargetManagementItem struct {
	ID            string
	TenantID      string
	ApplicationID string
	EnvironmentID string
	TargetCode    string
	Name          string
	TargetURI     string
	Status        string
	Version       uint64
	CreatedAt     time.Time
	UpdatedAt     time.Time
}

// LoginTargetCreateInput 定义管理员登记新落地目标时唯一可变的字段集合。
// 应用/环境边界由受保护路由提供，仓储必须在插入前再次核验，不能信任入参自洽。
type LoginTargetCreateInput struct {
	TenantID      string
	OperatorID    string
	ApplicationID string
	EnvironmentID string
	TargetCode    string
	Name          string
	TargetURI     string
	Status        string
}

// LoginTargetUpdateInput 以乐观锁修改已登记目标。TargetCode 不可变：
// 调用方把它当作稳定的登录请求契约，变更会静默改写既有跳转入口。
type LoginTargetUpdateInput struct {
	TenantID      string
	OperatorID    string
	ApplicationID string
	EnvironmentID string
	LoginTargetID string
	Name          string
	TargetURI     string
	Status        string
	Version       uint64
}

// LoginTargetManagementRepository 持久化管理操作并核验完整的租户/应用/环境边界；
// 绝不允许从 URI 反推环境归属，否则跨租户探测会以合法身份通过。
type LoginTargetManagementRepository interface {
	EnsureLoginTargetBoundary(context.Context, string, string, string) error
	ListLoginTargets(context.Context, string, string, string, PageRequest) (PageResult[LoginTargetManagementItem], error)
	CreateLoginTarget(context.Context, LoginTargetCreateInput, string, time.Time) (LoginTargetManagementItem, error)
	GetLoginTarget(context.Context, string, string, string, string) (LoginTargetManagementItem, error)
	UpdateLoginTarget(context.Context, LoginTargetUpdateInput, time.Time) (LoginTargetManagementItem, error)
}

// LoginTargetManagementService 管理登录后的跨应用落点；运行时解析由独立只读服务完成。
// 登录目标不是 OAuth 授权码回调地址，两类白名单不能共用，否则会把门户导航能力扩大成凭据回传能力。
type LoginTargetManagementService struct {
	repository LoginTargetManagementRepository
	ids        IdentifierGenerator
	clock      Clock
}

// NewLoginTargetManagementService 构造登录目标管理用例；任一依赖缺失即拒绝构造，
// 管理面不允许降级为无审计时钟或无 ID 生成的半可用服务。
func NewLoginTargetManagementService(repository LoginTargetManagementRepository, ids IdentifierGenerator, clock Clock) (*LoginTargetManagementService, error) {
	if repository == nil || ids == nil || clock == nil {
		return nil, errors.New("application login target management dependencies must not be nil")
	}
	return &LoginTargetManagementService{repository: repository, ids: ids, clock: clock}, nil
}

// ListLoginTargets 在核验父级边界之后才列出目标，防止合法租户借列表接口
// 探测任意 application/environment 标识符的存在性。
func (service *LoginTargetManagementService) ListLoginTargets(ctx context.Context, tenantID, applicationID, environmentID string, query PageRequest) (PageResult[LoginTargetManagementItem], error) {
	tenantID, applicationID, environmentID = normalizeLoginTargetBoundary(tenantID, applicationID, environmentID)
	query = normalizePageRequest(query)
	if !validLoginTargetBoundary(tenantID, applicationID, environmentID) || !validLoginTargetStatusFilter(query.Status) {
		return PageResult[LoginTargetManagementItem]{}, ErrValidation
	}
	if err := service.repository.EnsureLoginTargetBoundary(ctx, tenantID, applicationID, environmentID); err != nil {
		return PageResult[LoginTargetManagementItem]{}, err
	}
	return service.repository.ListLoginTargets(ctx, tenantID, applicationID, environmentID, query)
}

// CreateLoginTarget 登记一个精确的、经管理员批准的外部落地 URI；
// URI 合法性与边界校验失败一律返回 ErrValidation，不做部分创建。
func (service *LoginTargetManagementService) CreateLoginTarget(ctx context.Context, input LoginTargetCreateInput) (LoginTargetManagementItem, error) {
	input = normalizeLoginTargetCreate(input)
	if !validLoginTargetCreate(input) {
		return LoginTargetManagementItem{}, ErrValidation
	}
	if err := service.repository.EnsureLoginTargetBoundary(ctx, input.TenantID, input.ApplicationID, input.EnvironmentID); err != nil {
		return LoginTargetManagementItem{}, err
	}

	now := service.clock.Now().UTC()
	identifier, err := service.ids.New(now)
	if err != nil {
		return LoginTargetManagementItem{}, fmt.Errorf("generate application login target ID: %w", err)
	}
	return service.repository.CreateLoginTarget(ctx, input, identifier, now)
}

// GetLoginTarget 在精确的租户/应用/环境边界内返回单个目标；
// 边界或标识符非法按 ErrValidation 拒绝，不泄露存在性信息。
func (service *LoginTargetManagementService) GetLoginTarget(ctx context.Context, tenantID, applicationID, environmentID, loginTargetID string) (LoginTargetManagementItem, error) {
	tenantID, applicationID, environmentID = normalizeLoginTargetBoundary(tenantID, applicationID, environmentID)
	loginTargetID = strings.TrimSpace(loginTargetID)
	if !validLoginTargetBoundary(tenantID, applicationID, environmentID) || !validLoginTargetIdentifier(loginTargetID) {
		return LoginTargetManagementItem{}, ErrValidation
	}
	return service.repository.GetLoginTarget(ctx, tenantID, applicationID, environmentID, loginTargetID)
}

// UpdateLoginTarget 执行带版本号的乐观锁更新，且不改变稳定的目标编码；
// 版本冲突由仓储返回，调用方不得静默重试覆盖他人修改。
func (service *LoginTargetManagementService) UpdateLoginTarget(ctx context.Context, input LoginTargetUpdateInput) (LoginTargetManagementItem, error) {
	input = normalizeLoginTargetUpdate(input)
	if !validLoginTargetUpdate(input) {
		return LoginTargetManagementItem{}, ErrValidation
	}
	return service.repository.UpdateLoginTarget(ctx, input, service.clock.Now().UTC())
}

// LoginTargetRedirectDecision 向共享登录流程给出安全的重定向结论：
// 未请求目标时保留平台本域回退；已请求但解析失败时调用方必须中断登录迁移，
// 而不是改跳调用方提供或环境默认的 URI。
type LoginTargetRedirectDecision struct {
	Location       string
	TargetResolved bool
}

// ResolvePostLoginRedirect 只从注册数据管控的内容解析请求目标。回退值仅接受
// 平台本域相对路径；外部地址、协议相对地址与畸形值一律折叠为 "/"。
// 目标解析失败会原样返回错误，由调用方终止登录完成流程并展示稳定的平台错误页，
// 绝不静默降级为跳转默认地址。
func (service *LoginTargetService) ResolvePostLoginRedirect(ctx context.Context, input *LoginTargetResolveInput, platformFallback string) (LoginTargetRedirectDecision, error) {
	fallback := SafePlatformLoginFallback(platformFallback)
	if input == nil {
		return LoginTargetRedirectDecision{Location: fallback}, nil
	}

	location, err := service.ResolveActiveTargetURI(ctx, *input)
	if err != nil {
		return LoginTargetRedirectDecision{Location: fallback}, err
	}
	return LoginTargetRedirectDecision{Location: location, TargetResolved: true}, nil
}

// SafePlatformLoginFallback 只接受平台本域相对路径作为异常回退；它不是通用 URL 校验器，
// 也不能用于放行应用登录目标，否则会绕过登记仓储和租户/环境边界。
func SafePlatformLoginFallback(candidate string) string {
	candidate = strings.TrimSpace(candidate)
	if candidate == "" {
		return defaultLoginTargetFallback
	}
	if !strings.HasPrefix(candidate, "/") || strings.HasPrefix(candidate, "//") || strings.Contains(candidate, "\\") {
		return defaultLoginTargetFallback
	}
	for _, character := range []byte(candidate) {
		if character < 0x21 || character > 0x7e {
			return defaultLoginTargetFallback
		}
	}
	return candidate
}

func normalizeLoginTargetBoundary(tenantID, applicationID, environmentID string) (string, string, string) {
	return strings.TrimSpace(tenantID), strings.TrimSpace(applicationID), strings.TrimSpace(environmentID)
}

func normalizeLoginTargetCreate(input LoginTargetCreateInput) LoginTargetCreateInput {
	input.TenantID, input.ApplicationID, input.EnvironmentID = normalizeLoginTargetBoundary(input.TenantID, input.ApplicationID, input.EnvironmentID)
	input.OperatorID = strings.TrimSpace(input.OperatorID)
	input.TargetCode = strings.TrimSpace(input.TargetCode)
	input.Name = strings.TrimSpace(input.Name)
	input.TargetURI = strings.TrimSpace(input.TargetURI)
	input.Status = strings.ToUpper(strings.TrimSpace(input.Status))
	return input
}

func normalizeLoginTargetUpdate(input LoginTargetUpdateInput) LoginTargetUpdateInput {
	input.TenantID, input.ApplicationID, input.EnvironmentID = normalizeLoginTargetBoundary(input.TenantID, input.ApplicationID, input.EnvironmentID)
	input.OperatorID = strings.TrimSpace(input.OperatorID)
	input.LoginTargetID = strings.TrimSpace(input.LoginTargetID)
	input.Name = strings.TrimSpace(input.Name)
	input.TargetURI = strings.TrimSpace(input.TargetURI)
	input.Status = strings.ToUpper(strings.TrimSpace(input.Status))
	return input
}

func validLoginTargetBoundary(tenantID, applicationID, environmentID string) bool {
	return validLoginTargetIdentifier(tenantID) && validLoginTargetIdentifier(applicationID) && validLoginTargetIdentifier(environmentID)
}

func validLoginTargetCreate(input LoginTargetCreateInput) bool {
	return validLoginTargetBoundary(input.TenantID, input.ApplicationID, input.EnvironmentID) && validLoginTargetIdentifier(input.OperatorID) &&
		validCode(input.TargetCode, 64) && validManagementText(input.Name, 128, false) &&
		validLoginTargetURI(input.TargetURI) && validLoginTargetStatus(input.Status)
}

func validLoginTargetUpdate(input LoginTargetUpdateInput) bool {
	return validLoginTargetBoundary(input.TenantID, input.ApplicationID, input.EnvironmentID) && validLoginTargetIdentifier(input.OperatorID) &&
		validLoginTargetIdentifier(input.LoginTargetID) && input.Version > 0 && validManagementText(input.Name, 128, false) &&
		validLoginTargetURI(input.TargetURI) && validLoginTargetStatus(input.Status)
}

func validLoginTargetStatus(value string) bool {
	return value == loginTargetStatusActive || value == loginTargetStatusDisabled
}

func validLoginTargetStatusFilter(value string) bool {
	return value == "" || validLoginTargetStatus(value)
}
