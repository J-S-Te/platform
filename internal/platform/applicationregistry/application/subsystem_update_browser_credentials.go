package application

import (
	"context"
	"errors"
	"fmt"
	"strings"
)

// SubsystemUpdateBrowserCredentialPort 是浏览器凭据重建所需的既有 OAuth 客户端能力。
type SubsystemUpdateBrowserCredentialPort interface {
	ListOAuthClients(ctx context.Context, tenantID string) ([]OAuthClientView, error)
	CreateOAuthClientSecret(ctx context.Context, input OAuthClientSecretCreateInput) (OAuthClientSecretResult, error)
	CreateOAuthClient(ctx context.Context, input OAuthClientCreateInput) (OAuthClientCreateResult, error)
}

// SubsystemUpdateBackchannelLogoutPort 是受控更新路径的 Back-Channel Logout 回填端：
// 由组合根适配到既有 application.BackchannelLogoutURIRepository（其内部复用
// UpsertBackchannelLogoutURIWithin 与 ValidateBackchannelLogoutURI）。应用层不直接写
// 登记表，避免绕过租户范围与协议校验（BIZ-XEVT2-07）。
type SubsystemUpdateBackchannelLogoutPort interface {
	// ReadBackchannelLogoutURI 返回客户端已登记的端点；found=false 表示尚未登记。
	ReadBackchannelLogoutURI(ctx context.Context, tenantID, oauthClientID string) (string, bool, error)
	// UpsertBackchannelLogoutURI 幂等登记端点（ON DUPLICATE KEY UPDATE 语义）。
	UpsertBackchannelLogoutURI(ctx context.Context, tenantID, operatorID, oauthClientID, uri string) error
}

// SubsystemUpdateBrowserCredential 是本次更新要下发给运行时的浏览器客户端凭据（明文仅内存）。
type SubsystemUpdateBrowserCredential struct {
	ClientID     string
	ClientSecret string
}

// SubsystemUpdateBrowserCredentialsService 按既有规则重建浏览器客户端凭据：
// 找到规范名客户端 → 按 round 43/48 的期望校验 → 签发新密钥（带期望版本号，避免丢失更新）。
// 找不到客户端时明确报错（创建客户端仍由旧路径承担），不静默跳过。
//
// 自 BIZ-XEVT2-07 起，凭据就绪后还会为同一客户端幂等补登记 Back-Channel Logout 端点：
// 此前登记只能靠接入时自动写或人工调用管理 API，漏登记的存量环境在更新后仍然收不到
// 登出通知。回填端未接线时保持既有语义（只重建凭据）。
type SubsystemUpdateBrowserCredentialsService struct {
	clients SubsystemUpdateBrowserCredentialPort
	// backchannel 是回填端；nil 表示该组合根未接线（不做补登记）。
	backchannel SubsystemUpdateBackchannelLogoutPort
	// backchannelLogoutPath 与接入使用同一条规则：nil=默认端点，
	// 显式空串=调用方声明该 RP 不支持 Back-Channel Logout（opt-out）。
	backchannelLogoutPath *string
}

// SubsystemUpdateBrowserCredentialsOption 调整凭据服务的可选行为，不改变既有构造调用。
type SubsystemUpdateBrowserCredentialsOption func(*SubsystemUpdateBrowserCredentialsService)

// WithBackchannelLogoutBackfill 注入 Back-Channel Logout 回填端；port 为 nil 时等于未接线
// （保持既有语义：只重建凭据，不写登记）。接线后写失败会向上返回错误：更新不允许产出
// "凭据已下发但登出通知永远送不到"且无人知晓的半成品。
func WithBackchannelLogoutBackfill(port SubsystemUpdateBackchannelLogoutPort) SubsystemUpdateBrowserCredentialsOption {
	return func(service *SubsystemUpdateBrowserCredentialsService) {
		service.backchannel = port
	}
}

// WithBackchannelLogoutPath 声明回填使用的端点路径，语义与接入完全一致：nil 取默认
// 路径（/auth/backchannel-logout），显式空串表示该 RP 不支持 Back-Channel Logout
// （opt-out，不改写也不删除既有登记），其它值按相对路径归一化。
func WithBackchannelLogoutPath(path *string) SubsystemUpdateBrowserCredentialsOption {
	return func(service *SubsystemUpdateBrowserCredentialsService) {
		service.backchannelLogoutPath = normalizeBackchannelLogoutPath(path)
	}
}

// NewSubsystemUpdateBrowserCredentialsService 构造服务。
func NewSubsystemUpdateBrowserCredentialsService(clients SubsystemUpdateBrowserCredentialPort, options ...SubsystemUpdateBrowserCredentialsOption) (*SubsystemUpdateBrowserCredentialsService, error) {
	if clients == nil {
		return nil, errors.New("subsystem update browser credentials requires an OAuth client port")
	}
	service := &SubsystemUpdateBrowserCredentialsService{clients: clients}
	for _, option := range options {
		if option != nil {
			option(service)
		}
	}
	if service.backchannelLogoutPath == nil {
		service.backchannelLogoutPath = normalizeBackchannelLogoutPath(nil)
	}
	return service, nil
}

// Ensure 返回本次更新使用的浏览器客户端凭据。
func (service *SubsystemUpdateBrowserCredentialsService) Ensure(ctx context.Context, tenantID, operatorID, applicationID, environmentID, applicationCode, environment, redirectURI string) (SubsystemUpdateBrowserCredential, error) {
	tenantID = strings.TrimSpace(tenantID)
	if tenantID == "" || strings.TrimSpace(applicationCode) == "" || strings.TrimSpace(environment) == "" {
		return SubsystemUpdateBrowserCredential{}, fmt.Errorf("browser credentials require tenant, application and environment: %w", ErrValidation)
	}
	clients, err := service.clients.ListOAuthClients(ctx, tenantID)
	if err != nil {
		return SubsystemUpdateBrowserCredential{}, err
	}
	name := SubsystemUpdateBrowserClientName(applicationCode, environment)
	var found *OAuthClientView
	for index := range clients {
		if clients[index].ClientID != name {
			continue
		}
		if found != nil {
			return SubsystemUpdateBrowserCredential{}, ErrSubsystemUpdateServiceClientInvalid
		}
		found = &clients[index]
	}
	if found == nil {
		// 与既有实现一致：缺失时按既定字段创建（保密客户端 + PKCE + authorization_code）。
		created, createErr := service.clients.CreateOAuthClient(ctx, OAuthClientCreateInput{
			TenantID: tenantID, ApplicationID: strings.TrimSpace(applicationID), EnvironmentID: strings.TrimSpace(environmentID),
			OperatorID: strings.TrimSpace(operatorID), ClientID: name, ClientName: applicationCode + " Web",
			ClientType: "confidential", TokenAuthMethod: "client_secret_basic", AccessTokenTTLSeconds: 15 * 60,
			RequirePKCE: true, GrantTypes: []string{"authorization_code", "refresh_token"},
			Scopes: []string{"openid", "profile"}, RedirectURIs: []string{strings.TrimSpace(redirectURI)},
		})
		if createErr != nil {
			return SubsystemUpdateBrowserCredential{}, createErr
		}
		if strings.TrimSpace(created.PlaintextSecret) == "" {
			return SubsystemUpdateBrowserCredential{}, errors.New("browser credential secret was not issued")
		}
		// 新建客户端同样补齐登出端点：否则更新完成后该 RP 仍处于"有登录状态但无登出登记"。
		if backfillErr := service.backfillBackchannelLogoutURI(ctx, tenantID, operatorID, created.Client.ID, redirectURI); backfillErr != nil {
			return SubsystemUpdateBrowserCredential{}, backfillErr
		}
		return SubsystemUpdateBrowserCredential{ClientID: created.Client.ClientID, ClientSecret: created.PlaintextSecret}, nil
	}
	client := *found
	if err := ValidateSubsystemUpdateServiceClient(client, SubsystemUpdateBrowserClientExpectation(tenantID, applicationID, environmentID, applicationCode, environment, redirectURI)); err != nil {
		return SubsystemUpdateBrowserCredential{}, err
	}
	version := client.Version
	secret, err := service.clients.CreateOAuthClientSecret(ctx, OAuthClientSecretCreateInput{
		TenantID: tenantID, OAuthClientID: client.ID, OperatorID: strings.TrimSpace(operatorID),
		ExpectedClientVersion: &version,
	})
	if err != nil {
		return SubsystemUpdateBrowserCredential{}, err
	}
	if strings.TrimSpace(secret.PlaintextSecret) == "" {
		return SubsystemUpdateBrowserCredential{}, errors.New("browser credential secret was not issued")
	}
	if err := service.backfillBackchannelLogoutURI(ctx, tenantID, operatorID, client.ID, redirectURI); err != nil {
		return SubsystemUpdateBrowserCredential{}, err
	}
	return SubsystemUpdateBrowserCredential{ClientID: client.ClientID, ClientSecret: secret.PlaintextSecret}, nil
}

// backfillBackchannelLogoutURI 为浏览器客户端幂等补登记 Back-Channel Logout 端点。
//
// 语义选择（与接入路径一致、但不改动核心接入逻辑）：
//   - 回填端未接线：直接返回，保持既有"只重建凭据"的行为，缺口由 /metrics 的
//     platform_oauth_backchannel_unregistered_rp 暴露。
//   - 显式 opt-out（空路径）：不写登记、也不删除既有登记——删除是不可逆的安全相关
//     变更，必须走带审计的接入/管理 API 流程，不能在凭据更新里静默发生。
//   - 已有登记：原样保留（人工登记的定制端点不被默认路径覆盖），仅补"完全没有登记"的客户端。
//   - 推导出的端点未通过协议校验（例如明文 HTTP 非回环）：跳过登记但不阻断更新，
//     与接入路径的"跳过并留痕"一致；此处没有审计通道，缺口由监控 gauge 兜底。
//   - 读/写失败：向上返回错误（失败关闭），不把 DB 故障吞成"已补登记"。
func (service *SubsystemUpdateBrowserCredentialsService) backfillBackchannelLogoutURI(ctx context.Context, tenantID, operatorID, oauthClientID, redirectURI string) error {
	if service == nil || service.backchannel == nil || strings.TrimSpace(oauthClientID) == "" {
		return nil
	}
	path := defaultBackchannelLogoutPath
	if service.backchannelLogoutPath != nil {
		path = strings.TrimSpace(*service.backchannelLogoutPath)
	}
	if path == "" {
		// 显式 opt-out：不改写、不删除既有登记。
		return nil
	}
	candidate, err := backchannelLogoutURIFromRedirect(redirectURI, path)
	if err != nil {
		// 无法从规范回调地址推导同源端点时不做任何写入；凭据重建本身不应因此失败。
		return nil
	}
	// HTTPS 校验不放宽：与接入路径复用同一实现，且不接受任何明文 HTTP 例外。
	if err := ValidateBackchannelLogoutURI(candidate, false); err != nil {
		return nil
	}
	if _, found, err := service.backchannel.ReadBackchannelLogoutURI(ctx, tenantID, oauthClientID); err != nil {
		return fmt.Errorf("read back-channel logout URI: %w", err)
	} else if found {
		// 已有登记（含人工登记的定制端点）不被覆盖：回填只补缺失。
		return nil
	}
	if err := service.backchannel.UpsertBackchannelLogoutURI(ctx, tenantID, strings.TrimSpace(operatorID), oauthClientID, candidate); err != nil {
		return fmt.Errorf("register back-channel logout URI: %w", err)
	}
	return nil
}

// backchannelLogoutURIFromRedirect 从规范回调地址推导同源登出端点：回调固定为
// <public-origin><path-prefix>/auth/callback，登出端点在同一 origin+prefix 之后追加
// 接入规则给出的路径。推导失败返回 ErrValidation，调用方据此跳过而不是猜测地址。
func backchannelLogoutURIFromRedirect(redirectURI, logoutPath string) (string, error) {
	const callbackSuffix = "/auth/callback"
	base := strings.TrimSpace(redirectURI)
	if !strings.HasSuffix(base, callbackSuffix) {
		return "", ErrValidation
	}
	base = strings.TrimSuffix(base, callbackSuffix)
	if base == "" || !strings.HasPrefix(logoutPath, "/") {
		return "", ErrValidation
	}
	return base + logoutPath, nil
}

// ReadSubsystemAccessBrowserClientID 恢复认证投影所需的非敏感引用，禁止创建或轮换密钥。
func (service *SubsystemUpdateBrowserCredentialsService) ReadSubsystemAccessBrowserClientID(ctx context.Context, tenantID, applicationID, environmentID, applicationCode, environment string) (string, error) {
	clients, err := service.clients.ListOAuthClients(ctx, tenantID)
	if err != nil {
		return "", err
	}
	expectation := SubsystemUpdateBrowserClientExpectation(tenantID, applicationID, environmentID, applicationCode, environment, "")
	var found *OAuthClientView
	for i := range clients {
		if clients[i].ClientID != expectation.CanonicalName {
			continue
		}
		if found != nil {
			return "", ErrSubsystemUpdateServiceClientInvalid
		}
		found = &clients[i]
	}
	if found == nil {
		return "", ErrSubsystemUpdateServiceClientInvalid
	}
	if err := ValidateSubsystemUpdateServiceClient(*found, expectation); err != nil {
		return "", err
	}
	return found.ClientID, nil
}
