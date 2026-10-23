package application

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"math/big"
	"regexp"
	"strings"
	"time"
	"unicode"

	"github.com/J-S-Te/Basic-Platform/internal/platform/identity/domain"
)

// ErrExternalIdentityUnavailable 表示外部身份提供方的密码重置/登出暂时不可用；调用方应
// 视为可重试失败，而不是账号不存在。
var ErrExternalIdentityUnavailable = errors.New("external identity password reset unavailable")

const (
	accountNameMinLength = 3
	accountNameMaxLength = 64
	passwordMinRunes     = 8
	passwordMaxRunes     = 128
	generatedPasswordLen = 24
)

var accountNamePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)

// PasswordGenerator 为管理员生成一次性强密码，供线下交付。
type PasswordGenerator interface {
	Generate() (string, error)
}

// CryptoPasswordGenerator 使用 crypto/rand，产出值始终通过 validateStrongPassword。
// 本包绝不持久化或记录生成的密码。
type CryptoPasswordGenerator struct{}

// Generate 创建 24 位密码，包含大小写字母、数字与符号；字符集刻意排除了易混淆字符。
func (CryptoPasswordGenerator) Generate() (string, error) {
	const upper = "ABCDEFGHJKLMNPQRSTUVWXYZ"
	const lower = "abcdefghijkmnopqrstuvwxyz"
	const digits = "23456789"
	const symbols = "!@#$%^&*-_=+"
	const all = upper + lower + digits + symbols

	sets := []string{upper, lower, digits, symbols}
	password := make([]byte, 0, generatedPasswordLen)
	for _, set := range sets {
		character, err := randomCharacter(set)
		if err != nil {
			return "", err
		}
		password = append(password, character)
	}
	for len(password) < generatedPasswordLen {
		character, err := randomCharacter(all)
		if err != nil {
			return "", err
		}
		password = append(password, character)
	}
	for index := len(password) - 1; index > 0; index-- {
		randomIndex, err := rand.Int(rand.Reader, big.NewInt(int64(index+1)))
		if err != nil {
			return "", fmt.Errorf("randomize generated password: %w", err)
		}
		selected := int(randomIndex.Int64())
		password[index], password[selected] = password[selected], password[index]
	}
	return string(password), nil
}

func randomCharacter(characters string) (byte, error) {
	index, err := rand.Int(rand.Reader, big.NewInt(int64(len(characters))))
	if err != nil {
		return 0, fmt.Errorf("select random password character: %w", err)
	}
	return characters[index.Int64()], nil
}

// LocalPasswordCredential 携带校验现有密码所需的最小含秘密数据；必须留在应用层与基础设施
// 层内，不向传输层外泄。
type LocalPasswordCredential struct {
	TenantID         string
	AccountID        string
	AccountStatus    string
	CredentialStatus string
	PasswordHash     []byte
	HashAlgorithm    string
	AlgorithmParams  []byte
}

// LocalAccountCreateInput 是普通本地人类账号的公共用例输入。
type LocalAccountCreateInput struct {
	TenantID        string
	OperatorID      string
	UserID          string
	AccountName     string
	InitialPassword string
	ValidUntil      *time.Time
}

// PasswordInitializeInput 为缺失的本地密码凭据做一次性初始化；已有凭据时失败。
type PasswordInitializeInput struct {
	TenantID   string
	OperatorID string
	AccountID  string
	Password   string
	Version    uint64
}

// PasswordResetInput 请求服务端生成密码，明文只在管理员当前 HTTP 响应中返回、供线下交付。
type PasswordResetInput struct {
	TenantID   string
	OperatorID string
	AccountID  string
	Version    uint64
}

// PasswordChangeInput 变更已认证账号自身的本地密码。
type PasswordChangeInput struct {
	TenantID        string
	AccountID       string
	OperatorID      string
	CurrentPassword string
	NewPassword     string
}

// LocalAccountCreateWrite 是不含秘密的持久化命令；有意只携带密码摘要与校验元数据，
// 绝不携带明文密码。
type LocalAccountCreateWrite struct {
	AccountID       string
	CredentialID    string
	TenantID        string
	UserID          string
	OperatorID      string
	AccountName     string
	PasswordDigest  []byte
	AlgorithmParams []byte
	OccurredAt      time.Time
	ValidUntil      *time.Time
}

// PasswordWrite 原子更新本地凭据并吊销活跃浏览器会话。
type PasswordWrite struct {
	CredentialID    string
	TenantID        string
	AccountID       string
	OperatorID      string
	ExpectedVersion uint64
	ExpectedHash    []byte
	PasswordDigest  []byte
	AlgorithmParams []byte
	OccurredAt      time.Time
	RevokeReason    string
}

// AccountLifecycleRepository 持有本地账号与凭据变更的事务边界。实现不得存储或记录
// 明文密码。
type AccountIdentity struct{ AuthSource, ExternalSubjectID string }

// ExternalPasswordResetter 在外部身份提供方侧重置密码并登出该身份的全部会话。
type ExternalPasswordResetter interface {
	ResetPasswordAndLogout(context.Context, string, string) error
}

// AccountIdentityRepository 查询账号的认证来源与外部主体标识。
type AccountIdentityRepository interface {
	FindAccountIdentity(context.Context, string, string) (AccountIdentity, error)
}

// AccountLifecycleRepository 定义本地账号与密码生命周期所需的持久化操作。
type AccountLifecycleRepository interface {
	CreateLocalAccount(context.Context, LocalAccountCreateWrite) (domain.Account, error)
	InitializePassword(context.Context, PasswordWrite) (domain.Account, error)
	ResetPassword(context.Context, PasswordWrite) (domain.Account, error)
	FindLocalPasswordCredential(context.Context, string, string) (LocalPasswordCredential, error)
	ChangeOwnPassword(context.Context, PasswordWrite) error
}

// PasswordResetResult 携带为管理员生成的一次性明文密码。调用方只能在紧随的响应中渲染，
// 绝不能持久化、记录或写入审计。
type PasswordResetResult struct {
	AccountID         string
	TemporaryPassword string
}

// AccountLifecycleService 实现常规本地账号开通与密码生命周期。
type AccountLifecycleService struct {
	repository        AccountLifecycleRepository
	hasher            PasswordHasher
	verifier          PasswordVerifier
	passwordGenerator PasswordGenerator
	ids               IDGenerator
	clock             Clock
	externalResetter  ExternalPasswordResetter
}

// NewAccountLifecycleService 构造账号生命周期服务；任一必需依赖为 nil 或外部重置器多于
// 一个时返回错误。
func NewAccountLifecycleService(repository AccountLifecycleRepository, hasher PasswordHasher, verifier PasswordVerifier, passwordGenerator PasswordGenerator, ids IDGenerator, clock Clock, resetters ...ExternalPasswordResetter) (*AccountLifecycleService, error) {
	if repository == nil || hasher == nil || verifier == nil || passwordGenerator == nil || ids == nil || clock == nil {
		return nil, errors.New("account lifecycle service dependencies must not be nil")
	}
	var resetter ExternalPasswordResetter
	if len(resetters) > 1 {
		return nil, errors.New("at most one external password resetter")
	}
	if len(resetters) == 1 {
		resetter = resetters[0]
	}
	return &AccountLifecycleService{repository: repository, hasher: hasher, verifier: verifier, passwordGenerator: passwordGenerator, ids: ids, clock: clock, externalResetter: resetter}, nil
}

// CreateLocalAccount 原子创建 ACTIVE 的 HUMAN/LOCAL 账号与 Argon2id 凭据。
func (service *AccountLifecycleService) CreateLocalAccount(ctx context.Context, input LocalAccountCreateInput) (domain.Account, error) {
	if err := validateLocalAccountCreateInput(input); err != nil {
		return domain.Account{}, err
	}
	digest, metadata, err := service.hasher.Hash(input.InitialPassword)
	if err != nil {
		return domain.Account{}, fmt.Errorf("hash initial password: %w", err)
	}
	now := service.clock.Now().UTC()
	if input.ValidUntil != nil && !input.ValidUntil.After(now) {
		return domain.Account{}, ErrValidation
	}
	accountID, err := service.ids.New(now)
	if err != nil {
		return domain.Account{}, fmt.Errorf("generate account ID: %w", err)
	}
	credentialID, err := service.ids.New(now)
	if err != nil {
		return domain.Account{}, fmt.Errorf("generate password credential ID: %w", err)
	}
	return service.repository.CreateLocalAccount(ctx, LocalAccountCreateWrite{
		AccountID: accountID, CredentialID: credentialID, TenantID: strings.TrimSpace(input.TenantID), UserID: strings.TrimSpace(input.UserID), OperatorID: strings.TrimSpace(input.OperatorID), AccountName: strings.TrimSpace(input.AccountName), PasswordDigest: digest, AlgorithmParams: metadata, OccurredAt: now, ValidUntil: normalizedFutureTime(input.ValidUntil),
	})
}

// InitializePassword 仅在该账号尚无凭据时设置强本地密码。
func (service *AccountLifecycleService) InitializePassword(ctx context.Context, input PasswordInitializeInput) (domain.Account, error) {
	if err := validatePasswordInitializeInput(input); err != nil {
		return domain.Account{}, err
	}
	now := service.clock.Now().UTC()
	credentialID, err := service.ids.New(now)
	if err != nil {
		return domain.Account{}, fmt.Errorf("generate password credential ID: %w", err)
	}
	return service.writeAdministratorPassword(ctx, input.TenantID, input.AccountID, input.OperatorID, input.Version, input.Password, credentialID, "PASSWORD_INITIALIZED")
}

// ResetPassword 生成只在本次响应中返回的强密码。已有本地凭据时替换摘要；外部身份预留
// 的 HUMAN/LOCAL 账号尚无凭据时则初始化首个凭据。明文始终不会跨越仓储边界。
func (service *AccountLifecycleService) ResetPassword(ctx context.Context, input PasswordResetInput) (PasswordResetResult, error) {
	if err := validatePasswordResetInput(input); err != nil {
		return PasswordResetResult{}, err
	}
	var externalIdentity *AccountIdentity
	if identityRepo, ok := service.repository.(AccountIdentityRepository); ok {
		identity, identityErr := identityRepo.FindAccountIdentity(ctx, input.TenantID, input.AccountID)
		if identityErr != nil {
			return PasswordResetResult{}, identityErr
		}
		if strings.EqualFold(identity.AuthSource, "KEYCLOAK") || strings.EqualFold(identity.AuthSource, "FEDERATED") {
			externalIdentity = &identity
		}
	}
	password, err := service.passwordGenerator.Generate()
	if err != nil {
		return PasswordResetResult{}, fmt.Errorf("generate reset password: %w", err)
	}
	if err := validateStrongPassword(password); err != nil {
		return PasswordResetResult{}, fmt.Errorf("generated password violates policy: %w", err)
	}
	if externalIdentity != nil {
		if service.externalResetter == nil || strings.TrimSpace(externalIdentity.ExternalSubjectID) == "" {
			return PasswordResetResult{}, ErrConflict
		}
		if err := service.externalResetter.ResetPasswordAndLogout(ctx, externalIdentity.ExternalSubjectID, password); err != nil {
			return PasswordResetResult{}, fmt.Errorf("%w: %v", ErrExternalIdentityUnavailable, err)
		}
		return PasswordResetResult{AccountID: strings.TrimSpace(input.AccountID), TemporaryPassword: password}, nil
	}
	if _, err = service.writeAdministratorPassword(ctx, input.TenantID, input.AccountID, input.OperatorID, input.Version, password, "", "ADMIN_PASSWORD_RESET"); err != nil {
		if !errors.Is(err, ErrConflict) {
			return PasswordResetResult{}, err
		}
		// 外部身份源可能预留 HUMAN/LOCAL 账号但不写密码；普通重置返回冲突时，复用同一份
		// 服务端强密码完成首次初始化，仍保持管理界面“一次生成、线下交付”的语义。
		credentialID, idErr := service.ids.New(service.clock.Now().UTC())
		if idErr != nil {
			return PasswordResetResult{}, fmt.Errorf("generate initialized password credential ID: %w", idErr)
		}
		if _, initErr := service.writeAdministratorPassword(ctx, input.TenantID, input.AccountID, input.OperatorID, input.Version, password, credentialID, "ADMIN_PASSWORD_RESET"); initErr != nil {
			return PasswordResetResult{}, initErr
		}
	}
	return PasswordResetResult{AccountID: strings.TrimSpace(input.AccountID), TemporaryPassword: password}, nil
}

// ChangeOwnPassword 使用读取到的旧摘要作为并发条件更新凭据，并在同一事务撤销该账号
// 全部会话（包括当前浏览器）；调用方随后必须清理 Cookie，避免旧会话表面仍显示已登录。
func (service *AccountLifecycleService) ChangeOwnPassword(ctx context.Context, input PasswordChangeInput) error {
	if err := validatePasswordChangeInput(input); err != nil {
		return err
	}
	credential, err := service.repository.FindLocalPasswordCredential(ctx, strings.TrimSpace(input.TenantID), strings.TrimSpace(input.AccountID))
	if err != nil {
		return err
	}
	if credential.AccountStatus != domain.StatusActive || credential.CredentialStatus != domain.StatusActive {
		return ErrConflict
	}
	matched, err := service.verifier.Verify(input.CurrentPassword, credential.HashAlgorithm, credential.PasswordHash, credential.AlgorithmParams)
	if err != nil {
		return fmt.Errorf("verify current password: %w", err)
	}
	if !matched {
		return ErrUnauthenticated
	}
	digest, metadata, err := service.hasher.Hash(input.NewPassword)
	if err != nil {
		return fmt.Errorf("hash new password: %w", err)
	}
	return service.repository.ChangeOwnPassword(ctx, PasswordWrite{
		TenantID: strings.TrimSpace(input.TenantID), AccountID: strings.TrimSpace(input.AccountID), OperatorID: strings.TrimSpace(input.OperatorID), ExpectedHash: append([]byte(nil), credential.PasswordHash...), PasswordDigest: digest, AlgorithmParams: metadata, OccurredAt: service.clock.Now().UTC(), RevokeReason: "SELF_PASSWORD_CHANGED",
	})
}

func (service *AccountLifecycleService) writeAdministratorPassword(ctx context.Context, tenantID, accountID, operatorID string, version uint64, password, credentialID, reason string) (domain.Account, error) {
	digest, metadata, err := service.hasher.Hash(password)
	if err != nil {
		return domain.Account{}, fmt.Errorf("hash password: %w", err)
	}
	write := PasswordWrite{CredentialID: credentialID, TenantID: strings.TrimSpace(tenantID), AccountID: strings.TrimSpace(accountID), OperatorID: strings.TrimSpace(operatorID), ExpectedVersion: version, PasswordDigest: digest, AlgorithmParams: metadata, OccurredAt: service.clock.Now().UTC(), RevokeReason: reason}
	if credentialID != "" {
		return service.repository.InitializePassword(ctx, write)
	}
	return service.repository.ResetPassword(ctx, write)
}

func validateLocalAccountCreateInput(input LocalAccountCreateInput) error {
	if strings.TrimSpace(input.TenantID) == "" || strings.TrimSpace(input.OperatorID) == "" || strings.TrimSpace(input.UserID) == "" || !validAccountName(input.AccountName) || validateStrongPassword(input.InitialPassword) != nil {
		return ErrValidation
	}
	return nil
}

func normalizedFutureTime(value *time.Time) *time.Time {
	if value == nil {
		return nil
	}
	normalized := value.UTC()
	return &normalized
}

func validatePasswordInitializeInput(input PasswordInitializeInput) error {
	if strings.TrimSpace(input.TenantID) == "" || strings.TrimSpace(input.OperatorID) == "" || strings.TrimSpace(input.AccountID) == "" || input.Version == 0 || validateStrongPassword(input.Password) != nil {
		return ErrValidation
	}
	return nil
}

func validatePasswordResetInput(input PasswordResetInput) error {
	if strings.TrimSpace(input.TenantID) == "" || strings.TrimSpace(input.OperatorID) == "" || strings.TrimSpace(input.AccountID) == "" || input.Version == 0 {
		return ErrValidation
	}
	return nil
}

func validatePasswordChangeInput(input PasswordChangeInput) error {
	if strings.TrimSpace(input.TenantID) == "" || strings.TrimSpace(input.AccountID) == "" || strings.TrimSpace(input.OperatorID) == "" || input.CurrentPassword == "" || validateStrongPassword(input.NewPassword) != nil {
		return ErrValidation
	}
	return nil
}

func validAccountName(value string) bool {
	value = strings.TrimSpace(value)
	return len(value) >= accountNameMinLength && len(value) <= accountNameMaxLength && accountNamePattern.MatchString(value)
}

// validateStrongPassword 执行共享的账号密码策略：8–128 个字符、不含 Unicode 空白，且至少
// 各含一个大写字母、小写字母、数字和符号。
func validateStrongPassword(password string) error {
	length := len([]rune(password))
	if length < passwordMinRunes || length > passwordMaxRunes {
		return ErrValidation
	}
	var upper, lower, digit, symbol bool
	for _, character := range password {
		if unicode.IsSpace(character) {
			return ErrValidation
		}
		switch {
		case unicode.IsUpper(character):
			upper = true
		case unicode.IsLower(character):
			lower = true
		case unicode.IsDigit(character):
			digit = true
		default:
			symbol = true
		}
	}
	if !upper || !lower || !digit || !symbol {
		return ErrValidation
	}
	return nil
}
