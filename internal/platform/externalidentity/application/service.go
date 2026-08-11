// Package application implements machine-only external customer provisioning and Portal role
// convergence. It may reserve a credential-free local login account, but never creates, receives,
// generates, or returns password or other credential material.
package application

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"errors"
	"fmt"
	"net/mail"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/J-S-Te/Basic-Platform/internal/platform/externalidentity/domain"
	"github.com/J-S-Te/Basic-Platform/internal/shared/appctx"
)

const (
	// 机器接口只能收敛到客户门户的低权限客户角色；应用码和角色码不是调用方可自由选择的委派入口。
	// PortalApplicationCode 是平台默认门户应用码，B4 解耦后可通过 Options 覆盖。
	PortalApplicationCode = "customer_portal"
	PortalCustomerRole    = "portal_customer"
	maxClockSkew          = 5 * time.Minute
	nonceRetention        = 10 * time.Minute
)

// Options 允许装配层覆盖门户应用码，缺省保持平台默认，保证既有行为不变。
type Options struct {
	PortalApplicationCode string
}

// ServiceOption 是构造选项函数。
type ServiceOption func(*Options)

// WithPortalApplicationCode 覆盖客户门户应用码（默认 customer_portal）。
func WithPortalApplicationCode(code string) ServiceOption {
	return func(options *Options) {
		if strings.TrimSpace(code) != "" {
			options.PortalApplicationCode = strings.TrimSpace(code)
		}
	}
}

var (
	ErrValidation  = errors.New("external identity validation failed")
	ErrConflict    = errors.New("external identity conflict")
	ErrNotFound    = errors.New("external identity not found")
	ErrReplay      = errors.New("external identity integration replay detected")
	ErrUnavailable = errors.New("external identity dependency unavailable")
)

type Clock interface{ Now() time.Time }
type IDGenerator interface {
	New(time.Time) (string, error)
}

type MobileProtection interface {
	Encrypt(string) ([]byte, error)
	Digest(string) []byte
}

type RequestProof struct {
	// 幂等键保证业务重试返回原结果；时间戳和 nonce 则阻断截获请求在时间窗内外被再次利用。
	IdempotencyKey string
	Timestamp      time.Time
	Nonce          string
}

type ProvisionInput struct {
	DisplayName string
	Mobile      string
	Email       string
}

type ProvisionResult struct {
	PlatformUserID string `json:"platform_user_id"`
	AccountNo      string `json:"account_no"`
}

type RoleInput struct {
	PlatformUserID  string
	ApplicationCode string
	RoleCode        string
}

type ProvisionCommand struct {
	Principal      appctx.Principal
	IdempotencyKey string
	RequestHash    [32]byte
	NonceHash      [32]byte
	NonceExpiresAt time.Time
	DisplayName    string
	Email          *string
	EmailDigest    []byte
	MobileCipher   []byte
	MobileDigest   []byte
	IdentityID     string
	PlatformUserID string
	AccountID      string
	AccountNo      string
	EventID        string
	OccurredAt     time.Time
}

type RoleCommand struct {
	Principal       appctx.Principal
	IdempotencyKey  string
	RequestHash     [32]byte
	NonceHash       [32]byte
	NonceExpiresAt  time.Time
	PlatformUserID  string
	ApplicationCode string
	RoleCode        string
	BindingID       string
	EventID         string
	OccurredAt      time.Time
}

type Repository interface {
	Provision(context.Context, ProvisionCommand) (ProvisionResult, error)
	AssignRole(context.Context, RoleCommand) (domain.RoleResult, error)
	RevokeRole(context.Context, RoleCommand) (domain.RoleResult, error)
}

type Service struct {
	repository            Repository
	mobiles               MobileProtection
	ids                   IDGenerator
	clock                 Clock
	portalApplicationCode string
}

func NewService(repository Repository, mobiles MobileProtection, ids IDGenerator, clock Clock, options ...ServiceOption) (*Service, error) {
	if repository == nil || mobiles == nil || ids == nil || clock == nil {
		return nil, errors.New("external identity service dependencies must not be nil")
	}
	opts := Options{PortalApplicationCode: PortalApplicationCode}
	for _, apply := range options {
		apply(&opts)
	}
	return &Service{
		repository: repository, mobiles: mobiles, ids: ids, clock: clock,
		portalApplicationCode: opts.PortalApplicationCode,
	}, nil
}

func (service *Service) Provision(ctx context.Context, principal appctx.Principal, proof RequestProof, input ProvisionInput) (ProvisionResult, error) {
	// 先验证机器主体与防重放证明，再处理个人信息；验证失败时不执行加密、摘要或写库。
	now, nonceHash, err := validateProof(principal, proof, service.clock.Now())
	if err != nil {
		return ProvisionResult{}, err
	}
	displayName := strings.TrimSpace(input.DisplayName)
	if displayName == "" || utf8.RuneCountInString(displayName) > 128 || containsControl(displayName) {
		return ProvisionResult{}, ErrValidation
	}
	email, emailDigest, err := service.prepareEmail(input.Email)
	if err != nil {
		return ProvisionResult{}, err
	}
	mobile, mobileCipher, mobileDigest, err := service.prepareMobile(input.Mobile)
	if err != nil {
		return ProvisionResult{}, err
	}
	if email == nil && mobile == "" {
		return ProvisionResult{}, ErrValidation
	}
	// 幂等键还必须绑定规范化后的完整请求。相同键携带不同客户资料会被仓储判定为冲突。
	requestHash := hashProvisionRequest(displayName, mobile, email)
	identityID, userID, eventID, err := service.newIDs(now, 3)
	if err != nil {
		return ProvisionResult{}, err
	}
	accountID, err := service.ids.New(now)
	if err != nil {
		return ProvisionResult{}, fmt.Errorf("generate external login account identifier: %w", err)
	}
	return service.repository.Provision(ctx, ProvisionCommand{
		Principal: principal, IdempotencyKey: normalizedIdempotencyKey(proof), RequestHash: requestHash,
		NonceHash: nonceHash, NonceExpiresAt: now.Add(nonceRetention), DisplayName: displayName,
		Email: email, EmailDigest: emailDigest, MobileCipher: mobileCipher, MobileDigest: mobileDigest,
		IdentityID: identityID, PlatformUserID: userID, AccountID: accountID, AccountNo: "EXT-" + strings.ToUpper(userID),
		EventID: eventID, OccurredAt: now,
	})
}

func (service *Service) AssignPortalRole(ctx context.Context, principal appctx.Principal, proof RequestProof, input RoleInput) (domain.RoleResult, error) {
	return service.role(ctx, principal, proof, input, false)
}

func (service *Service) RevokePortalRole(ctx context.Context, principal appctx.Principal, proof RequestProof, input RoleInput) (domain.RoleResult, error) {
	return service.role(ctx, principal, proof, input, true)
}

func (service *Service) role(ctx context.Context, principal appctx.Principal, proof RequestProof, input RoleInput, revoke bool) (domain.RoleResult, error) {
	now, nonceHash, err := validateProof(principal, proof, service.clock.Now())
	if err != nil {
		return domain.RoleResult{}, err
	}
	input.PlatformUserID = strings.TrimSpace(input.PlatformUserID)
	input.ApplicationCode = strings.TrimSpace(input.ApplicationCode)
	input.RoleCode = strings.TrimSpace(input.RoleCode)
	// 严格固定应用与角色，避免具备该机器接口权限的 CRM 客户端借此绑定平台或其他应用高权角色。
	if input.PlatformUserID == "" || len(input.PlatformUserID) > 128 || input.ApplicationCode != service.portalApplicationCode || input.RoleCode != PortalCustomerRole {
		return domain.RoleResult{}, ErrValidation
	}
	requestHash := hashRoleRequest(input, revoke)
	bindingID, eventID, _, err := service.newIDs(now, 2)
	if err != nil {
		return domain.RoleResult{}, err
	}
	command := RoleCommand{
		Principal: principal, IdempotencyKey: normalizedIdempotencyKey(proof), RequestHash: requestHash,
		NonceHash: nonceHash, NonceExpiresAt: now.Add(nonceRetention), PlatformUserID: input.PlatformUserID,
		ApplicationCode: input.ApplicationCode, RoleCode: input.RoleCode,
		BindingID: bindingID, EventID: eventID, OccurredAt: now,
	}
	if revoke {
		return service.repository.RevokeRole(ctx, command)
	}
	return service.repository.AssignRole(ctx, command)
}

func validateProof(principal appctx.Principal, proof RequestProof, now time.Time) (time.Time, [32]byte, error) {
	now = now.UTC()
	proof.IdempotencyKey = strings.TrimSpace(proof.IdempotencyKey)
	proof.Nonce = strings.TrimSpace(proof.Nonce)
	if !principal.Valid() || proof.IdempotencyKey == "" || len(proof.IdempotencyKey) > 128 || proof.Nonce == "" || len(proof.Nonce) > 128 || proof.Timestamp.IsZero() {
		return time.Time{}, [32]byte{}, ErrValidation
	}
	timestamp := proof.Timestamp.UTC()
	// 同时拒绝过旧和显著超前的请求，避免服务器时钟偏差被利用来延长重放窗口。
	if timestamp.Before(now.Add(-maxClockSkew)) || timestamp.After(now.Add(maxClockSkew)) {
		return time.Time{}, [32]byte{}, ErrReplay
	}
	return now, sha256.Sum256([]byte(proof.Nonce)), nil
}

func normalizedIdempotencyKey(proof RequestProof) string {
	return strings.TrimSpace(proof.IdempotencyKey)
}

func (service *Service) prepareEmail(value string) (*string, []byte, error) {
	// 邮箱先做唯一规范化再摘要，确保大小写差异不会创建两个外部身份。
	value = strings.ToLower(strings.TrimSpace(value))
	if value == "" {
		return nil, nil, nil
	}
	address, err := mail.ParseAddress(value)
	if err != nil || address.Address != value || len(value) > 320 || containsControl(value) {
		return nil, nil, ErrValidation
	}
	return &value, append([]byte(nil), service.mobiles.Digest("email\x00"+value)...), nil
}

func (service *Service) prepareMobile(value string) (string, []byte, []byte, error) {
	// 密文用于必要的受控展示，带域分隔的摘要用于查重；数据库查重不需要解密手机号。
	value = strings.ReplaceAll(strings.ReplaceAll(strings.TrimSpace(value), " ", ""), "-", "")
	if value == "" {
		return "", nil, nil, nil
	}
	if len(value) > 32 {
		return "", nil, nil, ErrValidation
	}
	for index, character := range value {
		if !(character >= '0' && character <= '9') && !(character == '+' && index == 0) {
			return "", nil, nil, ErrValidation
		}
	}
	ciphertext, err := service.mobiles.Encrypt(value)
	if err != nil {
		return "", nil, nil, fmt.Errorf("protect external customer mobile: %w", ErrUnavailable)
	}
	return value, ciphertext, append([]byte(nil), service.mobiles.Digest("mobile\x00"+value)...), nil
}

func (service *Service) newIDs(now time.Time, count int) (string, string, string, error) {
	values := make([]string, count)
	for index := range values {
		value, err := service.ids.New(now)
		if err != nil {
			return "", "", "", fmt.Errorf("generate external identity identifier: %w", err)
		}
		values[index] = value
	}
	if count == 2 {
		return values[0], values[1], "", nil
	}
	return values[0], values[1], values[2], nil
}

func hashProvisionRequest(displayName, mobile string, email *string) [32]byte {
	emailValue := ""
	if email != nil {
		emailValue = *email
	}
	return sha256.Sum256([]byte("provision\x00" + displayName + "\x00" + mobile + "\x00" + emailValue))
}

func hashRoleRequest(input RoleInput, revoke bool) [32]byte {
	action := "assign"
	if revoke {
		action = "revoke"
	}
	return sha256.Sum256([]byte(action + "\x00" + input.PlatformUserID + "\x00" + input.ApplicationCode + "\x00" + input.RoleCode))
}

func containsControl(value string) bool {
	for _, character := range value {
		if character < 0x20 || character == 0x7f {
			return true
		}
	}
	return false
}

func SameDigest(left, right []byte) bool {
	// 摘要比较采用常量时间，避免在机器接口冲突路径上泄漏前缀匹配信息。
	return len(left) == sha256.Size && len(right) == sha256.Size && subtle.ConstantTimeCompare(left, right) == 1
}
