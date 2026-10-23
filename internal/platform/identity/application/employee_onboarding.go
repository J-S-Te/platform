package application

import (
	"context"
	"crypto/rand"
	"fmt"
	"strings"
	"time"
	"unicode"

	"github.com/J-S-Te/Basic-Platform/internal/platform/identity/domain"
	"github.com/mozillazg/go-pinyin"
)

// EmployeeCreateInput 是 POST /employees 使用的原子入职命令。账号与任职可选，使该端点
// 也支持只预建用户档案，而不会意外创建无绑定的账号或任职。
type EmployeeCreateInput struct {
	TenantID         string
	OperatorID       string
	DisplayName      string
	Email            *string
	Mobile           *string
	Status           string
	ValidUntil       *time.Time
	Account          *EmployeeAccountCreateInput
	Membership       *EmployeeMembershipCreateInput
	ApplicationRoles []ApplicationRoleAssignment
}

// EmployeeBatchCreateInput 原子导入完整员工记录。组织与岗位引用由 HTTP 适配层从人类可读
// 名称解析。
type EmployeeBatchCreateInput struct {
	TenantID   string
	OperatorID string
	Items      []EmployeeCreateInput
}

// importedEmployeeInitialPassword 为批量导入的每个账号生成独立随机初始密码。
// 之前的共享常量默认密码会随源码/文档扩散，可被用于抢占尚未首次登录的账号；
// 每账号独立随机值仅通过导入响应返回给执行导入的管理员分发，且必须改密后可用。
func importedEmployeeInitialPassword() (string, error) {
	const capitals = "ABCDEFGHJKLMNPQRSTUVWXYZ"
	const letters = "abcdefghijkmnpqrstuvwxyz"
	const digits = "23456789"
	const symbols = "!@#$%^&*"
	const mixed = capitals + letters + digits + symbols
	entropy := make([]byte, 32)
	if _, err := rand.Read(entropy); err != nil {
		return "", fmt.Errorf("generate imported employee password entropy: %w", err)
	}
	password := []byte{
		capitals[int(entropy[0])%len(capitals)],
		letters[int(entropy[1])%len(letters)],
		digits[int(entropy[2])%len(digits)],
		symbols[int(entropy[3])%len(symbols)],
	}
	for _, value := range entropy[4:16] {
		password = append(password, mixed[int(value)%len(mixed)])
	}
	// Fisher-Yates 洗牌，避免四类保证字符停留在固定位置。
	for index := len(password) - 1; index > 0; index-- {
		swap := int(entropy[16+index]) % (index + 1)
		password[index], password[swap] = password[swap], password[index]
	}
	return string(password), nil
}

// EmployeeAccountCreateInput 有意只包含本地账号字段。新用户 ID 由服务端生成，
// 绝不接受来自浏览器。
type EmployeeAccountCreateInput struct {
	AccountName     string
	InitialPassword string
	ValidUntil      *time.Time
}

// EmployeeMembershipCreateInput 描述可选的首次任职。它与用户记录有意分离，因为一个用户
// 之后可以有多条任职。
type EmployeeMembershipCreateInput struct {
	OrgUnitID            string
	PositionID           string
	MembershipType       string
	EffectiveFrom        *time.Time
	EffectiveTo          *time.Time
	InheritAuthorization *bool
}

// EmployeeOnboardingWrite 携带预校验、不含秘密的持久化值。密码明文在该对象跨越仓储边界
// 之前已经散列。
type EmployeeOnboardingWrite struct {
	User         UserWrite
	Account      *LocalAccountCreateWrite
	Membership   *MembershipCreateInput
	MembershipID string
	OccurredAt   time.Time
}

// EmployeeCreateResult 是全有或全无入职成功后的客户端安全结果。
type EmployeeCreateResult struct {
	User       UserView
	Account    *domain.Account
	Membership *domain.Membership
}

// EmployeeOnboardingRepository 在 ManagementRepository 上是可选的，以保留既有管理测试替身
// 与旧 POST /users 契约；生产 GORM 存储用一个数据库事务实现它。
type EmployeeOnboardingRepository interface {
	CreateEmployee(context.Context, EmployeeOnboardingWrite) (domain.User, *domain.Account, *domain.Membership, error)
}

// EmployeeBatchOnboardingRepository 在一个数据库事务内批量入职员工。
type EmployeeBatchOnboardingRepository interface {
	CreateEmployees(context.Context, []EmployeeOnboardingWrite) ([]domain.User, error)
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
		ValidUntil: input.ValidUntil,
	}
	if err := validateUserCreate(userInput, service.clock.Now()); err != nil {
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

// EmployeeInitialCredential 是批量导入时为账号生成的随机初始密码。仅出现在导入响应中
// 供管理员分发；不落日志、不落库明文，账号首登必须改密。
type EmployeeInitialCredential struct {
	AccountName     string `json:"account_name"`
	InitialPassword string `json:"initial_password"`
}

// EmployeeBatchCreateResult 同时返回导入的用户视图与每账号随机初始凭据。
type EmployeeBatchCreateResult struct {
	Users              []UserView                  `json:"users"`
	InitialCredentials []EmployeeInitialCredential `json:"initial_credentials,omitempty"`
}

// CreateEmployeesBatch 在进入仓储事务之前完成每个员工的全部准备。引用、校验、加密或 ID
// 生成任一失败都会阻止任何部分写入。
func (service *ManagementService) CreateEmployeesBatch(ctx context.Context, input EmployeeBatchCreateInput) (EmployeeBatchCreateResult, error) {
	if strings.TrimSpace(input.TenantID) == "" || strings.TrimSpace(input.OperatorID) == "" || len(input.Items) == 0 || len(input.Items) > MaxBatchUserCreateItems {
		return EmployeeBatchCreateResult{}, ErrValidation
	}
	repository, ok := service.repository.(EmployeeBatchOnboardingRepository)
	if !ok {
		return EmployeeBatchCreateResult{}, fmt.Errorf("employee batch onboarding is not supported by this identity repository")
	}
	writes := make([]EmployeeOnboardingWrite, 0, len(input.Items))
	initialCredentials := make([]EmployeeInitialCredential, 0, len(input.Items))
	now := service.clock.Now().UTC()
	for _, item := range input.Items {
		if item.Membership == nil {
			return EmployeeBatchCreateResult{}, ErrMembershipRequired
		}
		userInput := UserCreateInput{TenantID: input.TenantID, OperatorID: input.OperatorID, DisplayName: item.DisplayName, Email: item.Email, Mobile: item.Mobile, Status: item.Status, ApplicationRoles: item.ApplicationRoles}
		if err := validateUserCreate(userInput, service.clock.Now()); err != nil {
			return EmployeeBatchCreateResult{}, err
		}
		userID, err := service.ids.New(now)
		if err != nil {
			return EmployeeBatchCreateResult{}, err
		}
		bindingID, err := service.ids.New(now)
		if err != nil {
			return EmployeeBatchCreateResult{}, err
		}
		userInput.EmployeeNo = stringPtr("EMP-" + strings.ToUpper(userID))
		userWrite, err := service.prepareUserWrite(userInput, userID)
		if err != nil {
			return EmployeeBatchCreateResult{}, err
		}
		userWrite.RoleBindingID = bindingID
		accountName := importedAccountName(item.DisplayName)
		if accountName == "" {
			return EmployeeBatchCreateResult{}, ErrValidation
		}
		if service.hasher == nil {
			return EmployeeBatchCreateResult{}, fmt.Errorf("employee account password hasher is unavailable")
		}
		initialPassword, err := importedEmployeeInitialPassword()
		if err != nil {
			return EmployeeBatchCreateResult{}, err
		}
		digest, metadata, err := service.hasher.Hash(initialPassword)
		if err != nil {
			return EmployeeBatchCreateResult{}, fmt.Errorf("hash employee initial password: %w", err)
		}
		accountID, err := service.ids.New(now)
		if err != nil {
			return EmployeeBatchCreateResult{}, err
		}
		credentialID, err := service.ids.New(now)
		if err != nil {
			return EmployeeBatchCreateResult{}, err
		}
		writeAccount := &EmployeeAccountCreateInput{AccountName: accountName, InitialPassword: initialPassword}
		if err := validateLocalAccountCreateInput(LocalAccountCreateInput{TenantID: input.TenantID, OperatorID: input.OperatorID, UserID: userID, AccountName: writeAccount.AccountName, InitialPassword: writeAccount.InitialPassword}); err != nil {
			return EmployeeBatchCreateResult{}, err
		}
		roles, err := normalizeApplicationRoleAssignments(item.ApplicationRoles)
		if err != nil {
			return EmployeeBatchCreateResult{}, err
		}
		for _, role := range roles {
			roleID, e := service.ids.New(now)
			if e != nil {
				return EmployeeBatchCreateResult{}, e
			}
			userWrite.ApplicationRoleBindings = append(userWrite.ApplicationRoleBindings, ApplicationRoleBindingWrite{ID: roleID, ApplicationCode: role.ApplicationCode, ApplicationName: role.ApplicationName, RoleCode: role.RoleCode, RoleName: role.RoleName})
		}
		membership := &MembershipCreateInput{TenantID: input.TenantID, OperatorID: input.OperatorID, UserID: userID, OrgUnitID: item.Membership.OrgUnitID, PositionID: item.Membership.PositionID, MembershipType: item.Membership.MembershipType, EffectiveFrom: item.Membership.EffectiveFrom, EffectiveTo: item.Membership.EffectiveTo, InheritAuthorization: item.Membership.InheritAuthorization}
		if err := validateMembership(membership.TenantID, membership.OperatorID, membership.UserID, membership.OrgUnitID, membership.PositionID, membership.MembershipType, membership.EffectiveFrom, membership.EffectiveTo); err != nil {
			return EmployeeBatchCreateResult{}, err
		}
		if membership.InheritAuthorization == nil {
			enabled := true
			membership.InheritAuthorization = &enabled
		}
		membershipID, err := service.ids.New(now)
		if err != nil {
			return EmployeeBatchCreateResult{}, err
		}
		writes = append(writes, EmployeeOnboardingWrite{User: userWrite, Account: &LocalAccountCreateWrite{AccountID: accountID, CredentialID: credentialID, TenantID: input.TenantID, UserID: userID, OperatorID: input.OperatorID, AccountName: accountName, PasswordDigest: digest, AlgorithmParams: metadata, OccurredAt: now}, Membership: membership, MembershipID: membershipID, OccurredAt: now})
		initialCredentials = append(initialCredentials, EmployeeInitialCredential{AccountName: accountName, InitialPassword: initialPassword})
	}
	users, err := repository.CreateEmployees(ctx, writes)
	if err != nil {
		return EmployeeBatchCreateResult{}, err
	}
	views := make([]UserView, 0, len(users))
	for _, user := range users {
		view, e := service.toUserView(user)
		if e != nil {
			return EmployeeBatchCreateResult{}, e
		}
		views = append(views, view)
	}
	return EmployeeBatchCreateResult{Users: views, InitialCredentials: initialCredentials}, nil
}

func stringPtr(value string) *string { return &value }

func importedAccountName(displayName string) string {
	name := strings.TrimSpace(displayName)
	if name == "" {
		return ""
	}
	args := pinyin.NewArgs()
	args.Style = pinyin.Normal
	parts := pinyin.Pinyin(name, args)
	var builder strings.Builder
	for _, syllables := range parts {
		if len(syllables) > 0 {
			builder.WriteString(syllables[0])
		}
	}
	value := strings.ToLower(builder.String())
	if value == "" {
		value = strings.ToLower(name)
	}
	var normalized strings.Builder
	for _, character := range value {
		if (character >= 'a' && character <= 'z') || (character >= '0' && character <= '9') || unicode.IsDigit(character) {
			normalized.WriteRune(character)
		}
	}
	return normalized.String()
}
