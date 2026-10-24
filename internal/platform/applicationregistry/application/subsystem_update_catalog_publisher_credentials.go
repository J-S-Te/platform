package application

import (
	"context"
	"errors"
	"fmt"
	"strings"
)

// SubsystemUpdateCatalogPublisherPort 是目录发布者凭据所需的既有 OAuth 客户端能力。
type SubsystemUpdateCatalogPublisherPort interface {
	ListOAuthClients(ctx context.Context, tenantID string) ([]OAuthClientView, error)
	CreateOAuthClientSecret(ctx context.Context, input OAuthClientSecretCreateInput) (OAuthClientSecretResult, error)
	CreateOAuthClient(ctx context.Context, input OAuthClientCreateInput) (OAuthClientCreateResult, error)
}

// SubsystemUpdateCatalogPublisherCredential 是本次更新使用的目录发布者凭据（明文仅内存）。
type SubsystemUpdateCatalogPublisherCredential struct {
	ClientID     string
	ClientSecret string
}

// SubsystemUpdateCatalogPublisherCredentialsService 恢复目录发布者凭据：
// 与既有实现逐条一致 —— 规范名 <应用编码>-<环境>-catalog-publisher；
// 已存在时校验状态与归属后**总是**签发新密钥（密钥只写不可回读）；
// 不存在时按既有字段创建（service / client_secret_basic / TTL 900 / client_credentials / authorization.catalog.sync）。
type SubsystemUpdateCatalogPublisherCredentialsService struct {
	clients SubsystemUpdateCatalogPublisherPort
}

// NewSubsystemUpdateCatalogPublisherCredentialsService 构造服务。
func NewSubsystemUpdateCatalogPublisherCredentialsService(clients SubsystemUpdateCatalogPublisherPort) (*SubsystemUpdateCatalogPublisherCredentialsService, error) {
	if clients == nil {
		return nil, errors.New("subsystem update catalog publisher credentials requires an OAuth client port")
	}
	return &SubsystemUpdateCatalogPublisherCredentialsService{clients: clients}, nil
}

// CatalogPublisherClientName 返回既有实现使用的规范名。
func CatalogPublisherClientName(applicationCode, environment string) string {
	return applicationCode + "-" + environment + "-catalog-publisher"
}

// Ensure 返回目录发布者凭据（必要时创建客户端）。
func (service *SubsystemUpdateCatalogPublisherCredentialsService) Ensure(ctx context.Context, tenantID, operatorID, applicationID, environmentID, applicationCode, environment string) (SubsystemUpdateCatalogPublisherCredential, error) {
	tenantID = strings.TrimSpace(tenantID)
	if tenantID == "" || strings.TrimSpace(applicationID) == "" || strings.TrimSpace(environmentID) == "" || strings.TrimSpace(applicationCode) == "" || strings.TrimSpace(environment) == "" {
		return SubsystemUpdateCatalogPublisherCredential{}, fmt.Errorf("catalog publisher credentials require tenant, application, environment: %w", ErrValidation)
	}
	clientID := CatalogPublisherClientName(applicationCode, environment)
	clients, err := service.clients.ListOAuthClients(ctx, tenantID)
	if err != nil {
		return SubsystemUpdateCatalogPublisherCredential{}, err
	}
	for _, client := range clients {
		if client.ClientID != clientID {
			continue
		}
		if !strings.EqualFold(strings.TrimSpace(client.Status), "ACTIVE") ||
			client.ApplicationID != applicationID || client.EnvironmentID != environmentID {
			return SubsystemUpdateCatalogPublisherCredential{}, ErrSubsystemUpdateServiceClientInvalid
		}
		secret, secretErr := service.clients.CreateOAuthClientSecret(ctx, OAuthClientSecretCreateInput{
			TenantID: tenantID, OAuthClientID: client.ID, OperatorID: strings.TrimSpace(operatorID),
		})
		if secretErr != nil {
			return SubsystemUpdateCatalogPublisherCredential{}, secretErr
		}
		if strings.TrimSpace(secret.PlaintextSecret) == "" {
			return SubsystemUpdateCatalogPublisherCredential{}, errors.New("catalog publisher secret was not issued")
		}
		return SubsystemUpdateCatalogPublisherCredential{ClientID: client.ClientID, ClientSecret: secret.PlaintextSecret}, nil
	}
	created, createErr := service.clients.CreateOAuthClient(ctx, OAuthClientCreateInput{
		TenantID: tenantID, ApplicationID: applicationID, EnvironmentID: environmentID, OperatorID: strings.TrimSpace(operatorID),
		ClientID: clientID, ClientName: applicationCode + " Authorization Catalog Publisher", ClientType: "service",
		TokenAuthMethod: "client_secret_basic", AccessTokenTTLSeconds: 15 * 60,
		GrantTypes: []string{"client_credentials"}, Scopes: []string{"authorization.catalog.sync"},
	})
	if createErr != nil {
		return SubsystemUpdateCatalogPublisherCredential{}, createErr
	}
	if strings.TrimSpace(created.PlaintextSecret) == "" {
		return SubsystemUpdateCatalogPublisherCredential{}, errors.New("catalog publisher secret was not issued")
	}
	return SubsystemUpdateCatalogPublisherCredential{ClientID: created.Client.ClientID, ClientSecret: created.PlaintextSecret}, nil
}

// RequiresSubsystemCatalogPublisherCredential 判断该应用在受控更新时是否需要目录发布者凭据。
// 与既有实现逐条一致：六个已交付子系统需要，其它不需要。
func RequiresSubsystemCatalogPublisherCredential(applicationCode string) bool {
	switch strings.TrimSpace(applicationCode) {
	case "contract_management", "customer_and_opportunity", "customer_portal", "data_analysis", "project_management", "settlement":
		return true
	default:
		return false
	}
}
