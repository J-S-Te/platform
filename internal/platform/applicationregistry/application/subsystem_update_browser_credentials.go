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

// SubsystemUpdateBrowserCredential 是本次更新要下发给运行时的浏览器客户端凭据（明文仅内存）。
type SubsystemUpdateBrowserCredential struct {
	ClientID     string
	ClientSecret string
}

// SubsystemUpdateBrowserCredentialsService 按既有规则重建浏览器客户端凭据：
// 找到规范名客户端 → 按 round 43/48 的期望校验 → 签发新密钥（带期望版本号，避免丢失更新）。
// 找不到客户端时明确报错（创建客户端仍由旧路径承担），不静默跳过。
type SubsystemUpdateBrowserCredentialsService struct {
	clients SubsystemUpdateBrowserCredentialPort
}

// NewSubsystemUpdateBrowserCredentialsService 构造服务。
func NewSubsystemUpdateBrowserCredentialsService(clients SubsystemUpdateBrowserCredentialPort) (*SubsystemUpdateBrowserCredentialsService, error) {
	if clients == nil {
		return nil, errors.New("subsystem update browser credentials requires an OAuth client port")
	}
	return &SubsystemUpdateBrowserCredentialsService{clients: clients}, nil
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
	return SubsystemUpdateBrowserCredential{ClientID: client.ClientID, ClientSecret: secret.PlaintextSecret}, nil
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
