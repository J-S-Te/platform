// Package application 提供 OAuth 客户端登记与凭据生命周期用例。
package application

import (
	"context"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"net"
	"net/url"
	"regexp"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"golang.org/x/crypto/bcrypt"
)

var (
	// ErrManagementNotFound 表示租户范围内的 OAuth 客户端或凭据不存在；
	// 跨租户查询同样折叠为该错误，不泄露存在性。
	ErrManagementNotFound = errors.New("OAuth client resource not found")
	// ErrManagementConflict 表示客户端 ID 重复或聚合版本过期（乐观锁失败）。
	ErrManagementConflict = errors.New("OAuth client resource conflict")
	// ErrManagementValidation 表示调用方可自行修正的登记输入错误。
	ErrManagementValidation = errors.New("invalid OAuth client management input")
)

const (
	oauthClientStatusActive   = "ACTIVE"
	oauthClientStatusDisabled = "DISABLED"
	credentialStatusActive    = "ACTIVE"
	credentialStatusRevoked   = "REVOKED"

	clientSecretPrefix      = "ocsec_"
	clientSecretRandomBytes = 32
	maxClientSecretLifetime = 365 * 24 * time.Hour
	maxRotationOverlap      = 30 * 24 * time.Hour
)

var (
	clientIDPattern = regexp.MustCompile(`^[A-Za-z0-9._-]{3,128}$`)
	scopePattern    = regexp.MustCompile(`^[A-Za-z0-9:._-]{1,128}$`)
)

// ManagementIdentifierGenerator 提供聚合与凭据标识符；实现必须保证时间有序且全局唯一，
// 供审计追踪与凭据版本管理使用。
type ManagementIdentifierGenerator interface {
	New(at time.Time) (string, error)
}

// OAuthClientManagementRepository 持久化 OAuth 客户端聚合变更。
// 每次写入必须原子完成，包括聚合子集合的整体替换与凭据轮换，
// 不允许出现聚合主记录与子记录不一致的中间状态。
type OAuthClientManagementRepository interface {
	CreateOAuthClient(context.Context, OAuthClientCreateInput, string, *SecretWrite, time.Time) (OAuthClientView, error)
	ListOAuthClients(context.Context, string) ([]OAuthClientView, error)
	GetOAuthClient(context.Context, string, string) (OAuthClientView, error)
	GetOAuthClientByClientID(context.Context, string, string) (OAuthClientView, error)
	ReplaceOAuthClientScopes(context.Context, OAuthClientScopesUpdateInput, time.Time) (OAuthClientView, error)
	ReplaceOAuthClientRedirectURIs(context.Context, OAuthClientRedirectURIsUpdateInput, time.Time) (OAuthClientView, error)
	GetOAuthClientPostLogoutRedirectURIs(context.Context, string, string) (OAuthClientPostLogoutRedirectURIsView, error)
	ReplaceOAuthClientPostLogoutRedirectURIs(context.Context, OAuthClientPostLogoutRedirectURIsUpdateInput, time.Time) (OAuthClientPostLogoutRedirectURIsView, error)
	GetOAuthClientJWKs(context.Context, string, string) (OAuthClientJWKsView, error)
	ReplaceOAuthClientJWKs(context.Context, OAuthClientJWKsUpdateInput, time.Time) (OAuthClientJWKsView, error)
	DisableOAuthClient(context.Context, OAuthClientDisableInput, time.Time) (OAuthClientView, error)
	CreateOAuthClientSecret(context.Context, OAuthClientSecretCreateInput, SecretWrite, time.Time) (OAuthClientCredentialView, error)
	RotateOAuthClientSecret(context.Context, OAuthClientSecretRotateInput, SecretWrite, time.Time) (OAuthClientCredentialView, error)
	DisableOAuthClientSecret(context.Context, OAuthClientSecretDisableInput, time.Time) error
}

// OAuthClientCreateInput 定义租户范围内的客户端登记。使用 client_secret_basic 的客户端
// 会在创建结果中收到一枚生成的 Secret；该 Secret 绝不存入本输入结构，
// 也不会出现在常规 OAuthClientView 中。
type OAuthClientCreateInput struct {
	TenantID               string
	ApplicationID          string
	EnvironmentID          string
	OperatorID             string
	ClientID               string
	ClientName             string
	ClientType             string
	TokenAuthMethod        string
	AccessTokenTTLSeconds  uint
	RefreshTokenTTLSeconds uint
	RequirePKCE            bool
	GrantTypes             []string
	Scopes                 []string
	RedirectURIs           []string
	SecretValidUntil       *time.Time
}

// OAuthClientScopesUpdateInput 以乐观锁整体替换允许的 scope 集合；
// 部分修改必须读改写整个集合，防止并发更新产生口径漂移。
type OAuthClientScopesUpdateInput struct {
	TenantID      string
	OAuthClientID string
	OperatorID    string
	Scopes        []string
	Version       uint64
}

// OAuthClientRedirectURIsUpdateInput 以乐观锁整体替换回调地址集合；
// 替换是全量的，任何未随请求提交的旧地址在成功后立即失效。
type OAuthClientRedirectURIsUpdateInput struct {
	TenantID      string
	OAuthClientID string
	OperatorID    string
	RedirectURIs  []string
	Version       uint64
}

// OAuthClientPostLogoutRedirectURIsUpdateInput 使用乐观锁整体替换 RP 发起登出后的回调地址集合。
// 该集合与 OAuth 授权回调地址独立，避免两类用途混用。
type OAuthClientPostLogoutRedirectURIsUpdateInput struct {
	TenantID               string
	OAuthClientID          string
	OperatorID             string
	PostLogoutRedirectURIs []string
	Version                uint64
}

// OAuthClientJWK 表示用于 private_key_jwt 客户端断言或签名授权请求的单个公钥。
// PublicJWK 只能包含一个公钥 JWK 对象；应用服务会在持久化前拒绝所有私钥字段。
type OAuthClientJWK struct {
	ID         string
	KeyID      string
	PublicJWK  json.RawMessage
	Algorithm  string
	ValidFrom  time.Time
	ValidUntil *time.Time
	Status     string
}

// OAuthClientJWKsUpdateInput 使用乐观锁整体替换客户端公钥 JWK 集合。
// JWKs 中的 ID 由服务端生成，调用方提供的值会被覆盖。
type OAuthClientJWKsUpdateInput struct {
	TenantID      string
	OAuthClientID string
	OperatorID    string
	JWKs          []OAuthClientJWK
	Version       uint64
}

// OAuthClientPostLogoutRedirectURIsView 表示独立的登出后回调地址集合，
// 并携带后续整体替换所需的聚合版本号。
type OAuthClientPostLogoutRedirectURIsView struct {
	OAuthClientID          string
	Version                uint64
	PostLogoutRedirectURIs []string
}

// OAuthClientJWKsView 表示安全的公钥集合，并携带后续整体替换所需的聚合版本号。
type OAuthClientJWKsView struct {
	OAuthClientID string
	Version       uint64
	JWKs          []OAuthClientJWK
}

// OAuthClientDisableInput 停用客户端并吊销其全部 ACTIVE Secret；
// Version 用于乐观锁，防止并发修改被静默覆盖。
type OAuthClientDisableInput struct {
	TenantID      string
	OAuthClientID string
	OperatorID    string
	Version       uint64
}

// OAuthClientSecretCreateInput 追加一枚可并发生效的 Secret。
type OAuthClientSecretCreateInput struct {
	TenantID      string
	OAuthClientID string
	OperatorID    string
	ValidUntil    *time.Time
	// ExpectedClientVersion 在完成租户/应用/环境/scope 核验后，为受控的按用途
	// 重新交付提供围栏；nil 表示沿用旧 API 的无围栏语义。
	ExpectedClientVersion *uint64
}

// OAuthClientSecretRotateInput 生成新 Secret，并仅在请求的重叠窗口内保持既有
// ACTIVE Secret 有效；重叠为 0 表示立即吊销，避免无界的双密钥共存期。
type OAuthClientSecretRotateInput struct {
	TenantID       string
	OAuthClientID  string
	OperatorID     string
	OverlapSeconds uint
	ValidUntil     *time.Time
}

// OAuthClientSecretDisableInput 吊销单个凭据版本；入参不含也不回显 Secret 哈希，
// 防止管理面成为哈希泄露通道。
type OAuthClientSecretDisableInput struct {
	TenantID      string
	OAuthClientID string
	CredentialID  string
	OperatorID    string
}

// SecretWrite 是生成 Secret 的持久化安全表示。PlaintextSecret 刻意独立在结构之外，
// 绝不传入仓储层，也绝不允许进日志。
type SecretWrite struct {
	CredentialID string
	SecretHash   []byte
	Fingerprint  string
	ValidUntil   *time.Time
}

// OAuthClientCredentialView 仅含安全的 Secret 版本元数据（指纹与有效期），
// 不含哈希与明文，供管理面展示与审计比对。
type OAuthClientCredentialView struct {
	ID          string
	Fingerprint string
	ValidFrom   time.Time
	ValidUntil  *time.Time
	RevokedAt   *time.Time
	Status      string
}

// OAuthClientView 是安全的管理投影，刻意不含 Secret 哈希或明文字段，
// 可直接跨管理接口传输。
type OAuthClientView struct {
	ID                     string
	TenantID               string
	ApplicationID          string
	EnvironmentID          string
	ClientID               string
	ClientName             string
	ClientType             string
	TokenAuthMethod        string
	AccessTokenTTLSeconds  uint
	RefreshTokenTTLSeconds uint
	RequirePKCE            bool
	Status                 string
	Version                uint64
	GrantTypes             []string
	Scopes                 []string
	RedirectURIs           []string
	Credentials            []OAuthClientCredentialView
	CreatedAt              time.Time
	UpdatedAt              time.Time
}

// OAuthClientSecretResult 只允许出现在创建或密钥生命周期操作的即时结果中。PlaintextSecret
// 必须由对应 HTTP 响应一次性呈现，禁止日志、审计、缓存和持久化；后续无法从平台恢复。
type OAuthClientSecretResult struct {
	Credential      OAuthClientCredentialView
	PlaintextSecret string
}

// OAuthClientCreateResult 返回安全的登记投影；仅当所选认证方式需要 Secret 时
// 才附带初始明文，且只出现在本次响应中。
type OAuthClientCreateResult struct {
	Client          OAuthClientView
	PlaintextSecret string
}

// RedirectURIValidationPolicy 控制可登记的回调协议。HTTPS 始终允许；HTTP 默认仅限回环地址，
// 可信服务器确需明文回调时必须通过配置显式开启，并仍由授权端点执行完整字符串精确匹配。
type RedirectURIValidationPolicy struct {
	AllowInsecureHTTP bool
}

// OAuthClientManagementService 串联登记输入校验、加密级 Secret 生成与哈希、
// 以及租户范围内的持久化；Secret 明文只在生成调用链上存在。
type OAuthClientManagementService struct {
	repository                  OAuthClientManagementRepository
	ids                         ManagementIdentifierGenerator
	clock                       Clock
	redirectURIValidationPolicy RedirectURIValidationPolicy
}

// NewOAuthClientManagementService 构造管理服务；仓储、ID 生成器或时钟缺失时拒绝构造，
// 凭据生命周期不允许在没有审计时钟的情况下运行。
func NewOAuthClientManagementService(repository OAuthClientManagementRepository, ids ManagementIdentifierGenerator, clock Clock, redirectURIValidationPolicy RedirectURIValidationPolicy) (*OAuthClientManagementService, error) {
	if repository == nil || ids == nil || clock == nil {
		return nil, errors.New("OAuth client management service dependencies must not be nil")
	}
	return &OAuthClientManagementService{
		repository: repository, ids: ids, clock: clock, redirectURIValidationPolicy: redirectURIValidationPolicy,
	}, nil
}

// CreateOAuthClient 登记 OAuth 客户端；仅 client_secret_basic 登记会生成初始 Secret，
// 公开客户端不产生任何可持久化凭据。
func (service *OAuthClientManagementService) CreateOAuthClient(ctx context.Context, input OAuthClientCreateInput) (OAuthClientCreateResult, error) {
	input, err := normalizeOAuthClientCreate(input, service.redirectURIValidationPolicy)
	if err != nil {
		return OAuthClientCreateResult{}, err
	}
	now := service.clock.Now().UTC()
	if !validSecretValidUntil(input.SecretValidUntil, now) {
		return OAuthClientCreateResult{}, ErrManagementValidation
	}
	clientID, err := service.ids.New(now)
	if err != nil {
		return OAuthClientCreateResult{}, fmt.Errorf("generate OAuth client id: %w", err)
	}

	var secret *SecretWrite
	var plaintext string
	if input.TokenAuthMethod == "client_secret_basic" {
		write, value, err := service.newSecretWrite(now, input.SecretValidUntil)
		if err != nil {
			return OAuthClientCreateResult{}, err
		}
		secret = &write
		plaintext = value
	}

	client, err := service.repository.CreateOAuthClient(ctx, input, clientID, secret, now)
	if err != nil {
		return OAuthClientCreateResult{}, err
	}
	return OAuthClientCreateResult{Client: client, PlaintextSecret: plaintext}, nil
}

// ListOAuthClients 返回单个租户内的安全客户端投影集合，不跨租户聚合。
func (service *OAuthClientManagementService) ListOAuthClients(ctx context.Context, tenantID string) ([]OAuthClientView, error) {
	tenantID = strings.TrimSpace(tenantID)
	if tenantID == "" {
		return nil, ErrManagementValidation
	}
	return service.repository.ListOAuthClients(ctx, tenantID)
}

// GetOAuthClient 按内部聚合 ID 返回租户范围内的安全客户端投影。
func (service *OAuthClientManagementService) GetOAuthClient(ctx context.Context, tenantID, oauthClientID string) (OAuthClientView, error) {
	if strings.TrimSpace(tenantID) == "" || strings.TrimSpace(oauthClientID) == "" {
		return OAuthClientView{}, ErrManagementValidation
	}
	return service.repository.GetOAuthClient(ctx, strings.TrimSpace(tenantID), strings.TrimSpace(oauthClientID))
}

// GetOAuthClientByClientID 用公开的 client_id 字符串（而非内部聚合 ID）返回
// 租户范围内的安全投影，供按稳定标识查找 broker 或 well-known 客户端，
// 避免为定位单客户端而扫描整个租户列表。
func (service *OAuthClientManagementService) GetOAuthClientByClientID(ctx context.Context, tenantID, clientID string) (OAuthClientView, error) {
	tenantID = strings.TrimSpace(tenantID)
	clientID = strings.TrimSpace(clientID)
	if tenantID == "" || clientID == "" {
		return OAuthClientView{}, ErrManagementValidation
	}
	return service.repository.GetOAuthClientByClientID(ctx, tenantID, clientID)
}

// ReplaceOAuthClientScopes 整体替换客户端允许的 scope 集合；版本冲突返回
// ErrManagementConflict，调用方必须重读后重试而不是盲目覆盖。
func (service *OAuthClientManagementService) ReplaceOAuthClientScopes(ctx context.Context, input OAuthClientScopesUpdateInput) (OAuthClientView, error) {
	input.TenantID, input.OAuthClientID, input.OperatorID = strings.TrimSpace(input.TenantID), strings.TrimSpace(input.OAuthClientID), strings.TrimSpace(input.OperatorID)
	input.Scopes = normalizeStrings(input.Scopes)
	if input.TenantID == "" || input.OAuthClientID == "" || input.OperatorID == "" || input.Version == 0 || !validScopes(input.Scopes) {
		return OAuthClientView{}, ErrManagementValidation
	}
	return service.repository.ReplaceOAuthClientScopes(ctx, input, service.clock.Now().UTC())
}

// ReplaceOAuthClientRedirectURIs 整体替换客户端登记的回调地址；新集合先经
// RedirectURIValidationPolicy 校验，非法协议在写入前即被拒绝。
func (service *OAuthClientManagementService) ReplaceOAuthClientRedirectURIs(ctx context.Context, input OAuthClientRedirectURIsUpdateInput) (OAuthClientView, error) {
	input.TenantID, input.OAuthClientID, input.OperatorID = strings.TrimSpace(input.TenantID), strings.TrimSpace(input.OAuthClientID), strings.TrimSpace(input.OperatorID)
	input.RedirectURIs = normalizeStrings(input.RedirectURIs)
	if input.TenantID == "" || input.OAuthClientID == "" || input.OperatorID == "" || input.Version == 0 || !validRedirectURIs(input.RedirectURIs, service.redirectURIValidationPolicy) {
		return OAuthClientView{}, ErrManagementValidation
	}
	return service.repository.ReplaceOAuthClientRedirectURIs(ctx, input, service.clock.Now().UTC())
}

// GetOAuthClientPostLogoutRedirectURIs 在租户边界内读取独立的登出后回调地址集合。
func (service *OAuthClientManagementService) GetOAuthClientPostLogoutRedirectURIs(ctx context.Context, tenantID, oauthClientID string) (OAuthClientPostLogoutRedirectURIsView, error) {
	tenantID, oauthClientID = strings.TrimSpace(tenantID), strings.TrimSpace(oauthClientID)
	if tenantID == "" || oauthClientID == "" {
		return OAuthClientPostLogoutRedirectURIsView{}, ErrManagementValidation
	}
	return service.repository.GetOAuthClientPostLogoutRedirectURIs(ctx, tenantID, oauthClientID)
}

// ReplaceOAuthClientPostLogoutRedirectURIs 使用乐观锁整体替换独立的登出后回调地址集合。
func (service *OAuthClientManagementService) ReplaceOAuthClientPostLogoutRedirectURIs(ctx context.Context, input OAuthClientPostLogoutRedirectURIsUpdateInput) (OAuthClientPostLogoutRedirectURIsView, error) {
	input.TenantID, input.OAuthClientID, input.OperatorID = strings.TrimSpace(input.TenantID), strings.TrimSpace(input.OAuthClientID), strings.TrimSpace(input.OperatorID)
	input.PostLogoutRedirectURIs = normalizeStrings(input.PostLogoutRedirectURIs)
	if input.TenantID == "" || input.OAuthClientID == "" || input.OperatorID == "" || input.Version == 0 || !validRedirectURIs(input.PostLogoutRedirectURIs, service.redirectURIValidationPolicy) {
		return OAuthClientPostLogoutRedirectURIsView{}, ErrManagementValidation
	}
	return service.repository.ReplaceOAuthClientPostLogoutRedirectURIs(ctx, input, service.clock.Now().UTC())
}

// GetOAuthClientJWKs 在租户边界内读取已登记的公钥材料。
func (service *OAuthClientManagementService) GetOAuthClientJWKs(ctx context.Context, tenantID, oauthClientID string) (OAuthClientJWKsView, error) {
	tenantID, oauthClientID = strings.TrimSpace(tenantID), strings.TrimSpace(oauthClientID)
	if tenantID == "" || oauthClientID == "" {
		return OAuthClientJWKsView{}, ErrManagementValidation
	}
	return service.repository.GetOAuthClientJWKs(ctx, tenantID, oauthClientID)
}

// ReplaceOAuthClientJWKs 整体替换供 private_key_jwt 和 JAR 使用的公钥 JWK。
// 服务会生成所有持久化标识，并在调用仓储前拒绝私钥材料。
func (service *OAuthClientManagementService) ReplaceOAuthClientJWKs(ctx context.Context, input OAuthClientJWKsUpdateInput) (OAuthClientJWKsView, error) {
	input.TenantID, input.OAuthClientID, input.OperatorID = strings.TrimSpace(input.TenantID), strings.TrimSpace(input.OAuthClientID), strings.TrimSpace(input.OperatorID)
	if input.TenantID == "" || input.OAuthClientID == "" || input.OperatorID == "" || input.Version == 0 {
		return OAuthClientJWKsView{}, ErrManagementValidation
	}
	now := service.clock.Now().UTC()
	keys, err := normalizeOAuthClientJWKs(input.JWKs, now, service.ids)
	if err != nil {
		return OAuthClientJWKsView{}, err
	}
	input.JWKs = keys
	return service.repository.ReplaceOAuthClientJWKs(ctx, input, now)
}

// DisableOAuthClient 停用聚合并吊销全部当前 ACTIVE 的客户端 Secret；
// 吊销即时生效，已签发令牌由认证时的登记回查兜底失效。
func (service *OAuthClientManagementService) DisableOAuthClient(ctx context.Context, input OAuthClientDisableInput) (OAuthClientView, error) {
	input.TenantID, input.OAuthClientID, input.OperatorID = strings.TrimSpace(input.TenantID), strings.TrimSpace(input.OAuthClientID), strings.TrimSpace(input.OperatorID)
	if input.TenantID == "" || input.OAuthClientID == "" || input.OperatorID == "" || input.Version == 0 {
		return OAuthClientView{}, ErrManagementValidation
	}
	return service.repository.DisableOAuthClient(ctx, input, service.clock.Now().UTC())
}

// CreateOAuthClientSecret 追加一枚新的 ACTIVE Secret。仓储的守卫写入把它限制在
// ACTIVE 且使用 client_secret_basic 的客户端上，停用或公开客户端无法获得密钥。
func (service *OAuthClientManagementService) CreateOAuthClientSecret(ctx context.Context, input OAuthClientSecretCreateInput) (OAuthClientSecretResult, error) {
	input.TenantID, input.OAuthClientID, input.OperatorID = strings.TrimSpace(input.TenantID), strings.TrimSpace(input.OAuthClientID), strings.TrimSpace(input.OperatorID)
	if input.TenantID == "" || input.OAuthClientID == "" || input.OperatorID == "" {
		return OAuthClientSecretResult{}, ErrManagementValidation
	}
	now := service.clock.Now().UTC()
	validUntil := normalizeSecretValidUntil(input.ValidUntil)
	if !validSecretValidUntil(validUntil, now) {
		return OAuthClientSecretResult{}, ErrManagementValidation
	}
	write, plaintext, err := service.newSecretWrite(now, validUntil)
	if err != nil {
		return OAuthClientSecretResult{}, err
	}
	credential, err := service.repository.CreateOAuthClientSecret(ctx, input, write, now)
	if err != nil {
		return OAuthClientSecretResult{}, err
	}
	return OAuthClientSecretResult{Credential: credential, PlaintextSecret: plaintext}, nil
}

// RotateOAuthClientSecret 生成新密钥，并把旧活跃密钥限制在有上限的重叠窗口内；
// 这样消费者可无中断发布，同时不会因忘记下线旧密钥而形成永久双凭据。
func (service *OAuthClientManagementService) RotateOAuthClientSecret(ctx context.Context, input OAuthClientSecretRotateInput) (OAuthClientSecretResult, error) {
	input.TenantID, input.OAuthClientID, input.OperatorID = strings.TrimSpace(input.TenantID), strings.TrimSpace(input.OAuthClientID), strings.TrimSpace(input.OperatorID)
	if input.TenantID == "" || input.OAuthClientID == "" || input.OperatorID == "" || time.Duration(input.OverlapSeconds)*time.Second > maxRotationOverlap {
		return OAuthClientSecretResult{}, ErrManagementValidation
	}
	now := service.clock.Now().UTC()
	validUntil := normalizeSecretValidUntil(input.ValidUntil)
	if !validSecretValidUntil(validUntil, now) {
		return OAuthClientSecretResult{}, ErrManagementValidation
	}
	write, plaintext, err := service.newSecretWrite(now, validUntil)
	if err != nil {
		return OAuthClientSecretResult{}, err
	}
	credential, err := service.repository.RotateOAuthClientSecret(ctx, input, write, now)
	if err != nil {
		return OAuthClientSecretResult{}, err
	}
	return OAuthClientSecretResult{Credential: credential, PlaintextSecret: plaintext}, nil
}

// DisableOAuthClientSecret 立即吊销单个 Secret 版本，不产生重叠窗口；
// 用于凭据疑似泄露时的紧急止损。
func (service *OAuthClientManagementService) DisableOAuthClientSecret(ctx context.Context, input OAuthClientSecretDisableInput) error {
	input.TenantID, input.OAuthClientID, input.CredentialID, input.OperatorID = strings.TrimSpace(input.TenantID), strings.TrimSpace(input.OAuthClientID), strings.TrimSpace(input.CredentialID), strings.TrimSpace(input.OperatorID)
	if input.TenantID == "" || input.OAuthClientID == "" || input.CredentialID == "" || input.OperatorID == "" {
		return ErrManagementValidation
	}
	return service.repository.DisableOAuthClientSecret(ctx, input, service.clock.Now().UTC())
}

func (service *OAuthClientManagementService) newSecretWrite(now time.Time, validUntil *time.Time) (SecretWrite, string, error) {
	return newOAuthClientSecretWrite(service.ids, now, validUntil)
}

// newOAuthClientSecretWrite 统一独立客户端管理与子系统接入的密钥生成规则。返回值刻意拆分为
// 可持久化哈希写模型和一次性明文，防止仓储接口获得原始 secret。
func newOAuthClientSecretWrite(ids ManagementIdentifierGenerator, now time.Time, validUntil *time.Time) (SecretWrite, string, error) {
	credentialID, err := ids.New(now)
	if err != nil {
		return SecretWrite{}, "", fmt.Errorf("generate OAuth client credential id: %w", err)
	}
	plaintext, err := generateClientSecret()
	if err != nil {
		return SecretWrite{}, "", fmt.Errorf("generate OAuth client secret: %w", err)
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(plaintext), bcrypt.DefaultCost)
	if err != nil {
		return SecretWrite{}, "", fmt.Errorf("hash OAuth client secret: %w", err)
	}
	return SecretWrite{CredentialID: credentialID, SecretHash: hash, Fingerprint: secretFingerprint(plaintext), ValidUntil: validUntil}, plaintext, nil
}

func normalizeOAuthClientCreate(input OAuthClientCreateInput, redirectURIValidationPolicy RedirectURIValidationPolicy) (OAuthClientCreateInput, error) {
	input.TenantID, input.ApplicationID, input.EnvironmentID, input.OperatorID = strings.TrimSpace(input.TenantID), strings.TrimSpace(input.ApplicationID), strings.TrimSpace(input.EnvironmentID), strings.TrimSpace(input.OperatorID)
	input.ClientID, input.ClientName = strings.TrimSpace(input.ClientID), strings.TrimSpace(input.ClientName)
	input.ClientType, input.TokenAuthMethod = strings.TrimSpace(input.ClientType), strings.TrimSpace(input.TokenAuthMethod)
	input.GrantTypes, input.Scopes, input.RedirectURIs = normalizeStrings(input.GrantTypes), normalizeStrings(input.Scopes), normalizeStrings(input.RedirectURIs)
	input.SecretValidUntil = normalizeSecretValidUntil(input.SecretValidUntil)
	if input.TenantID == "" || input.ApplicationID == "" || input.EnvironmentID == "" || input.OperatorID == "" ||
		!clientIDPattern.MatchString(input.ClientID) || !validOAuthClientName(input.ClientName) ||
		!validClientConfiguration(input) || !validScopes(input.Scopes) || !validRedirectURIs(input.RedirectURIs, redirectURIValidationPolicy) {
		return OAuthClientCreateInput{}, ErrManagementValidation
	}
	return input, nil
}

func validClientConfiguration(input OAuthClientCreateInput) bool {
	if !oneOf(input.ClientType, "public", "confidential", "service") || !oneOf(input.TokenAuthMethod, "none", "client_secret_basic", "private_key_jwt") ||
		input.AccessTokenTTLSeconds < 60 || input.AccessTokenTTLSeconds > 86400 || input.RefreshTokenTTLSeconds > 30*24*60*60 || !validGrantTypes(input.GrantTypes) {
		return false
	}
	hasAuthorizationCode, hasClientCredentials := contains(input.GrantTypes, "authorization_code"), contains(input.GrantTypes, "client_credentials")
	// 普通浏览器授权码客户端必须启用 PKCE。两个服务器托管的 Keycloak Broker
	// 是受控例外：Keycloak Broker 当前不会向上游发出 PKCE challenge，强制开启
	// 会导致 Broker 回调无法完成；例外同时限定客户端身份、类型和认证方式。
	if hasAuthorizationCode && ((!input.RequirePKCE && !isServerManagedBrokerClient(input)) || len(input.RedirectURIs) == 0) {
		return false
	}
	if hasClientCredentials && (input.ClientType == "public" || !oneOf(input.TokenAuthMethod, "client_secret_basic", "private_key_jwt")) {
		return false
	}
	if input.TokenAuthMethod == "none" && input.ClientType != "public" {
		return false
	}
	if (input.TokenAuthMethod == "client_secret_basic" || input.TokenAuthMethod == "private_key_jwt") && input.ClientType == "public" {
		return false
	}
	if input.TokenAuthMethod != "client_secret_basic" && input.SecretValidUntil != nil {
		return false
	}
	return true
}

func isServerManagedBrokerClient(input OAuthClientCreateInput) bool {
	return (input.ClientID == "keycloak-broker" || input.ClientID == "keycloak-customer-portal-broker") &&
		input.ClientType == "confidential" && input.TokenAuthMethod == "client_secret_basic" &&
		contains(input.GrantTypes, "authorization_code") && !contains(input.GrantTypes, "client_credentials")
}

func validGrantTypes(values []string) bool {
	if len(values) == 0 {
		return false
	}
	for _, value := range values {
		if !oneOf(value, "authorization_code", "client_credentials", "refresh_token") {
			return false
		}
	}
	return true
}

func validScopes(values []string) bool {
	for _, value := range values {
		if !scopePattern.MatchString(value) {
			return false
		}
	}
	return true
}

func validRedirectURIs(values []string, policy RedirectURIValidationPolicy) bool {
	for _, value := range values {
		if len(value) > 2048 || !validRedirectURI(value, policy) {
			return false
		}
	}
	return true
}

// validRedirectURI 接受绝对 HTTP(S) 回调地址。授权端点仍要求与客户端登记的 URI
// 精确匹配，因此这里的宽松解析不会把回调目标变成开放重定向；回环之外的 HTTP 回调
// 必须经进程配置显式开启，否则授权码将穿越不安全信道。
func validRedirectURI(value string, policy RedirectURIValidationPolicy) bool {
	parsed, err := url.Parse(value)
	if err != nil || !parsed.IsAbs() || parsed.Host == "" || parsed.User != nil || parsed.Fragment != "" {
		return false
	}
	if strings.EqualFold(parsed.Scheme, "https") {
		return true
	}
	if !strings.EqualFold(parsed.Scheme, "http") {
		return false
	}
	if policy.AllowInsecureHTTP {
		return true
	}
	host := parsed.Hostname()
	return strings.EqualFold(host, "localhost") || net.ParseIP(host).IsLoopback()
}

func normalizeStrings(values []string) []string {
	seen := make(map[string]struct{}, len(values))
	result := make([]string, 0, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" {
			continue
		}
		if _, exists := seen[value]; exists {
			continue
		}
		seen[value] = struct{}{}
		result = append(result, value)
	}
	sort.Strings(result)
	return result
}

func normalizeSecretValidUntil(value *time.Time) *time.Time {
	if value == nil {
		return nil
	}
	utc := value.UTC()
	return &utc
}

func validSecretValidUntil(value *time.Time, now time.Time) bool {
	return value == nil || (value.After(now) && value.Sub(now) <= maxClientSecretLifetime)
}

func validOAuthClientName(value string) bool {
	length := utf8.RuneCountInString(value)
	return length >= 1 && length <= 128
}

func generateClientSecret() (string, error) {
	bytes := make([]byte, clientSecretRandomBytes)
	if _, err := rand.Read(bytes); err != nil {
		return "", err
	}
	return clientSecretPrefix + base64.RawURLEncoding.EncodeToString(bytes), nil
}

func secretFingerprint(secret string) string {
	digest := sha256.Sum256([]byte(secret))
	return hex.EncodeToString(digest[:])
}

func oneOf(value string, allowed ...string) bool {
	for _, candidate := range allowed {
		if value == candidate {
			return true
		}
	}
	return false
}

func contains(values []string, target string) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}

var privateJWKMembers = []string{"d", "k", "p", "q", "dp", "dq", "qi", "oth"}

var allowedPublicJWKMembers = map[string]struct{}{
	"alg": {}, "crv": {}, "e": {}, "kid": {}, "kty": {}, "n": {}, "use": {}, "x": {}, "y": {},
}

// normalizeOAuthClientJWKs 校验并规范化完整的公钥 JWK 集合。
// 它保留未来生效的轮换密钥，同时拒绝格式错误和值中包含的私钥材料。
func normalizeOAuthClientJWKs(values []OAuthClientJWK, now time.Time, ids ManagementIdentifierGenerator) ([]OAuthClientJWK, error) {
	result := make([]OAuthClientJWK, 0, len(values))
	seenKeyIDs := make(map[string]struct{}, len(values))
	for _, value := range values {
		value.KeyID = strings.TrimSpace(value.KeyID)
		value.Algorithm = strings.TrimSpace(value.Algorithm)
		if !validOAuthClientJWKKeyID(value.KeyID) {
			return nil, ErrManagementValidation
		}
		if _, exists := seenKeyIDs[value.KeyID]; exists {
			return nil, ErrManagementValidation
		}
		seenKeyIDs[value.KeyID] = struct{}{}

		validFrom := value.ValidFrom.UTC()
		if value.ValidFrom.IsZero() {
			validFrom = now
		}
		validUntil := normalizeSecretValidUntil(value.ValidUntil)
		if validUntil != nil && !validUntil.After(validFrom) {
			return nil, ErrManagementValidation
		}
		publicJWK, algorithm, err := validateAndCanonicalizePublicJWK(value.PublicJWK, value.KeyID, value.Algorithm)
		if err != nil {
			return nil, ErrManagementValidation
		}
		id, err := ids.New(now)
		if err != nil {
			return nil, fmt.Errorf("generate OAuth client JWK id: %w", err)
		}
		result = append(result, OAuthClientJWK{ID: id, KeyID: value.KeyID, PublicJWK: publicJWK, Algorithm: algorithm, ValidFrom: validFrom, ValidUntil: validUntil, Status: credentialStatusActive})
	}
	sort.Slice(result, func(i, j int) bool { return result[i].KeyID < result[j].KeyID })
	return result, nil
}

func validOAuthClientJWKKeyID(value string) bool {
	if value == "" || len(value) > 255 || strings.TrimSpace(value) != value {
		return false
	}
	for _, character := range value {
		if character < 0x20 || character == 0x7f {
			return false
		}
	}
	return true
}

func validateAndCanonicalizePublicJWK(raw json.RawMessage, expectedKeyID, requestedAlgorithm string) (json.RawMessage, string, error) {
	// 登记阶段不仅检查 JSON 结构，还实际解析曲线点、RSA 模数等公钥材料并重新编码为规范 JSON；
	// 私钥成员、算法不匹配或不可验签的 key_ops 均失败关闭，运行时无需容忍多种等价表示。
	var values map[string]json.RawMessage
	if len(raw) == 0 || json.Unmarshal(raw, &values) != nil || values == nil {
		return nil, "", errors.New("invalid JWK")
	}
	for member := range values {
		if contains(privateJWKMembers, member) {
			return nil, "", errors.New("private JWK member")
		}
		if _, allowed := allowedPublicJWKMembers[member]; !allowed {
			return nil, "", errors.New("unsupported JWK member")
		}
	}
	keyID, ok := jwkString(values, "kid")
	if !ok || keyID != expectedKeyID {
		return nil, "", errors.New("JWK kid mismatch")
	}
	keyType, ok := jwkString(values, "kty")
	if !ok {
		return nil, "", errors.New("missing JWK kty")
	}
	registeredAlgorithm, hasRegisteredAlgorithm := jwkString(values, "alg")
	if hasRegisteredAlgorithm && !supportedOAuthClientJWKAlgorithm(registeredAlgorithm) {
		return nil, "", errors.New("unsupported JWK algorithm")
	}
	if requestedAlgorithm != "" && !supportedOAuthClientJWKAlgorithm(requestedAlgorithm) {
		return nil, "", errors.New("unsupported requested algorithm")
	}
	if requestedAlgorithm != "" && hasRegisteredAlgorithm && requestedAlgorithm != registeredAlgorithm {
		return nil, "", errors.New("JWK algorithm mismatch")
	}
	algorithm := requestedAlgorithm
	if algorithm == "" {
		algorithm = registeredAlgorithm
	}
	if err := validateOAuthClientPublicJWK(keyType, values, algorithm); err != nil {
		return nil, "", err
	}
	canonical, err := json.Marshal(values)
	if err != nil {
		return nil, "", err
	}
	return json.RawMessage(canonical), algorithm, nil
}

func validateOAuthClientPublicJWK(keyType string, values map[string]json.RawMessage, algorithm string) error {
	value := func(name string) (string, bool) { return jwkString(values, name) }
	decode := func(encoded string) ([]byte, error) { return base64.RawURLEncoding.DecodeString(encoded) }
	compatible := func(allowed ...string) bool { return algorithm == "" || oneOf(algorithm, allowed...) }
	switch keyType {
	case "OKP":
		curve, hasCurve := value("crv")
		x, hasX := value("x")
		if !hasCurve || curve != "Ed25519" || !hasX || !compatible("EdDSA") {
			return errors.New("invalid OKP JWK")
		}
		bytes, err := decode(x)
		if err != nil || len(bytes) != ed25519.PublicKeySize {
			return errors.New("invalid Ed25519 key")
		}
	case "RSA":
		modulus, hasModulus := value("n")
		exponent, hasExponent := value("e")
		if !hasModulus || !hasExponent || !compatible("RS256", "RS384", "RS512", "PS256", "PS384", "PS512") {
			return errors.New("invalid RSA JWK")
		}
		modulusBytes, modulusErr := decode(modulus)
		exponentBytes, exponentErr := decode(exponent)
		exponentNumber := new(big.Int).SetBytes(exponentBytes)
		if modulusErr != nil || exponentErr != nil || len(modulusBytes) < 256 || !exponentNumber.IsInt64() || exponentNumber.Int64() < 3 || exponentNumber.Int64()%2 == 0 {
			return errors.New("invalid RSA key")
		}
	case "EC":
		curveName, hasCurve := value("crv")
		x, hasX := value("x")
		y, hasY := value("y")
		curve, coordinateLength, expectedAlgorithm := oauthClientJWKCurve(curveName)
		if !hasCurve || !hasX || !hasY || curve == nil || (algorithm != "" && algorithm != expectedAlgorithm) {
			return errors.New("invalid EC JWK")
		}
		xBytes, xErr := decode(x)
		yBytes, yErr := decode(y)
		publicX, publicY := new(big.Int).SetBytes(xBytes), new(big.Int).SetBytes(yBytes)
		if xErr != nil || yErr != nil || len(xBytes) != coordinateLength || len(yBytes) != coordinateLength || !curve.IsOnCurve(publicX, publicY) {
			return errors.New("invalid EC key")
		}
	default:
		return errors.New("unsupported JWK")
	}
	return nil
}

func oauthClientJWKCurve(name string) (elliptic.Curve, int, string) {
	switch name {
	case "P-256":
		return elliptic.P256(), 32, "ES256"
	case "P-384":
		return elliptic.P384(), 48, "ES384"
	case "P-521":
		return elliptic.P521(), 66, "ES512"
	default:
		return nil, 0, ""
	}
}

func supportedOAuthClientJWKAlgorithm(value string) bool {
	return oneOf(value, "EdDSA", "RS256", "RS384", "RS512", "PS256", "PS384", "PS512", "ES256", "ES384", "ES512")
}

func jwkString(values map[string]json.RawMessage, name string) (string, bool) {
	raw, exists := values[name]
	if !exists {
		return "", false
	}
	var value string
	if json.Unmarshal(raw, &value) != nil || value == "" {
		return "", false
	}
	return value, true
}
