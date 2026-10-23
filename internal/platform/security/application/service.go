// Package application 编排租户登录策略、失败计数、账号锁定和人工解锁用例。
package application

import (
	"context"
	"errors"
	"net"
	"strings"
	"time"

	"github.com/J-S-Te/Basic-Platform/internal/platform/security/domain"
)

var (
	// ErrNotFound 隐藏租户范围内不存在或对调用方不可见的资源，避免向调用方泄露资源存在性。
	ErrNotFound = errors.New("security resource not found")
	// ErrConflict 表示非法状态迁移或乐观锁版本冲突，调用方应重新读取后重试。
	ErrConflict = errors.New("security resource conflict")
	// ErrValidation 表示输入不完整或超出安全允许的范围，未触达存储层。
	ErrValidation = errors.New("security validation failed")
)

const (
	defaultMaxFailedAttempts         = 5
	defaultLockoutDurationSeconds    = 15 * 60
	defaultFailureResetWindowSeconds = 30 * 60
	defaultIdleTimeoutSeconds        = 30 * 60
)

// Clock 抽象时间源，使锁定窗口、失败计数过期等安全策略可被确定性测试。
type Clock interface {
	Now() time.Time
}

// SystemClock 是生产环境使用的 UTC 时钟实现。
type SystemClock struct{}

// Now 返回当前 UTC 时间，所有持久化时间戳一律以 UTC 落库。
func (SystemClock) Now() time.Time { return time.Now().UTC() }

// LoginFailureInput 携带一次密码失败中可信的账号与客户端信息；刻意不包含密码本身，
// 防止明文口令进入安全审计存储。
type LoginFailureInput struct {
	TenantID    string
	AccountID   string
	AccountName string
	IPAddress   net.IP
	UserAgent   string
}

// LoginFailureRecorder 供 identity 模块在认证失败后落一条失败记录，
// 而无需依赖安全模块的具体存储实现。
type LoginFailureRecorder interface {
	RecordFailedLogin(context.Context, LoginFailureInput) (LoginFailureResult, error)
}

// SessionIdleTimeoutProvider 向 identity 模块暴露租户级空闲超时，
// 避免其直接读取安全存储细节。
type SessionIdleTimeoutProvider interface {
	SessionIdleTimeout(context.Context, string) (time.Duration, error)
}

// LoginSecurityService 是 identity 认证流程依赖的最小安全能力集合。
type LoginSecurityService interface {
	LoginFailureRecorder
	SessionIdleTimeoutProvider
}

// LoginFailureResult 告知认证流程本次失败是否触发了账号锁定，供登录响应给出一致提示。
type LoginFailureResult struct {
	LockedUntil *time.Time
}

// LoginPolicyUpdateInput 是租户登录策略的整体替换载荷；Version 用于乐观锁并发控制。
type LoginPolicyUpdateInput struct {
	TenantID                  string
	OperatorID                string
	MaxFailedAttempts         uint
	LockoutDurationSeconds    uint
	FailureResetWindowSeconds uint
	IdleTimeoutSeconds        uint
	Version                   uint64
}

// PageRequest 承载锁定账号列表查询的分页与关键字过滤条件。
type PageRequest struct {
	Page     int
	PageSize int
	Keyword  string
}

// PageResult 是按页码分页、以租户为范围过滤的结果集。
type PageResult[T any] struct {
	Items    []T
	Page     int
	PageSize int
	Total    int64
}

// UnlockInput 标识一次由管理员发起的账号解锁操作，记录操作者以便审计追责。
type UnlockInput struct {
	TenantID   string
	AccountID  string
	OperatorID string
}

// Repository 持久化登录安全聚合。所有实现必须在 SQL 层强制租户过滤，
// 不得依赖调用方自行传参保证数据范围。
type Repository interface {
	GetLoginPolicy(context.Context, string) (domain.LoginPolicy, error)
	UpdateLoginPolicy(context.Context, LoginPolicyUpdateInput, time.Time) (domain.LoginPolicy, error)
	RecordFailedLogin(context.Context, LoginFailureInput, domain.LoginPolicy, time.Time) (LoginFailureResult, error)
	ListLockedAccounts(context.Context, string, PageRequest, time.Time) (PageResult[domain.LockedAccount], error)
	UnlockAccount(context.Context, UnlockInput, time.Time) (domain.LockedAccount, error)
}

// Service 在仓储操作前统一执行登录安全校验与输入规范化。
type Service struct {
	repository Repository
	clock      Clock
}

// NewService 创建安全应用服务；任一依赖为 nil 时失败关闭，拒绝构造半可用实例。
func NewService(repository Repository, clock Clock) (*Service, error) {
	if repository == nil || clock == nil {
		return nil, errors.New("security service dependencies must not be nil")
	}
	return &Service{repository: repository, clock: clock}, nil
}

// GetLoginPolicy 对尚未持久化自定义策略的租户返回文档化默认值。默认值只在应用层合成，
// 仓储的“未找到”仍可与真实数据库错误区分。
func (service *Service) GetLoginPolicy(ctx context.Context, tenantID string) (domain.LoginPolicy, error) {
	if strings.TrimSpace(tenantID) == "" {
		return domain.LoginPolicy{}, ErrValidation
	}
	policy, err := service.repository.GetLoginPolicy(ctx, tenantID)
	if errors.Is(err, ErrNotFound) {
		return defaultLoginPolicy(tenantID), nil
	}
	return policy, err
}

// UpdateLoginPolicy 校验并持久化管理员选择的租户登录策略，参数越界一律拒绝写入。
func (service *Service) UpdateLoginPolicy(ctx context.Context, input LoginPolicyUpdateInput) (domain.LoginPolicy, error) {
	if strings.TrimSpace(input.TenantID) == "" || strings.TrimSpace(input.OperatorID) == "" ||
		!validPolicy(input.MaxFailedAttempts, input.LockoutDurationSeconds, input.FailureResetWindowSeconds, input.IdleTimeoutSeconds) {
		return domain.LoginPolicy{}, ErrValidation
	}
	return service.repository.UpdateLoginPolicy(ctx, input, service.clock.Now().UTC())
}

// SessionIdleTimeout 提供租户实际空闲超时；identity 仓储必须据此在每次认证访问时判断会话，
// 不能只依赖 Cookie 或会话自身的绝对过期时间。
func (service *Service) SessionIdleTimeout(ctx context.Context, tenantID string) (time.Duration, error) {
	policy, err := service.GetLoginPolicy(ctx, tenantID)
	if err != nil {
		return 0, err
	}
	return time.Duration(policy.IdleTimeoutSeconds) * time.Second, nil
}

// RecordFailedLogin 仅记录已定位到真实账号后的密码失败；未知用户名不进入该接口，
// 从而避免用安全记录反向枚举账号，同时由仓储原子决定是否达到锁定阈值。
func (service *Service) RecordFailedLogin(ctx context.Context, input LoginFailureInput) (LoginFailureResult, error) {
	if strings.TrimSpace(input.TenantID) == "" || strings.TrimSpace(input.AccountID) == "" || strings.TrimSpace(input.AccountName) == "" {
		return LoginFailureResult{}, ErrValidation
	}
	policy, err := service.GetLoginPolicy(ctx, input.TenantID)
	if err != nil {
		return LoginFailureResult{}, err
	}
	now := service.clock.Now().UTC().Truncate(time.Millisecond)
	return service.repository.RecordFailedLogin(ctx, input, policy, now)
}

// ListLockedAccounts 返回当前仍处于锁定期、对租户安全管理员可见的账号列表。
func (service *Service) ListLockedAccounts(ctx context.Context, tenantID string, query PageRequest) (PageResult[domain.LockedAccount], error) {
	if strings.TrimSpace(tenantID) == "" {
		return PageResult[domain.LockedAccount]{}, ErrValidation
	}
	return service.repository.ListLockedAccounts(ctx, tenantID, normalizePage(query), service.clock.Now().UTC())
}

// UnlockAccount 清除锁定并逻辑关闭仍生效的失败尝试，但不会恢复锁定时已经撤销的会话，
// 用户必须重新登录建立新会话。
func (service *Service) UnlockAccount(ctx context.Context, input UnlockInput) (domain.LockedAccount, error) {
	if strings.TrimSpace(input.TenantID) == "" || strings.TrimSpace(input.AccountID) == "" || strings.TrimSpace(input.OperatorID) == "" {
		return domain.LockedAccount{}, ErrValidation
	}
	return service.repository.UnlockAccount(ctx, input, service.clock.Now().UTC())
}

func defaultLoginPolicy(tenantID string) domain.LoginPolicy {
	return domain.LoginPolicy{
		TenantID: tenantID, MaxFailedAttempts: defaultMaxFailedAttempts,
		LockoutDurationSeconds:    defaultLockoutDurationSeconds,
		FailureResetWindowSeconds: defaultFailureResetWindowSeconds,
		IdleTimeoutSeconds:        defaultIdleTimeoutSeconds,
		Version:                   1,
	}
}

func validPolicy(maxAttempts, lockoutSeconds, resetSeconds, idleTimeoutSeconds uint) bool {
	return maxAttempts >= 1 && maxAttempts <= 20 &&
		lockoutSeconds >= 60 && lockoutSeconds <= 24*60*60 &&
		resetSeconds >= 60 && resetSeconds <= 24*60*60 &&
		idleTimeoutSeconds >= 60 && idleTimeoutSeconds <= 24*60*60
}

func normalizePage(query PageRequest) PageRequest {
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
	return query
}
