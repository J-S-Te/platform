package application

import (
	"context"
	"errors"
	"fmt"
	"strings"
)

// SubsystemUpdateServiceCredentialPort 是更新凭据所需的既有 OAuth 客户端能力（窄接口）。
// 这些方法均已存在于既有 OAuth 客户端管理服务中；本片只负责按规则编排，不新增写语义。
type SubsystemUpdateServiceCredentialPort interface {
	ListOAuthClients(ctx context.Context, tenantID string) ([]OAuthClientView, error)
	ReplaceOAuthClientScopes(ctx context.Context, input OAuthClientScopesUpdateInput) (OAuthClientView, error)
	CreateOAuthClientSecret(ctx context.Context, input OAuthClientSecretCreateInput) (OAuthClientSecretResult, error)
	CreateOAuthClient(ctx context.Context, input OAuthClientCreateInput) (OAuthClientCreateResult, error)
}

// SubsystemUpdateServiceCredentialRequest 是一次受控更新的凭据准备请求。
type SubsystemUpdateServiceCredentialRequest struct {
	TenantID        string
	OperatorID      string
	ApplicationID   string
	EnvironmentID   string
	ApplicationCode string
	Environment     string
	// Operation 取 ADOPT / RETRY / UPDATE（大小写不敏感），用于轮换判定。
	Operation    string
	Requirements []SubsystemUpdateServiceCredentialRequirement
}

// SubsystemUpdateServiceCredentialsService 按既有规则准备受控更新所需的服务凭据。
// 覆盖「既有客户端已存在」分支：严格校验 → 需要时整体替换范围 → 需要时签发新密钥。
// 既有客户端缺失的分支（创建客户端）仍由旧路径承担，本服务会明确报错而不是静默跳过。
type SubsystemUpdateServiceCredentialsService struct {
	clients SubsystemUpdateServiceCredentialPort
}

// NewSubsystemUpdateServiceCredentialsService 构造服务。
func NewSubsystemUpdateServiceCredentialsService(clients SubsystemUpdateServiceCredentialPort) (*SubsystemUpdateServiceCredentialsService, error) {
	if clients == nil {
		return nil, errors.New("subsystem update service credentials requires an OAuth client port")
	}
	return &SubsystemUpdateServiceCredentialsService{clients: clients}, nil
}

// Ensure 返回本次更新应下发给 Agent 的服务凭据（含仅内存的明文）。
func (service *SubsystemUpdateServiceCredentialsService) Ensure(ctx context.Context, request SubsystemUpdateServiceCredentialRequest) ([]SubsystemServiceCredential, error) {
	tenantID := strings.TrimSpace(request.TenantID)
	if tenantID == "" || strings.TrimSpace(request.ApplicationCode) == "" || strings.TrimSpace(request.Environment) == "" {
		return nil, fmt.Errorf("update service credentials requires tenant, application and environment: %w", ErrValidation)
	}
	clients, err := service.clients.ListOAuthClients(ctx, tenantID)
	if err != nil {
		return nil, err
	}
	byClientID := make(map[string]OAuthClientView, len(clients))
	for _, client := range clients {
		byClientID[client.ClientID] = client
	}
	credentials := make([]SubsystemServiceCredential, 0, len(request.Requirements))
	for _, requirement := range request.Requirements {
		name := SubsystemUpdateServiceClientName(request.ApplicationCode, request.Environment, requirement.Suffix)
		desiredScopes := SubsystemUpdateServiceCredentialScopes(requirement)
		client, ok := byClientID[name]
		if !ok {
			// 既有客户端缺失：按既有字段创建（service / client_secret_basic / TTL 900 / client_credentials / 派生范围）。
			created, createErr := service.clients.CreateOAuthClient(ctx, OAuthClientCreateInput{
				TenantID: tenantID, ApplicationID: strings.TrimSpace(request.ApplicationID), EnvironmentID: strings.TrimSpace(request.EnvironmentID),
				OperatorID: strings.TrimSpace(request.OperatorID), ClientID: name, ClientName: requirement.ClientName,
				ClientType: "service", TokenAuthMethod: "client_secret_basic", AccessTokenTTLSeconds: 15 * 60,
				GrantTypes: []string{"client_credentials"}, Scopes: desiredScopes,
			})
			if createErr != nil {
				return nil, createErr
			}
			if strings.TrimSpace(created.PlaintextSecret) == "" {
				return nil, errors.New("service credential secret was not issued")
			}
			credentials = append(credentials, SubsystemServiceCredential{
				Purpose: requirement.Purpose, OAuthClient: created.Client, PlaintextSecret: created.PlaintextSecret,
			})
			continue
		}
		if RequiresStrictSubsystemUpdateServiceClientValidation(request.ApplicationCode, requirement.Purpose) {
			if err := ValidateSubsystemUpdateServiceClient(client, SubsystemUpdateServiceClientExpectation{
				CanonicalName: name, TenantID: tenantID, ApplicationID: strings.TrimSpace(request.ApplicationID),
				EnvironmentID: strings.TrimSpace(request.EnvironmentID), ClientType: "service",
				TokenAuthMethod: "client_secret_basic", GrantTypes: []string{"client_credentials"},
				// 注意：范围漂移是**可修复**项（下一步整体替换），不属于拒绝条件 —— 与既有实现一致。
			}); err != nil {
				return nil, err
			}
		}
		// 期望范围按用途派生（文件网关写入器含 bind/download），避免把该客户端"修复"成单范围。
		if ShouldRepairSubsystemUpdateScopes(client.Scopes, desiredScopes) {
			repaired, repairErr := service.clients.ReplaceOAuthClientScopes(ctx, OAuthClientScopesUpdateInput{
				TenantID: tenantID, OAuthClientID: client.ID, OperatorID: strings.TrimSpace(request.OperatorID),
				Scopes: desiredScopes, Version: client.Version,
			})
			if repairErr != nil {
				return nil, repairErr
			}
			client = repaired
		}
		if !ShouldRotateSubsystemUpdateSecret(request.Operation, requirement.Rotate) {
			credentials = append(credentials, SubsystemServiceCredential{Purpose: requirement.Purpose, OAuthClient: client})
			continue
		}
		var expectedVersion *uint64
		if RequiresStrictSubsystemUpdateServiceClientValidation(request.ApplicationCode, requirement.Purpose) {
			version := client.Version
			expectedVersion = &version
		}
		secret, secretErr := service.clients.CreateOAuthClientSecret(ctx, OAuthClientSecretCreateInput{
			TenantID: tenantID, OAuthClientID: client.ID, OperatorID: strings.TrimSpace(request.OperatorID),
			ExpectedClientVersion: expectedVersion,
		})
		if secretErr != nil {
			return nil, secretErr
		}
		credentials = append(credentials, SubsystemServiceCredential{
			Purpose: requirement.Purpose, OAuthClient: client, PlaintextSecret: secret.PlaintextSecret,
		})
	}
	return credentials, nil
}
