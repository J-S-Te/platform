package application

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/J-S-Te/Basic-Platform/backend/internal/platform/identity/domain"
)

// EmployeeCreateInput is the atomic onboarding command used by POST /employees.  Account and
// membership are optional so the endpoint also supports pre-hiring a user record without
// accidentally creating an unbound account or appointment.
type EmployeeCreateInput struct {
	TenantID    string
	OperatorID  string
	DisplayName string
	Email       *string
	Mobile      *string
	Status      string
	Account     *EmployeeAccountCreateInput
	Membership  *EmployeeMembershipCreateInput
}

// EmployeeAccountCreateInput deliberately contains only local-account fields.  The new user ID
// is generated server-side and is never accepted from the browser.
type EmployeeAccountCreateInput struct {
	AccountName     string
	InitialPassword string
	ValidUntil      *time.Time
}

// EmployeeMembershipCreateInput describes the optional first appointment.  It is intentionally
// separate from a user record because a user can later have multiple appointments.
type EmployeeMembershipCreateInput struct {
	OrgUnitID            string
	PositionID           string
	MembershipType       string
	EffectiveFrom        *time.Time
	EffectiveTo          *time.Time
	InheritAuthorization *bool
}

// EmployeeOnboardingWrite contains prevalidated, secret-safe persistence values.  Password
// plaintext is hashed before this object crosses the repository boundary.
type EmployeeOnboardingWrite struct {
	User         UserWrite
	Account      *LocalAccountCreateWrite
	Membership   *MembershipCreateInput
	MembershipID string
	OccurredAt   time.Time
}

// EmployeeCreateResult is the client-safe result of a successful all-or-nothing onboarding.
type EmployeeCreateResult struct {
	User       UserView
	Account    *domain.Account
	Membership *domain.Membership
}

// EmployeeOnboardingRepository is optional on ManagementRepository to preserve existing
// management test doubles and the legacy POST /users contract.  Production GORM storage
// implements it using one database transaction.
type EmployeeOnboardingRepository interface {
	CreateEmployee(context.Context, EmployeeOnboardingWrite) (domain.User, *domain.Account, *domain.Membership, error)
}

// CreateEmployee 先在应用层完成校验、密码散列和全部 ID 生成，再把用户、平台普通用户
// 角色、可选本地账号和首次任职作为一个聚合交给仓储落库；任一环节失败都不能留下
// “已有用户但账号或任职缺失”的部分状态。
func (service *ManagementService) CreateEmployee(ctx context.Context, input EmployeeCreateInput) (EmployeeCreateResult, error) {
	if strings.TrimSpace(input.TenantID) == "" || strings.TrimSpace(input.OperatorID) == "" {
		return EmployeeCreateResult{}, ErrValidation
	}
	if input.Membership == nil {
		return EmployeeCreateResult{}, ErrMembershipRequired
	}

	userInput := UserCreateInput{
		TenantID: input.TenantID, OperatorID: input.OperatorID, DisplayName: input.DisplayName,
		Email: input.Email, Mobile: input.Mobile, Status: input.Status,
	}
	if err := validateUserCreate(userInput); err != nil {
		return EmployeeCreateResult{}, err
	}

	now := service.clock.Now().UTC()
	userID, err := service.ids.New(now)
	if err != nil {
		return EmployeeCreateResult{}, fmt.Errorf("generate employee user ID: %w", err)
	}
	roleBindingID, err := service.ids.New(now)
	if err != nil {
		return EmployeeCreateResult{}, fmt.Errorf("generate employee ordinary-user role binding ID: %w", err)
	}
	employeeNo := "EMP-" + strings.ToUpper(userID)
	userInput.EmployeeNo = &employeeNo
	userWrite, err := service.prepareUserWrite(userInput, userID)
	if err != nil {
		return EmployeeCreateResult{}, err
	}
	userWrite.RoleBindingID = roleBindingID

	write := EmployeeOnboardingWrite{User: userWrite, OccurredAt: now}
	if input.Account != nil {
		if service.hasher == nil {
			return EmployeeCreateResult{}, fmt.Errorf("employee account password hasher is unavailable")
		}
		accountInput := LocalAccountCreateInput{
			TenantID: input.TenantID, OperatorID: input.OperatorID, UserID: userID,
			AccountName: input.Account.AccountName, InitialPassword: input.Account.InitialPassword,
			ValidUntil: input.Account.ValidUntil,
		}
		if err := validateLocalAccountCreateInput(accountInput); err != nil {
			return EmployeeCreateResult{}, err
		}
		if accountInput.ValidUntil != nil && !accountInput.ValidUntil.After(now) {
			return EmployeeCreateResult{}, ErrValidation
		}
		digest, metadata, err := service.hasher.Hash(accountInput.InitialPassword)
		if err != nil {
			return EmployeeCreateResult{}, fmt.Errorf("hash employee initial password: %w", err)
		}
		accountID, err := service.ids.New(now)
		if err != nil {
			return EmployeeCreateResult{}, fmt.Errorf("generate employee account ID: %w", err)
		}
		credentialID, err := service.ids.New(now)
		if err != nil {
			return EmployeeCreateResult{}, fmt.Errorf("generate employee password credential ID: %w", err)
		}
		write.Account = &LocalAccountCreateWrite{
			AccountID: accountID, CredentialID: credentialID, TenantID: input.TenantID, UserID: userID,
			OperatorID: input.OperatorID, AccountName: strings.TrimSpace(input.Account.AccountName),
			PasswordDigest: digest, AlgorithmParams: metadata, OccurredAt: now, ValidUntil: normalizedFutureTime(input.Account.ValidUntil),
		}
	}

	if input.Membership != nil {
		// 首次任职默认参与组织/岗位授权继承；只有调用方显式传 false 才关闭，避免新增
		// 员工已有岗位却因遗漏可选字段而无法获得岗位模板授权。
		membership := MembershipCreateInput{
			TenantID: input.TenantID, OperatorID: input.OperatorID, UserID: userID,
			OrgUnitID: input.Membership.OrgUnitID, PositionID: input.Membership.PositionID,
			MembershipType: input.Membership.MembershipType, EffectiveFrom: input.Membership.EffectiveFrom,
			EffectiveTo: input.Membership.EffectiveTo, InheritAuthorization: input.Membership.InheritAuthorization,
		}
		if err := validateMembership(membership.TenantID, membership.OperatorID, membership.UserID, membership.OrgUnitID, membership.PositionID, membership.MembershipType, membership.EffectiveFrom, membership.EffectiveTo); err != nil {
			return EmployeeCreateResult{}, err
		}
		if membership.InheritAuthorization == nil {
			enabled := true
			membership.InheritAuthorization = &enabled
		}
		membershipID, err := service.ids.New(now)
		if err != nil {
			return EmployeeCreateResult{}, fmt.Errorf("generate employee membership ID: %w", err)
		}
		write.Membership = &membership
		write.MembershipID = membershipID
	}

	repository, ok := service.repository.(EmployeeOnboardingRepository)
	if !ok {
		return EmployeeCreateResult{}, fmt.Errorf("employee onboarding is not supported by this identity repository")
	}
	user, account, membership, err := repository.CreateEmployee(ctx, write)
	if err != nil {
		return EmployeeCreateResult{}, err
	}
	view, err := service.toUserView(user)
	if err != nil {
		return EmployeeCreateResult{}, err
	}
	return EmployeeCreateResult{User: view, Account: account, Membership: membership}, nil
}
