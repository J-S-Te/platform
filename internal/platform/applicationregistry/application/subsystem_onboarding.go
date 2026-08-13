package application

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"
)

const (
	defaultSubsystemAccessTokenTTLSeconds  = 15 * 60
	defaultSubsystemRefreshTokenTTLSeconds = 30 * 24 * 60 * 60
	integratedCustomerApplicationCode      = "customer_and_opportunity"
	integratedCustomerPathPrefix           = "/customer-opportunity"
	integratedCustomerUpstreamURL          = "http://customer-api:8090"
	legacyCustomerPathPrefix               = "/customer_and_opportunity"
	legacyCustomerUpstreamURL              = "http://opportunity-api:8082"
	integratedPortalApplicationCode        = "customer_portal"
	integratedPortalPathPrefix             = "/customer-portal"
	integratedPortalUpstreamURL            = "http://portal-api:8091"
	integratedContractApplicationCode      = "contract_management"
)

const (
	// 审计写入客户端是所有接入环境的基线能力，只能追加审计事件；它与浏览器登录、
	// 授权目录发布和子系统间业务调用使用不同客户端，避免任一密钥泄露后权限横向扩散。
	ServiceCredentialAuditIngest                    = "audit_ingest"
	ServiceCredentialExternalUserProvision          = "external_user_provision"
	ServiceCredentialApplicationRoleAssign          = "application_role_assign"
	ServiceCredentialApplicationRoleRevoke          = "application_role_revoke"
	ServiceCredentialPortalMappingProvision         = "portal_mapping_provision"
	ServiceCredentialPortalMappingDisable           = "portal_mapping_disable"
	ServiceCredentialPortalInviteVerify             = "portal_invite_verify"
	ServiceCredentialOwnerDirectoryRead             = "owner_directory_read"
	ServiceCredentialContractOpportunitySignedWrite = "contract_opportunity_signed_write"
	ServiceCredentialContractSummaryRead            = "contract_summary_read"
)

// 上述用途常量同时是“凭据最小权限”的协议标识：部署 Agent 按用途把不同密钥写入
// 对应子系统，不能把其中任一机器客户端当成通用平台客户端复用。

// 接入是仅创建操作；同租户的应用环境已经存在时返回可识别冲突，调用方可以提示改走
// 更新或重试流程，同时不把内部 ID、数据库信息或凭据材料带入错误响应。
var ErrSubsystemOnboardingAlreadyExists = errors.New("subsystem onboarding environment already exists")

// 冲突对象只记录操作者已经知道的应用编码、环境编码及非敏感状态，供接口生成可操作提示；
// OAuth 客户端 ID 和密钥不会进入该对象，避免错误日志或 JSON 响应意外泄露凭据。
type SubsystemOnboardingConflict struct {
	ApplicationCode string
	Environment     string
	Status          string
}

func (conflict *SubsystemOnboardingConflict) Error() string {
	return fmt.Sprintf("subsystem onboarding environment already exists: application=%s environment=%s status=%s", conflict.ApplicationCode, conflict.Environment, conflict.Status)
}

// 让传输层可用 errors.Is 识别接入冲突，而不依赖具体错误文本。
func (*SubsystemOnboardingConflict) Is(target error) bool {
	return target == ErrSubsystemOnboardingAlreadyExists
}

// 输入只接收管理员需要决定的公开信息；资源 ID、门户目标、OAuth 回调及凭据由服务端
// 统一推导，避免分别调用多个管理接口时形成环境错配或遗漏安全参数。
type SubsystemOnboardingInput struct {
	TenantID   string
	OperatorID string
	// InitialAdminUserID is persisted with the deployment state so a failed first deployment
	// can grant the originally selected administrator after a safe retry.
	InitialAdminUserID string
	ApplicationCode    string
	ApplicationName    string
	Description        *string
	Environment        string
	PublicBaseURL      string
	UpstreamURL        string
	PathPrefix         string
	ClientType         string
	// IssuerAlias is the selected authentication provider.  It is persisted on
	// the environment so a later deploy/retry has an explicit, auditable choice
	// instead of guessing from the current process configuration.
	IssuerAlias *string
	// AllowedServiceBindings 是该应用清单声明的可创建服务凭据用途（不含 audit_ingest 基线）。
	// 为空且清单未声明时回退到平台硬编码默认，保证既有行为不变。
	AllowedServiceBindings []string
}

// 写模型汇总一次接入产生的控制面对象，仓储在一个数据库事务内持久化；明文密钥不进入
// 写模型，持久化边界只接收经过保护的 SecretWrite。
type SubsystemOnboardingWrite struct {
	InitialAdminUserID string
	Application        ApplicationCreateInput
	ApplicationID      string
	Environment        EnvironmentCreateInput
	EnvironmentID      string
	LoginTarget        LoginTargetCreateInput
	LoginTargetID      string
	OAuthClient        OAuthClientCreateInput
	OAuthClientID      string
	OAuthClientSecret  *SecretWrite

	// 授权目录发布使用独立机器客户端，不能与浏览器 OIDC 回调或其他系统集成复用。
	CatalogPublisherOAuthClient       OAuthClientCreateInput
	CatalogPublisherOAuthClientID     string
	CatalogPublisherOAuthClientSecret *SecretWrite

	// 每个环境都会获得单 scope 的审计写入客户端；已知集成应用再按用途增加独立客户端：
	// CRM 增加负责人目录读取，客户门户增加六个身份及门户协作能力。每种用途使用独立密钥。
	ServiceClients []SubsystemServiceClientWrite
}

// SubsystemDirectoryRegistrationInput is the non-authentication half of an
// application integration.  It deliberately contains no OAuth client type,
// callback URI or credential fields: those are owned by the selected identity
// provider (for example Keycloak) and are created through its separate
// integration workflow.
//
// The legacy SubsystemOnboardingInput remains available for already deployed
// installations.  New callers should register the directory first, then use
// the Keycloak integration APIs to create/synchronise the provider client and
// finally apply the runtime deployment configuration.
type SubsystemDirectoryRegistrationInput struct {
	TenantID        string
	OperatorID      string
	ApplicationCode string
	ApplicationName string
	Description     *string
	Environment     string
	PublicBaseURL   string
	UpstreamURL     string
	PathPrefix      string
	IssuerAlias     *string
}

// SubsystemDirectoryRegistrationWrite is the atomic persistence boundary for
// application directory information.  OAuth clients, service credentials and
// deployment state are intentionally absent.
type SubsystemDirectoryRegistrationWrite struct {
	Application   ApplicationCreateInput
	ApplicationID string
	Environment   EnvironmentCreateInput
	EnvironmentID string
	LoginTarget   LoginTargetCreateInput
	LoginTargetID string
}

// SubsystemDirectoryRegistrationResult is safe to expose to administrators:
// it is only the application catalogue, environment and landing target.
type SubsystemDirectoryRegistrationResult struct {
	Application Application
	Environment Environment
	LoginTarget LoginTargetManagementItem
	PublicURL   string
}

type SubsystemServiceClientWrite struct {
	Purpose           string
	OAuthClient       OAuthClientCreateInput
	OAuthClientID     string
	OAuthClientSecret *SecretWrite
}

type SubsystemServiceCredential struct {
	Purpose         string
	OAuthClient     OAuthClientView
	PlaintextSecret string
}

// 结果中的明文密钥只在首次接入成功后交付一次，后续列表接口无法恢复；调用方不得记录
// 或再次持久化这些值，只能直接交给可信部署器完成运行时配置。
type SubsystemOnboardingResult struct {
	Application Application
	Environment Environment
	LoginTarget LoginTargetManagementItem
	OAuthClient OAuthClientView
	// 目录发布客户端对象用于审计追踪，明文则单独承载并只转交可信部署器。
	CatalogPublisherOAuthClient     OAuthClientView
	PlaintextSecret                 string
	CatalogPublisherPlaintextSecret string
	ServiceCredentials              []SubsystemServiceCredential
	RedirectURI                     string
	PublicURL                       string
}

// 门户投影只包含已认证用户可见且可解析的跳转信息，不携带控制面配置或任何凭据。
type PortalApplication struct {
	ApplicationID string
	Code          string
	Name          string
	Description   *string
	EnvironmentID string
	Environment   string
	PathPrefix    *string
	TargetCode    string
	TargetURI     string
	PublicURL     string
}

// 仓储负责在单个数据库事务内写入接入聚合，并按租户读取有效门户登记。
type SubsystemOnboardingRepository interface {
	CreateSubsystem(context.Context, SubsystemOnboardingWrite, time.Time) (SubsystemOnboardingResult, error)
	CreateSubsystemDirectory(context.Context, SubsystemDirectoryRegistrationWrite, time.Time) (SubsystemDirectoryRegistrationResult, error)
	ListPortalApplications(context.Context, string, string, string) ([]PortalApplication, error)
	ResolveApplicationEnvironment(context.Context, string, string, string) (string, string, error)
}

// 服务把应用、环境、门户入口、浏览器客户端及用途隔离的机器客户端编排成一次接入。
type SubsystemOnboardingService struct {
	repository                  SubsystemOnboardingRepository
	ids                         ManagementIdentifierGenerator
	clock                       Clock
	redirectURIValidationPolicy RedirectURIValidationPolicy
}

// 构造时要求 ID、时钟和仓储齐备；回调 URI 策略允许由环境控制是否接受受限 HTTP。
func NewSubsystemOnboardingService(repository SubsystemOnboardingRepository, ids ManagementIdentifierGenerator, clock Clock, redirectURIValidationPolicy RedirectURIValidationPolicy) (*SubsystemOnboardingService, error) {
	if repository == nil || ids == nil || clock == nil {
		return nil, errors.New("subsystem onboarding dependencies must not be nil")
	}
	return &SubsystemOnboardingService{
		repository: repository, ids: ids, clock: clock, redirectURIValidationPolicy: redirectURIValidationPolicy,
	}, nil
}

// 预检只规范化并验证公开接入参数，不生成 ID、密钥或数据库记录，便于部署器在真正写入
// 控制面事务前先检查运行环境。
func ValidateSubsystemOnboardingInput(input SubsystemOnboardingInput) error {
	input = normalizeSubsystemOnboardingInput(input)
	if !validSubsystemOnboardingInput(input) {
		return ErrValidation
	}
	return nil
}

// 接入先在内存中生成完整聚合和一次性密钥，再由仓储事务统一落库；数据库成功并不等于
// 部署文件已经写入，后续部署失败必须由上层补偿或重试，不能把两者描述成跨系统原子操作。
func (service *SubsystemOnboardingService) OnboardSubsystem(ctx context.Context, input SubsystemOnboardingInput) (SubsystemOnboardingResult, error) {
	input = normalizeSubsystemOnboardingInput(input)
	if !validSubsystemOnboardingInput(input) {
		return SubsystemOnboardingResult{}, ErrValidation
	}

	publicURL := input.PublicBaseURL + input.PathPrefix + "/"
	redirectURI := input.PublicBaseURL + input.PathPrefix + "/auth/callback"
	clientID := input.ApplicationCode + "-" + input.Environment + "-web"
	// 浏览器公开客户端不能安全保存 secret，因此只能使用 PKCE；机密客户端则由后续的
	// 一次性交付链路把 secret 交给部署 Agent，明文不会进入仓储写模型。
	tokenAuthMethod := "client_secret_basic"
	if input.ClientType == "public" {
		tokenAuthMethod = "none"
	}

	applicationInput := normalizeApplicationCreate(ApplicationCreateInput{
		TenantID: input.TenantID, OperatorID: input.OperatorID, Code: input.ApplicationCode,
		Name: input.ApplicationName, ApplicationType: "web", HomepageURL: stringPointer(publicURL),
		Description: input.Description, Status: "ACTIVE",
	})
	if !validApplicationCreate(applicationInput) {
		return SubsystemOnboardingResult{}, ErrValidation
	}

	now := service.clock.Now().UTC()
	applicationID, err := service.newID(now, "application")
	if err != nil {
		return SubsystemOnboardingResult{}, err
	}
	environmentID, err := service.newID(now, "application environment")
	if err != nil {
		return SubsystemOnboardingResult{}, err
	}
	loginTargetID, err := service.newID(now, "application login target")
	if err != nil {
		return SubsystemOnboardingResult{}, err
	}
	oauthClientID, err := service.newID(now, "OAuth client")
	if err != nil {
		return SubsystemOnboardingResult{}, err
	}
	catalogPublisherOAuthClientID, err := service.newID(now, "authorization catalog publisher OAuth client")
	if err != nil {
		return SubsystemOnboardingResult{}, err
	}

	environmentInput := normalizeEnvironmentCreate(EnvironmentCreateInput{
		TenantID: input.TenantID, OperatorID: input.OperatorID, ApplicationID: applicationID,
		Environment: input.Environment, BaseURL: stringPointer(input.PublicBaseURL),
		UpstreamURL: stringPointer(input.UpstreamURL), PathPrefix: stringPointer(input.PathPrefix), IssuerAlias: input.IssuerAlias, Status: "ACTIVE",
	})
	if !validEnvironmentCreate(environmentInput) {
		return SubsystemOnboardingResult{}, ErrValidation
	}
	loginTargetInput := normalizeLoginTargetCreate(LoginTargetCreateInput{
		TenantID: input.TenantID, OperatorID: input.OperatorID, ApplicationID: applicationID,
		EnvironmentID: environmentID, TargetCode: "home", Name: input.ApplicationName + "首页",
		TargetURI: input.PathPrefix + "/", Status: "ACTIVE",
	})
	if !validLoginTargetCreate(loginTargetInput) {
		return SubsystemOnboardingResult{}, ErrValidation
	}
	oauthClientInput, err := normalizeOAuthClientCreate(OAuthClientCreateInput{
		TenantID: input.TenantID, OperatorID: input.OperatorID, ApplicationID: applicationID,
		EnvironmentID: environmentID, ClientID: clientID, ClientName: input.ApplicationName + " Web",
		ClientType: input.ClientType, TokenAuthMethod: tokenAuthMethod,
		AccessTokenTTLSeconds:  defaultSubsystemAccessTokenTTLSeconds,
		RefreshTokenTTLSeconds: defaultSubsystemRefreshTokenTTLSeconds,
		RequirePKCE:            true, GrantTypes: []string{"authorization_code", "refresh_token"},
		Scopes: []string{"openid", "profile"}, RedirectURIs: []string{redirectURI},
	}, service.redirectURIValidationPolicy)
	if err != nil {
		return SubsystemOnboardingResult{}, err
	}

	var secret *SecretWrite
	plaintextSecret := ""
	if tokenAuthMethod == "client_secret_basic" {
		write, plaintext, secretErr := newOAuthClientSecretWrite(service.ids, now, nil)
		if secretErr != nil {
			return SubsystemOnboardingResult{}, secretErr
		}
		secret, plaintextSecret = &write, plaintext
	}

	// 每个子系统都使用独立的目录发布客户端，避免 Web 登录客户端一旦泄露后同时获得
	// 修改授权目录的控制面能力。
	// 固定为 client_credentials/client_secret_basic 且只有一个 scope，防止目录发布凭据
	// 被拿去执行浏览器授权或其他机器接口。
	catalogPublisherClientInput, err := normalizeOAuthClientCreate(OAuthClientCreateInput{
		TenantID: input.TenantID, OperatorID: input.OperatorID, ApplicationID: applicationID,
		EnvironmentID: environmentID, ClientID: input.ApplicationCode + "-" + input.Environment + "-catalog-publisher",
		ClientName: input.ApplicationName + " Authorization Catalog Publisher",
		ClientType: "service", TokenAuthMethod: "client_secret_basic",
		AccessTokenTTLSeconds:  defaultSubsystemAccessTokenTTLSeconds,
		RefreshTokenTTLSeconds: 0, RequirePKCE: false,
		GrantTypes: []string{"client_credentials"},
		Scopes:     []string{"authorization.catalog.sync"},
	}, service.redirectURIValidationPolicy)
	if err != nil {
		return SubsystemOnboardingResult{}, err
	}
	catalogPublisherSecretWrite, catalogPublisherPlaintextSecret, err := newOAuthClientSecretWrite(service.ids, now, nil)
	if err != nil {
		return SubsystemOnboardingResult{}, err
	}
	serviceClients, serviceCredentials, err := service.buildIntegratedServiceClients(input, applicationID, environmentID, now)
	if err != nil {
		return SubsystemOnboardingResult{}, err
	}

	result, err := service.repository.CreateSubsystem(ctx, SubsystemOnboardingWrite{
		InitialAdminUserID: input.InitialAdminUserID,
		Application:        applicationInput, ApplicationID: applicationID,
		Environment: environmentInput, EnvironmentID: environmentID,
		LoginTarget: loginTargetInput, LoginTargetID: loginTargetID,
		OAuthClient: oauthClientInput, OAuthClientID: oauthClientID, OAuthClientSecret: secret,
		CatalogPublisherOAuthClient:       catalogPublisherClientInput,
		CatalogPublisherOAuthClientID:     catalogPublisherOAuthClientID,
		CatalogPublisherOAuthClientSecret: &catalogPublisherSecretWrite,
		ServiceClients:                    serviceClients,
	}, now)
	if err != nil {
		return SubsystemOnboardingResult{}, err
	}
	// 只有数据库事务成功后才把对应明文装配回结果。仓储始终只看到哈希；调用方必须把
	// 这些明文直接交给可信部署器，任何重试都不能依赖从数据库“找回”旧密钥。
	result.PlaintextSecret = plaintextSecret
	result.CatalogPublisherPlaintextSecret = catalogPublisherPlaintextSecret
	for index := range result.ServiceCredentials {
		if secret, ok := serviceCredentials[result.ServiceCredentials[index].Purpose]; ok {
			result.ServiceCredentials[index].PlaintextSecret = secret
		}
	}
	result.RedirectURI = redirectURI
	result.PublicURL = publicURL
	return result, nil
}

// RegisterSubsystemDirectory registers only the business application, its
// environment and an approved post-login landing target.  In particular, it
// never creates a platform browser OAuth client.  This keeps application
// directory/authorization administration independent from Keycloak Client
// lifecycle while preserving the legacy all-in-one onboarding path above for
// existing deployments.
func (service *SubsystemOnboardingService) RegisterSubsystemDirectory(ctx context.Context, input SubsystemDirectoryRegistrationInput) (SubsystemDirectoryRegistrationResult, error) {
	input = normalizeSubsystemDirectoryRegistrationInput(input)
	if !validSubsystemDirectoryRegistrationInput(input) {
		return SubsystemDirectoryRegistrationResult{}, ErrValidation
	}

	publicURL := input.PublicBaseURL + input.PathPrefix + "/"
	now := service.clock.Now().UTC()
	applicationID, err := service.newID(now, "application")
	if err != nil {
		return SubsystemDirectoryRegistrationResult{}, err
	}
	environmentID, err := service.newID(now, "application environment")
	if err != nil {
		return SubsystemDirectoryRegistrationResult{}, err
	}
	loginTargetID, err := service.newID(now, "application login target")
	if err != nil {
		return SubsystemDirectoryRegistrationResult{}, err
	}

	applicationInput := normalizeApplicationCreate(ApplicationCreateInput{
		TenantID: input.TenantID, OperatorID: input.OperatorID, Code: input.ApplicationCode,
		Name: input.ApplicationName, ApplicationType: "web", HomepageURL: stringPointer(publicURL),
		Description: input.Description, Status: "ACTIVE",
	})
	environmentInput := normalizeEnvironmentCreate(EnvironmentCreateInput{
		TenantID: input.TenantID, OperatorID: input.OperatorID, ApplicationID: applicationID,
		Environment: input.Environment, BaseURL: stringPointer(input.PublicBaseURL),
		UpstreamURL: stringPointer(input.UpstreamURL), PathPrefix: stringPointer(input.PathPrefix),
		IssuerAlias: input.IssuerAlias, Status: "ACTIVE",
	})
	loginTargetInput := normalizeLoginTargetCreate(LoginTargetCreateInput{
		TenantID: input.TenantID, OperatorID: input.OperatorID, ApplicationID: applicationID,
		EnvironmentID: environmentID, TargetCode: "home", Name: input.ApplicationName + "首页",
		TargetURI: input.PathPrefix + "/", Status: "ACTIVE",
	})
	if !validApplicationCreate(applicationInput) || !validEnvironmentCreate(environmentInput) || !validLoginTargetCreate(loginTargetInput) {
		return SubsystemDirectoryRegistrationResult{}, ErrValidation
	}

	result, err := service.repository.CreateSubsystemDirectory(ctx, SubsystemDirectoryRegistrationWrite{
		Application: applicationInput, ApplicationID: applicationID,
		Environment: environmentInput, EnvironmentID: environmentID,
		LoginTarget: loginTargetInput, LoginTargetID: loginTargetID,
	}, now)
	if err != nil {
		return SubsystemDirectoryRegistrationResult{}, err
	}
	result.PublicURL = publicURL
	return result, nil
}

// integratedServicePurposeDefinition 描述一个服务用途客户端的固定元数据（scope/suffix/name）。
// 这是平台级的通用注册表，不属于任何具体子系统；清单声明 allowed_service_bindings 后，
// 接入服务按注册表创建对应客户端。
type integratedServicePurposeDefinition struct {
	purpose string
	suffix  string
	name    string
	scope   string
}

var integratedServicePurposeRegistry = map[string]integratedServicePurposeDefinition{
	ServiceCredentialAuditIngest:                    {ServiceCredentialAuditIngest, "audit-publisher", "Audit Publisher", "audit.ingest"},
	ServiceCredentialOwnerDirectoryRead:             {ServiceCredentialOwnerDirectoryRead, "owner-directory", "Owner Directory Reader", "owner_directory.read"},
	ServiceCredentialExternalUserProvision:          {ServiceCredentialExternalUserProvision, "external-user-provision", "External User Provisioner", "external_user.provision"},
	ServiceCredentialApplicationRoleAssign:          {ServiceCredentialApplicationRoleAssign, "role-assign", "Application Role Assigner", "application_role.assign"},
	ServiceCredentialApplicationRoleRevoke:          {ServiceCredentialApplicationRoleRevoke, "role-revoke", "Application Role Revoker", "application_role.revoke"},
	ServiceCredentialPortalMappingProvision:         {ServiceCredentialPortalMappingProvision, "portal-mapping-provision", "Portal Identity Mapping Provisioner", "portal.identity_mapping.provision"},
	ServiceCredentialPortalMappingDisable:           {ServiceCredentialPortalMappingDisable, "portal-mapping-disable", "Portal Identity Mapping Disabler", "portal.identity_mapping.disable"},
	ServiceCredentialPortalInviteVerify:             {ServiceCredentialPortalInviteVerify, "portal-invite-verify", "Portal Invite Verifier", "portal.invite.verify"},
	ServiceCredentialContractOpportunitySignedWrite: {ServiceCredentialContractOpportunitySignedWrite, "opportunity-intake", "Opportunity Signed Intake", "opportunity.signed.write"},
	ServiceCredentialContractSummaryRead:            {ServiceCredentialContractSummaryRead, "contract-summary", "Contract Summary Reader", "contract.summary.read"},
}

// hardcodedIntegratedServicePurposes 是平台内置默认的集成服务用途（不含 audit_ingest 基线）。
// 清单未声明 allowed_service_bindings 时使用，保证既有行为不变。
func hardcodedIntegratedServicePurposes(applicationCode string) []string {
	switch strings.TrimSpace(applicationCode) {
	case integratedCustomerApplicationCode:
		return []string{ServiceCredentialOwnerDirectoryRead}
	case integratedPortalApplicationCode:
		return []string{
			ServiceCredentialExternalUserProvision,
			ServiceCredentialApplicationRoleAssign,
			ServiceCredentialApplicationRoleRevoke,
			ServiceCredentialPortalMappingProvision,
			ServiceCredentialPortalMappingDisable,
			ServiceCredentialPortalInviteVerify,
		}
	case integratedContractApplicationCode:
		return []string{ServiceCredentialContractOpportunitySignedWrite, ServiceCredentialContractSummaryRead, ServiceCredentialOwnerDirectoryRead}
	}
	return nil
}

func (service *SubsystemOnboardingService) buildIntegratedServiceClients(input SubsystemOnboardingInput, applicationID, environmentID string, now time.Time) ([]SubsystemServiceClientWrite, map[string]string, error) {
	// 集中审计写入是所有接入环境的基线，而不是特定应用的可选集成；每个环境都使用
	// 自己的单 scope 客户端，平台可据此绑定来源应用和环境。
	purposes := []string{ServiceCredentialAuditIngest}
	if input.AllowedServiceBindings != nil {
		// 清单驱动（B3）：按清单声明的用途创建；审计基线恒在。
		for _, purpose := range input.AllowedServiceBindings {
			purposes = append(purposes, strings.TrimSpace(purpose))
		}
	} else {
		// 缺省回退：平台硬编码默认，保证既有行为不变。
		purposes = append(purposes, hardcodedIntegratedServicePurposes(input.ApplicationCode)...)
	}
	definitions := make([]integratedServicePurposeDefinition, 0, len(purposes))
	seen := make(map[string]struct{}, len(purposes))
	for _, purpose := range purposes {
		if purpose == "" {
			continue
		}
		if _, duplicate := seen[purpose]; duplicate {
			continue
		}
		definition, known := integratedServicePurposeRegistry[purpose]
		if !known {
			return nil, nil, fmt.Errorf("subsystem service purpose %q is not registered", purpose)
		}
		seen[purpose] = struct{}{}
		definitions = append(definitions, definition)
	}
	writes := make([]SubsystemServiceClientWrite, 0, len(definitions))
	secrets := make(map[string]string, len(definitions))
	for _, definition := range definitions {
		client, err := normalizeOAuthClientCreate(OAuthClientCreateInput{
			TenantID: input.TenantID, OperatorID: input.OperatorID, ApplicationID: applicationID,
			EnvironmentID: environmentID,
			ClientID:      input.ApplicationCode + "-" + input.Environment + "-" + definition.suffix,
			ClientName:    input.ApplicationName + " " + definition.name,
			ClientType:    "service", TokenAuthMethod: "client_secret_basic",
			AccessTokenTTLSeconds: defaultSubsystemAccessTokenTTLSeconds, RequirePKCE: false,
			GrantTypes: []string{"client_credentials"}, Scopes: []string{definition.scope},
		}, service.redirectURIValidationPolicy)
		if err != nil {
			return nil, nil, err
		}
		clientID, err := service.newID(now, definition.purpose+" OAuth client")
		if err != nil {
			return nil, nil, err
		}
		secretWrite, plaintext, err := newOAuthClientSecretWrite(service.ids, now, nil)
		if err != nil {
			return nil, nil, err
		}
		writes = append(writes, SubsystemServiceClientWrite{
			Purpose: definition.purpose, OAuthClient: client, OAuthClientID: clientID,
			OAuthClientSecret: &secretWrite,
		})
		secrets[definition.purpose] = plaintext
	}
	return writes, secrets, nil
}

// ResolveApplicationEnvironment reads the registered pair without portal visibility or
// deployment-readiness filters. Control-plane synchronization must repair failed environments.
func (service *SubsystemOnboardingService) ResolveApplicationEnvironment(ctx context.Context, tenantID, applicationCode, environment string) (string, string, error) {
	tenantID, applicationCode, environment = strings.TrimSpace(tenantID), strings.TrimSpace(applicationCode), strings.ToLower(strings.TrimSpace(environment))
	if tenantID == "" || applicationCode == "" || !validEnvironmentCode(environment) {
		return "", "", ErrValidation
	}
	return service.repository.ResolveApplicationEnvironment(ctx, tenantID, applicationCode, environment)
}

// 门户列表只返回当前租户、当前用户可见且能安全解析的有效应用；未指定环境时由仓储按
// 固定优先级选择，避免同一登记在不同请求中随机切换环境。
func (service *SubsystemOnboardingService) ListPortalApplications(ctx context.Context, tenantID, userID, environment string) ([]PortalApplication, error) {
	tenantID = strings.TrimSpace(tenantID)
	userID = strings.TrimSpace(userID)
	environment = strings.ToLower(strings.TrimSpace(environment))
	if tenantID == "" || userID == "" || (environment != "" && !validEnvironmentCode(environment)) {
		return nil, ErrValidation
	}
	items, err := service.repository.ListPortalApplications(ctx, tenantID, userID, environment)
	if err != nil {
		return nil, err
	}
	result := make([]PortalApplication, 0, len(items))
	for _, item := range items {
		// 仓储返回的是登记数据，但门户最终跳转前仍重新解析并丢弃异常目标；单条脏数据
		// 不应导致整个门户不可用，更不能把未验证字符串直接返回给浏览器。
		publicURL, resolveErr := resolvePortalTarget(item.PublicURL, item.TargetURI)
		if resolveErr != nil {
			continue
		}
		item.PublicURL = publicURL
		result = append(result, item)
	}
	return result, nil
}

func (service *SubsystemOnboardingService) newID(now time.Time, resource string) (string, error) {
	identifier, err := service.ids.New(now)
	if err != nil {
		return "", fmt.Errorf("generate %s ID: %w", resource, err)
	}
	return identifier, nil
}

func normalizeSubsystemOnboardingInput(input SubsystemOnboardingInput) SubsystemOnboardingInput {
	input.TenantID = strings.TrimSpace(input.TenantID)
	input.OperatorID = strings.TrimSpace(input.OperatorID)
	input.InitialAdminUserID = strings.TrimSpace(input.InitialAdminUserID)
	if input.InitialAdminUserID == "" {
		input.InitialAdminUserID = input.OperatorID
	}
	input.ApplicationCode = strings.ToLower(strings.TrimSpace(input.ApplicationCode))
	input.ApplicationName = strings.TrimSpace(input.ApplicationName)
	input.Description = normalizeOptional(input.Description)
	input.Environment = strings.ToLower(strings.TrimSpace(input.Environment))
	if input.Environment == "" {
		input.Environment = "prod"
	}
	input.PublicBaseURL = strings.TrimRight(strings.TrimSpace(input.PublicBaseURL), "/")
	input.UpstreamURL = strings.TrimRight(strings.TrimSpace(input.UpstreamURL), "/")
	input.PathPrefix = strings.TrimRight(strings.TrimSpace(input.PathPrefix), "/")
	if input.PathPrefix == "" && input.ApplicationCode != "" {
		input.PathPrefix = "/" + input.ApplicationCode
	}
	// 本地工作区把 customer_and_opportunity 纳入统一 frontend/Compose 拓扑；历史部署可能只迁移了
	// upstream 或 path，因此分别兼容已知旧值，避免一次接入因旧配置组合不同而失败。
	if input.ApplicationCode == integratedCustomerApplicationCode {
		if input.PathPrefix == legacyCustomerPathPrefix {
			input.PathPrefix = integratedCustomerPathPrefix
		}
		if input.UpstreamURL == legacyCustomerUpstreamURL {
			input.UpstreamURL = integratedCustomerUpstreamURL
		}
	}
	if input.ApplicationCode == integratedPortalApplicationCode {
		if input.PathPrefix == "/customer_portal" {
			input.PathPrefix = integratedPortalPathPrefix
		}
		if input.UpstreamURL == "http://customer-portal-api:8091" {
			input.UpstreamURL = integratedPortalUpstreamURL
		}
	}
	input.ClientType = strings.ToLower(strings.TrimSpace(input.ClientType))
	if input.ClientType == "" {
		input.ClientType = "confidential"
	}
	return input
}

func normalizeSubsystemDirectoryRegistrationInput(input SubsystemDirectoryRegistrationInput) SubsystemDirectoryRegistrationInput {
	input.TenantID = strings.TrimSpace(input.TenantID)
	input.OperatorID = strings.TrimSpace(input.OperatorID)
	input.ApplicationCode = strings.ToLower(strings.TrimSpace(input.ApplicationCode))
	input.ApplicationName = strings.TrimSpace(input.ApplicationName)
	input.Description = normalizeOptional(input.Description)
	input.Environment = strings.ToLower(strings.TrimSpace(input.Environment))
	if input.Environment == "" {
		input.Environment = "prod"
	}
	input.PublicBaseURL = strings.TrimRight(strings.TrimSpace(input.PublicBaseURL), "/")
	input.UpstreamURL = strings.TrimRight(strings.TrimSpace(input.UpstreamURL), "/")
	input.PathPrefix = strings.TrimRight(strings.TrimSpace(input.PathPrefix), "/")
	if input.PathPrefix == "" && input.ApplicationCode != "" {
		input.PathPrefix = "/" + input.ApplicationCode
	}
	if input.ApplicationCode == integratedCustomerApplicationCode {
		if input.PathPrefix == legacyCustomerPathPrefix {
			input.PathPrefix = integratedCustomerPathPrefix
		}
		if input.UpstreamURL == legacyCustomerUpstreamURL {
			input.UpstreamURL = integratedCustomerUpstreamURL
		}
	}
	if input.ApplicationCode == integratedPortalApplicationCode {
		if input.PathPrefix == "/customer_portal" {
			input.PathPrefix = integratedPortalPathPrefix
		}
		if input.UpstreamURL == "http://customer-portal-api:8091" {
			input.UpstreamURL = integratedPortalUpstreamURL
		}
	}
	if input.IssuerAlias != nil {
		alias := strings.ToLower(strings.TrimSpace(*input.IssuerAlias))
		if alias == "" || alias == IssuerAliasPlatform || alias == "basic_platform" {
			input.IssuerAlias = nil
		} else {
			input.IssuerAlias = &alias
		}
	}
	return input
}

func validSubsystemOnboardingInput(input SubsystemOnboardingInput) bool {
	baseURL, upstreamURL, pathPrefix := input.PublicBaseURL, input.UpstreamURL, input.PathPrefix
	return len(input.TenantID) == 26 && validIdentifier(input.TenantID) &&
		len(input.OperatorID) == 26 && validIdentifier(input.OperatorID) &&
		len(input.InitialAdminUserID) == 26 && validIdentifier(input.InitialAdminUserID) && validCode(input.ApplicationCode, 64) &&
		validManagementText(input.ApplicationName, 128, false) && validEnvironmentCode(input.Environment) &&
		validOptionalBaseURL(&baseURL) && validOptionalUpstreamURL(&upstreamURL) &&
		validOptionalPathPrefix(&pathPrefix) && validGatewayTripleConsistent(&baseURL, &upstreamURL, &pathPrefix) &&
		oneOf(input.ClientType, "public", "confidential")
}

func validSubsystemDirectoryRegistrationInput(input SubsystemDirectoryRegistrationInput) bool {
	baseURL, upstreamURL, pathPrefix := input.PublicBaseURL, input.UpstreamURL, input.PathPrefix
	return len(input.TenantID) == 26 && validIdentifier(input.TenantID) &&
		len(input.OperatorID) == 26 && validIdentifier(input.OperatorID) && validCode(input.ApplicationCode, 64) &&
		validManagementText(input.ApplicationName, 128, false) && validEnvironmentCode(input.Environment) &&
		validOptionalBaseURL(&baseURL) && validOptionalUpstreamURL(&upstreamURL) &&
		validOptionalPathPrefix(&pathPrefix) && validGatewayTripleConsistent(&baseURL, &upstreamURL, &pathPrefix) &&
		(input.IssuerAlias == nil || *input.IssuerAlias == IssuerAliasKeycloak)
}

func resolvePortalTarget(baseURL, targetURI string) (string, error) {
	targetURI = strings.TrimSpace(targetURI)
	parsed, err := url.Parse(targetURI)
	if err != nil {
		return "", err
	}
	if parsed.IsAbs() {
		if !validRedirectURI(targetURI, RedirectURIValidationPolicy{}) {
			return "", ErrValidation
		}
		return targetURI, nil
	}
	baseURL = strings.TrimRight(strings.TrimSpace(baseURL), "/")
	if baseURL == "" || !strings.HasPrefix(targetURI, "/") {
		return "", ErrValidation
	}
	return baseURL + targetURI, nil
}

func stringPointer(value string) *string { return &value }
