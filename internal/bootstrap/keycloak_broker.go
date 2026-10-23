package bootstrap

import (
	"context"
	"errors"
	"fmt"

	application "github.com/J-S-Te/Basic-Platform/internal/platform/applicationregistry/application"
)

// keycloakBrokerRegistrar 将平台 OAuth 客户端生命周期限制在平台控制面内。
// Keycloak 只在内存中接收密钥；密钥不会序列化到浏览器或环境文件。
type keycloakBrokerRegistrar struct {
	applications *application.ManagementService
	oauth        *application.OAuthClientManagementService
	publicURL    string
	realm        string
	environment  string
}

const (
	platformBrokerClientID       = "keycloak-broker"
	customerPortalBrokerClientID = "keycloak-customer-portal-broker"
)

// errBrokerTargetNotRegistered 表示 Broker 依赖的应用或环境尚未在平台注册。
// 平台 Broker 依赖内置的 platform 应用，缺失即属于真实故障；客户门户是按需接入的
// 可选子系统，尚未注册时调用方应跳过而不是阻断 Worker/API 启动。
var errBrokerTargetNotRegistered = errors.New("broker target application or environment is not registered")

func keycloakBrokerEnvironment(appEnvironment string) string {
	if appEnvironment == "production" {
		return "prod"
	}
	return "dev"
}

// EnsureKeycloakBroker 确保平台 Broker OAuth client 存在并返回其凭据。
func (registrar keycloakBrokerRegistrar) EnsureKeycloakBroker(ctx context.Context, tenantID string) (string, string, error) {
	return registrar.ensureBrokerClient(ctx, tenantID, "platform", platformBrokerClientID, "Keycloak Broker")
}

// RecoverKeycloakBroker 在 Keycloak 丢失不可取回的 client secret 时，经 OAuth
// 控制面轮换平台 Broker client。明文仅用于紧随其后的 IdP 对账，
// 绝不持久化或写日志。
func (registrar keycloakBrokerRegistrar) RecoverKeycloakBroker(ctx context.Context, tenantID, operatorID string) (string, string, error) {
	return registrar.recoverBrokerClient(ctx, tenantID, operatorID, platformBrokerClientID)
}

// RecoverCustomerPortalBroker 用其平台侧 OAuth client 凭据修复缺失的门户
// Broker IdP，仅在定性漂移后才轮换。
func (registrar keycloakBrokerRegistrar) RecoverCustomerPortalBroker(ctx context.Context, tenantID, operatorID string) (string, string, error) {
	return registrar.recoverBrokerClient(ctx, tenantID, operatorID, customerPortalBrokerClientID)
}

// EnsureCustomerPortalBroker 为外部客户认证开通独立的上游 OAuth client。
// 它绝不能与平台 Broker client 共用：OAuth 授权解析器依据 client 所属应用推导
// claims，共用会让员工或客户在对方应用的登录流程中 fail-closed。
func (registrar keycloakBrokerRegistrar) EnsureCustomerPortalBroker(ctx context.Context, tenantID string) (string, string, error) {
	return registrar.ensureBrokerClient(ctx, tenantID, "customer_portal", customerPortalBrokerClientID, "Customer Portal Keycloak Broker")
}

func (registrar keycloakBrokerRegistrar) recoverBrokerClient(ctx context.Context, tenantID, operatorID, clientID string) (string, string, error) {
	existing, err := registrar.oauth.GetOAuthClientByClientID(ctx, tenantID, clientID)
	if err != nil {
		return "", "", fmt.Errorf("lookup keycloak broker OAuth client for recovery: %w", err)
	}
	secret, err := registrar.oauth.RotateOAuthClientSecret(ctx, application.OAuthClientSecretRotateInput{
		TenantID: tenantID, OAuthClientID: existing.ID, OperatorID: operatorID,
		// 让旧凭据短暂保持有效，直到 Keycloak 收到新凭据。
		// 这使 Keycloak Admin 的瞬时失败不具破坏性。
		OverlapSeconds: 300,
	})
	if err != nil {
		return "", "", fmt.Errorf("rotate keycloak broker OAuth client secret: %w", err)
	}
	return existing.ClientID, secret.PlaintextSecret, nil
}

func (registrar keycloakBrokerRegistrar) ensureBrokerClient(ctx context.Context, tenantID, applicationCode, clientID, clientName string) (string, string, error) {
	apps, err := registrar.applications.ListApplications(ctx, tenantID, application.PageRequest{Page: 1, PageSize: 100})
	if err != nil {
		return "", "", err
	}
	var targetApplication application.Application
	for _, item := range apps.Items {
		if item.Code == applicationCode {
			targetApplication = item
			break
		}
	}
	if targetApplication.ID == "" {
		return "", "", fmt.Errorf("%s application is not registered: %w", applicationCode, errBrokerTargetNotRegistered)
	}
	envs, err := registrar.applications.ListEnvironments(ctx, tenantID, targetApplication.ID, application.PageRequest{Page: 1, PageSize: 100})
	if err != nil {
		return "", "", err
	}
	var environment application.Environment
	environmentCode := registrar.environment
	if environmentCode == "" {
		environmentCode = "dev"
	}
	for _, item := range envs.Items {
		if item.Environment == environmentCode {
			environment = item
			break
		}
	}
	if environment.ID == "" {
		return "", "", fmt.Errorf("%s %s environment is not registered: %w", applicationCode, environmentCode, errBrokerTargetNotRegistered)
	}
	brokerAlias := "basic-platform"
	if applicationCode == "customer_portal" {
		brokerAlias = "basic-platform-customer"
	}
	redirect := registrar.publicURL + "/realms/" + registrar.realm + "/broker/" + brokerAlias + "/endpoint"
	// 用 client_id 直接查找代替全租户扫描，避免 OAuth client 很多时
	// 每次 worker 重启都产生 O(n) 查询。
	existing, lookupErr := registrar.oauth.GetOAuthClientByClientID(ctx, tenantID, clientID)
	if lookupErr == nil {
		// 仅当 client 没有活动凭据时才轮换。此前每次 worker 重启都会轮换，
		// 在 Keycloak 中累积 bcrypt 哈希，并在中途使 IdP 配置失效。现在信任
		// 既有活动凭据仍有效；EnsureBroker 中的 Broker 对账会检测并修复
		// 损坏的 IdP 配置。
		hasActiveCredential := false
		for _, cred := range existing.Credentials {
			if cred.Status == "ACTIVE" && cred.RevokedAt == nil {
				hasActiveCredential = true
				break
			}
		}
		if !hasActiveCredential {
			secret, createErr := registrar.oauth.CreateOAuthClientSecret(ctx, application.OAuthClientSecretCreateInput{
				TenantID: tenantID, OAuthClientID: existing.ID, OperatorID: "system-keycloak",
			})
			if createErr != nil {
				return "", "", fmt.Errorf("create keycloak broker secret: %w", createErr)
			}
			return existing.ClientID, secret.PlaintextSecret, nil
		}
		return existing.ClientID, "", nil
	}
	if !errors.Is(lookupErr, application.ErrManagementNotFound) {
		return "", "", fmt.Errorf("lookup keycloak broker OAuth client: %w", lookupErr)
	}
	created, err := registrar.oauth.CreateOAuthClient(ctx, application.OAuthClientCreateInput{TenantID: tenantID, ApplicationID: targetApplication.ID, EnvironmentID: environment.ID, OperatorID: "system-keycloak", ClientID: clientID, ClientName: clientName, ClientType: "confidential", TokenAuthMethod: "client_secret_basic", AccessTokenTTLSeconds: 900, RefreshTokenTTLSeconds: 2592000, GrantTypes: []string{"authorization_code", "refresh_token"}, RequirePKCE: false, Scopes: []string{"openid", "profile"}, RedirectURIs: []string{redirect}})
	if err != nil {
		return "", "", fmt.Errorf("create keycloak broker OAuth client: %w", err)
	}
	return created.Client.ClientID, created.PlaintextSecret, nil
}
