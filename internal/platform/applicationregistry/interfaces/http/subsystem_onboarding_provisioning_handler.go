// subsystem_onboarding_provisioning_handler.go：子系统接入与运行时供给（onboarding-provisioning）——接入、能力查询、更新、接管、拆除及服务凭据确保。
package http

import (
	"context"
	"errors"
	stdhttp "net/http"
	"strings"
	"time"

	"github.com/J-S-Te/Basic-Platform/internal/platform/applicationregistry/application"
	"github.com/J-S-Te/Basic-Platform/internal/shared/httperror"
	"github.com/J-S-Te/Basic-Platform/internal/shared/httpresponse"
)

// OnboardSubsystem 处理 POST /api/v1/subsystem-onboarding。
func (handler *SubsystemOnboardingHandler) OnboardSubsystem(writer stdhttp.ResponseWriter, request *stdhttp.Request) {
	extendSubsystemDeploymentWriteDeadline(writer)
	principal, ok := subsystemPrincipal(writer, request)
	if !ok {
		return
	}
	var payload subsystemOnboardingRequest
	if !decodeApplicationManagementJSON(writer, request, &payload) {
		return
	}
	initialAdminUserID := strings.TrimSpace(payload.InitialAdminID)
	if initialAdminUserID == "" {
		initialAdminUserID = principal.User.ID
	}
	effectiveIssuerAlias := handler.effectiveIssuerAlias(payload.IssuerAlias)
	onboardingInput := application.SubsystemOnboardingInput{
		TenantID: principal.Tenant.ID, OperatorID: principal.User.ID, InitialAdminUserID: initialAdminUserID,
		ApplicationCode: payload.ApplicationCode, ApplicationName: payload.ApplicationName,
		Description: payload.Description, Environment: payload.Environment,
		PublicBaseURL: payload.PublicBaseURL, UpstreamURL: payload.UpstreamURL,
		PathPrefix: payload.PathPrefix, ClientType: payload.ClientType,
		IssuerAlias:            optionalIssuerAlias(effectiveIssuerAlias),
		AllowedServiceBindings: handler.allowedServiceBindingsForTarget(payload.ApplicationCode, payload.Environment),
	}
	if err := application.ValidateSubsystemOnboardingInput(onboardingInput); err != nil {
		handler.writeError(writer, request, err)
		return
	}
	if err := handler.service.PreflightValidate(request.Context(), onboardingInput); err != nil {
		handler.writeError(writer, request, err, "preflight")
		return
	}
	issuer, err := handler.issuerForAlias(effectiveIssuerAlias)
	if err != nil {
		handler.writeError(writer, request, err)
		return
	}
	if err := handler.provisioner.Preflight(request.Context(), application.SubsystemPreflightInput{
		TenantID: principal.Tenant.ID, ApplicationCode: onboardingInput.ApplicationCode,
		Environment: onboardingInput.Environment, Issuer: issuer,
		ManifestChecksum: handler.manifestChecksumForTarget(onboardingInput.ApplicationCode, onboardingInput.Environment),
		PublicBaseURL:    onboardingInput.PublicBaseURL, UpstreamURL: onboardingInput.UpstreamURL,
		PathPrefix: onboardingInput.PathPrefix, ClientType: onboardingInput.ClientType,
	}); err != nil {
		handler.writeError(writer, request, err, "preflight")
		return
	}
	relatedClients, relatedErr := handler.relatedServiceClients(request.Context(), principal.Tenant.ID, onboardingInput.ApplicationCode, onboardingInput.Environment)
	if relatedErr != nil {
		handler.writeError(writer, request, relatedErr, "contract integration dependency")
		return
	}
	result, err := handler.service.OnboardSubsystem(request.Context(), onboardingInput)
	if err != nil {
		handler.writeError(writer, request, err)
		return
	}
	pathPrefix, upstreamURL := "", ""
	// Onboard 原子地创建 generation 1。即使在后续状态读取完成前请求被取消，
	// 也要保留该所有权标记，避免并发接管。
	generation := uint64(1)
	accepted := false
	defer func() {
		if !accepted {
			handler.markDeploymentFailureDetached(subsystemDeploymentJob{TenantID: principal.Tenant.ID, ApplicationCode: result.Application.Code, Environment: result.Environment.Environment, Operation: "ONBOARD", Generation: generation}, "DEPLOYMENT_PREPARATION_FAILED", "部署准备失败，请点击重试")
		}
	}()
	if result.Environment.PathPrefix != nil {
		pathPrefix = *result.Environment.PathPrefix
	}
	if result.Environment.UpstreamURL != nil {
		upstreamURL = *result.Environment.UpstreamURL
	}
	provisioningClientID, provisioningClientSecret := result.OAuthClient.ClientID, result.PlaintextSecret
	manifestChecksum := handler.manifestChecksumForTarget(result.Application.Code, result.Environment.Environment)
	if err := handler.setDesiredManifest(request.Context(), principal.Tenant.ID, result.Application.Code, result.Environment.Environment, manifestChecksum); err != nil {
		handler.writeError(writer, request, err)
		return
	}
	keycloakClientID := ""
	if strings.EqualFold(effectiveIssuerAlias, "keycloak") {
		if handler.keycloakControl == nil || handler.keycloakBroker == nil {
			handler.writeError(writer, request, application.ErrValidation)
			return
		}
		if err := handler.reconcileKeycloakBroker(request.Context(), principal.Tenant.ID, principal.User.ID); err != nil {
			handler.writeError(writer, request, application.ErrSubsystemProvisioningUnavailable)
			return
		}
		client, clientErr := handler.keycloakControl.EnsureClient(request.Context(), result.Application.Code+"-"+result.Environment.Environment+"-web", result.Application.Name+" "+result.Environment.Environment, result.RedirectURI)
		if clientErr != nil {
			handler.writeError(writer, request, application.ErrSubsystemProvisioningUnavailable)
			return
		}
		provisioningClientID, provisioningClientSecret = client.ClientID, client.ClientSecret
		keycloakClientID = client.ClientID
	}
	// 装配逻辑提取到应用层（BuildSubsystemOnboardingProvisioningInput）：统一入口与旧路径共用同一份装配，
	// 避免两套实现漂移；凭据仍只在内存中传递，不落库、不回显。
	provisioningInput, err := application.BuildSubsystemOnboardingProvisioningInput(application.SubsystemOnboardingProvisioningAssembly{
		TenantID: principal.Tenant.ID, ApplicationID: result.Application.ID, EnvironmentID: result.Environment.ID,
		ApplicationCode: result.Application.Code, Environment: result.Environment.Environment,
		Issuer: issuer, AuthenticationProvider: effectiveIssuerAlias, ManifestChecksum: manifestChecksum,
		ClientID: provisioningClientID, ClientSecret: provisioningClientSecret,
		CatalogPublisherClientID:     result.CatalogPublisherOAuthClient.ClientID,
		CatalogPublisherClientSecret: result.CatalogPublisherPlaintextSecret,
		ServiceCredentials:           result.ServiceCredentials,
		RelatedServiceClients:        relatedClients,
		RedirectURI:                  result.RedirectURI, PublicURL: result.PublicURL,
		PathPrefix: pathPrefix, UpstreamURL: upstreamURL,
	})
	if err != nil {
		handler.writeError(writer, request, err)
		return
	}
	handler.startSubsystemDeploymentJob(request, subsystemDeploymentJob{
		Operation:          "ONBOARD",
		Generation:         generation,
		TenantID:           principal.Tenant.ID,
		OperatorUserID:     principal.User.ID,
		ApplicationCode:    result.Application.Code,
		ApplicationName:    result.Application.Name,
		ApplicationID:      result.Application.ID,
		EnvironmentID:      result.Environment.ID,
		Environment:        result.Environment.Environment,
		Input:              provisioningInput,
		InitialAdminUserID: initialAdminUserID,
		KeycloakClientID:   keycloakClientID,
		ManifestChecksum:   manifestChecksum,
	})
	accepted = true
	writer.Header().Set("Cache-Control", "no-store, private")
	writer.Header().Set("Pragma", "no-cache")
	httpresponse.WriteSuccess(writer, request, stdhttp.StatusAccepted, "接入请求已受理，部署正在后台执行；请轮询部署状态", map[string]string{
		"status":           application.SubsystemDeploymentStatusUpdating,
		"operation":        "ONBOARD",
		"application_code": result.Application.Code,
		"environment":      result.Environment.Environment,
	})
	handler.logger.Info("subsystem onboarding accepted", "path", request.URL.Path,
		"application_code", result.Application.Code, "environment", result.Environment.Environment,
		"actor_user_id", principal.User.ID, "actor_tenant_id", principal.Tenant.ID,
	)
}

// GetSubsystemCapabilities 暴露服务端配置的非敏感接入策略，供渲染管理表单；
// 权威校验仍由隔离部署 Agent 执行。
func (handler *SubsystemOnboardingHandler) GetSubsystemCapabilities(writer stdhttp.ResponseWriter, request *stdhttp.Request) {
	if _, ok := subsystemPrincipal(writer, request); !ok {
		return
	}
	capabilities := application.SubsystemProvisioningCapabilities{
		Enabled: false, Mode: "unknown", SupportedEnvironments: []string{},
	}
	if provider, ok := handler.provisioner.(subsystemProvisioningCapabilityProvider); ok {
		capabilities = provider.Capabilities()
	}
	if provider, ok := handler.provisioner.(subsystemAvailableCapabilityProvider); ok {
		available, err := provider.AvailableCapabilities(request.Context())
		if err != nil {
			handler.writeError(writer, request, err)
			return
		}
		capabilities = available
	}
	defaultEnvironment := strings.TrimSpace(capabilities.DefaultEnvironment)
	if defaultEnvironment == "" && len(capabilities.SupportedEnvironments) > 0 {
		defaultEnvironment = capabilities.SupportedEnvironments[0]
	}
	if defaultEnvironment == "" {
		defaultEnvironment = "dev"
	}
	defaultClientType := strings.TrimSpace(capabilities.DefaultClientType)
	if defaultClientType == "" {
		defaultClientType = "confidential"
	}
	targets := make([]subsystemProvisioningTargetResponse, 0, len(capabilities.Targets))
	for _, target := range capabilities.Targets {
		targets = append(targets, subsystemProvisioningTargetResponse{
			ApplicationCode:  target.ApplicationCode,
			ApplicationName:  target.ApplicationName,
			Description:      target.Description,
			Environment:      target.Environment,
			UpstreamURL:      target.UpstreamURL,
			PathPrefix:       target.PathPrefix,
			ClientType:       target.ClientType,
			ManifestChecksum: target.ManifestChecksum,
		})
	}
	response := subsystemProvisioningCapabilitiesResponse{
		AutomationEnabled:         capabilities.Enabled,
		DeploymentMode:            capabilities.Mode,
		SupportedApplicationCodes: append([]string(nil), capabilities.SupportedApplicationCodes...),
		SupportedEnvironments:     append([]string(nil), capabilities.SupportedEnvironments...),
		Targets:                   targets,
		Defaults: subsystemOnboardingDefaults{
			ApplicationCode: capabilities.DefaultApplicationCode,
			ApplicationName: capabilities.DefaultApplicationName,
			Description:     capabilities.DefaultDescription,
			Environment:     defaultEnvironment,
			PublicBaseURL:   handler.oidcIssuer,
			UpstreamURL:     capabilities.DefaultUpstreamURL,
			PathPrefix:      capabilities.DefaultPathPrefix,
			ClientType:      defaultClientType,
		},
		AuthenticationProviders: []subsystemAuthenticationProviderResponse{{
			Alias: "platform", Name: "基础平台 OIDC", Issuer: handler.oidcIssuer,
			Status: "READY", SwitchReady: true,
		}},
	}
	keycloak := subsystemAuthenticationProviderResponse{
		Alias: "keycloak", Name: "Keycloak", Issuer: handler.keycloakIssuer,
		Realm: handler.keycloakRealm, Status: "NOT_CONFIGURED",
		Detail:      "尚未由平台配置 Keycloak 管理连接和 Broker Client。",
		SwitchGates: unverifiedKeycloakSwitchReadiness().Gates,
		NextAction:  "完成 Keycloak 配置后，选择具体应用环境同步并验证四项门禁。",
	}
	if handler.keycloakEnabled && handler.keycloakControl != nil && handler.keycloakBroker != nil && handler.keycloakIssuer != "" && handler.keycloakRealm != "" {
		keycloak.Status = "READY"
		keycloak.Detail = "Keycloak 管理连接已就绪；仍需对具体应用环境完成四项门禁验证。"
		keycloak.NextAction = "先同步 Keycloak，再完成用户投影和 Broker 登录验证。"
	}
	response.AuthenticationProviders = append(response.AuthenticationProviders, keycloak)
	writer.Header().Set("Cache-Control", "no-store, private")
	writer.Header().Set("Pragma", "no-cache")
	httpresponse.WriteSuccess(writer, request, stdhttp.StatusOK, "子系统部署能力查询成功", response)
}

// UpdateSubsystem 处理 POST /api/v1/subsystem-update。调用方必须先经常规管理
// PATCH 端点更新数据库聚合（Environment 基址、OAuth redirect URI）；本端点只重跑
// provisioner，让运行中的子系统拿到新的 .env.local 值并重载门户网关。
func (handler *SubsystemOnboardingHandler) UpdateSubsystem(writer stdhttp.ResponseWriter, request *stdhttp.Request) {
	handler.updateSubsystem(writer, request, "", "UPDATE")
}

type updateServiceCredentialRequirement = application.SubsystemUpdateServiceCredentialRequirement

// AdoptSubsystem 把仅在目录中登记的环境纳入部署状态机。刻意与 update 分开：
// 调用方无法把未受管的运行时误当成"已部署"处理。
func (handler *SubsystemOnboardingHandler) AdoptSubsystem(writer stdhttp.ResponseWriter, request *stdhttp.Request) {
	handler.updateSubsystem(writer, request, "", "ADOPT")
}

func updateServiceCredentialRequirements(applicationCode string) []updateServiceCredentialRequirement {
	// 需求表已提取到应用层（逐行复制）；这里转调，使既有用例成为等价性证明。
	return application.SubsystemUpdateServiceCredentialRequirements(applicationCode)
}

// ensureUpdateServiceCredentials 准备受控更新的独立服务身份及只写密钥。
// 参数：ctx 控制调用，tenantID 为租户，applicationID/applicationCode 为应用，
//
//	environmentID/environment 为环境，operatorID 为操作人，operation 为更新模式。
//
// 返回值：应重新交付的凭据；身份漂移或签发失败时返回错误。
func (handler *SubsystemOnboardingHandler) ensureUpdateServiceCredentials(ctx context.Context, tenantID, applicationID, environmentID, applicationCode, environment, operatorID, operation string) ([]application.SubsystemServiceCredential, error) {
	requirements := updateServiceCredentialRequirements(applicationCode)
	if handler.serviceCredentials == nil || len(requirements) == 0 {
		return nil, nil
	}
	if strings.TrimSpace(applicationID) == "" || strings.TrimSpace(environmentID) == "" {
		return nil, application.ErrNotFound
	}
	clients, err := handler.serviceCredentials.ListOAuthClients(ctx, tenantID)
	if err != nil {
		return nil, err
	}
	byClientID := make(map[string]application.OAuthClientView, len(clients))
	for _, client := range clients {
		byClientID[client.ClientID] = client
	}
	credentials := make([]application.SubsystemServiceCredential, 0, len(requirements))
	for _, requirement := range requirements {
		clientID := applicationCode + "-" + environment + "-" + requirement.Suffix
		if client, ok := byClientID[clientID]; ok {
			if !strings.EqualFold(client.Status, "ACTIVE") || client.ApplicationID != applicationID || client.EnvironmentID != environmentID {
				return nil, application.ErrConflict
			}
			if application.RequiresStrictSubsystemUpdateServiceClientValidation(applicationCode, requirement.Purpose) &&
				(client.TenantID != tenantID || client.ClientType != "service" || client.TokenAuthMethod != "client_secret_basic" || !sameStringSet(client.GrantTypes, []string{"client_credentials"})) {
				return nil, application.ErrConflict
			}
			desiredScopes := serviceCredentialScopes(requirement)
			if !sameStringSet(client.Scopes, desiredScopes) {
				// 历史服务凭据可能在文件下载能力引入前创建。受控更新在签发新密钥前
				// 先整体修复 Scope，避免“密钥已下发但调用仍因旧权限 403”的半成功状态。
				client, err = handler.serviceCredentials.ReplaceOAuthClientScopes(ctx, application.OAuthClientScopesUpdateInput{
					TenantID: tenantID, OAuthClientID: client.ID, OperatorID: operatorID,
					Scopes: desiredScopes, Version: client.Version,
				})
				if err != nil {
					return nil, err
				}
			}
			// 接管与重试要生成可恢复的替换凭据：此前生成的 Secret 可能恰在 Agent 失败前
			// 刚刚签发。标记 rotate 的凭据在普通受控更新中同样会重新下发。
			if operation != "RETRY" && operation != "ADOPT" && !requirement.Rotate {
				if p1ServicePurpose(requirement.Purpose) {
					credentials = append(credentials, application.SubsystemServiceCredential{Purpose: requirement.Purpose, OAuthClient: client})
				}
				continue
			}
			var expectedVersion *uint64
			settlementPlatformIdentity := (applicationCode == "settlement" || applicationCode == "settlement_and_invoicing") &&
				application.RequiresStrictSubsystemUpdateServiceClientValidation(applicationCode, requirement.Purpose)
			if p1ServicePurpose(requirement.Purpose) || settlementPlatformIdentity {
				v := client.Version
				expectedVersion = &v
			}
			secret, secretErr := handler.serviceCredentials.CreateOAuthClientSecret(ctx, application.OAuthClientSecretCreateInput{
				TenantID: tenantID, OAuthClientID: client.ID, OperatorID: operatorID, ExpectedClientVersion: expectedVersion,
			})
			if secretErr != nil {
				return nil, secretErr
			}
			credentials = append(credentials, application.SubsystemServiceCredential{
				Purpose: requirement.Purpose, OAuthClient: client, PlaintextSecret: secret.PlaintextSecret,
			})
			continue
		}
		scopes := serviceCredentialScopes(requirement)
		created, createErr := handler.serviceCredentials.CreateOAuthClient(ctx, application.OAuthClientCreateInput{
			TenantID: tenantID, ApplicationID: applicationID, EnvironmentID: environmentID, OperatorID: operatorID,
			ClientID: clientID, ClientName: requirement.ClientName, ClientType: "service",
			TokenAuthMethod: "client_secret_basic", AccessTokenTTLSeconds: 15 * 60,
			GrantTypes: []string{"client_credentials"}, Scopes: scopes,
		})
		if createErr != nil {
			return nil, createErr
		}
		credentials = append(credentials, application.SubsystemServiceCredential{
			Purpose: requirement.Purpose, OAuthClient: created.Client, PlaintextSecret: created.PlaintextSecret,
		})
	}
	return credentials, nil
}

// ensureInitialPlatformBrowserCredential 面向只写的平台浏览器初始 Secret：
// 仅受信任的首次接管/重试路径可为既有绑定客户端铸造新版本。
func (handler *SubsystemOnboardingHandler) ensureInitialPlatformBrowserCredential(ctx context.Context, tenantID, applicationID, environmentID, applicationCode, environment, operatorID, redirectURI string) (application.OAuthClientView, string, error) {
	if handler.serviceCredentials == nil || applicationID == "" || environmentID == "" {
		return application.OAuthClientView{}, "", application.ErrSubsystemProvisioningUnavailable
	}
	clients, err := handler.serviceCredentials.ListOAuthClients(ctx, tenantID)
	if err != nil {
		return application.OAuthClientView{}, "", err
	}
	canonical := strings.ToLower(applicationCode) + "-" + strings.ToLower(environment) + "-web"
	for _, client := range clients {
		if client.ClientID != canonical {
			continue
		}
		if client.ApplicationID != applicationID || client.EnvironmentID != environmentID || !strings.EqualFold(client.Status, "ACTIVE") || client.ClientType != "confidential" || client.TokenAuthMethod != "client_secret_basic" || !containsExactString(client.GrantTypes, "authorization_code") || !client.RequirePKCE || !containsExactString(client.RedirectURIs, redirectURI) {
			return application.OAuthClientView{}, "", application.ErrConflict
		}
		secret, err := handler.serviceCredentials.CreateOAuthClientSecret(ctx, application.OAuthClientSecretCreateInput{TenantID: tenantID, OAuthClientID: client.ID, OperatorID: operatorID})
		if err != nil {
			return application.OAuthClientView{}, "", err
		}
		if strings.TrimSpace(secret.PlaintextSecret) == "" {
			return application.OAuthClientView{}, "", application.ErrSubsystemProvisioningUnavailable
		}
		return client, secret.PlaintextSecret, nil
	}
	if strings.TrimSpace(redirectURI) == "" {
		return application.OAuthClientView{}, "", application.ErrValidation
	}
	created, err := handler.serviceCredentials.CreateOAuthClient(ctx, application.OAuthClientCreateInput{
		TenantID: tenantID, ApplicationID: applicationID, EnvironmentID: environmentID, OperatorID: operatorID,
		ClientID: canonical, ClientName: applicationCode + " Web", ClientType: "confidential", TokenAuthMethod: "client_secret_basic",
		AccessTokenTTLSeconds: 15 * 60, RefreshTokenTTLSeconds: 30 * 24 * 60 * 60,
		RequirePKCE: true, GrantTypes: []string{"authorization_code", "refresh_token"}, Scopes: []string{"openid", "profile"}, RedirectURIs: []string{redirectURI},
	})
	if err != nil {
		return application.OAuthClientView{}, "", err
	}
	if strings.TrimSpace(created.PlaintextSecret) == "" {
		return application.OAuthClientView{}, "", application.ErrSubsystemProvisioningUnavailable
	}
	return created.Client, created.PlaintextSecret, nil
}

func containsExactString(values []string, expected string) bool {
	for _, value := range values {
		if value == expected {
			return true
		}
	}
	return false
}

// serviceCredentialScopes 返回指定机器凭据的完整最小权限集合。
// 文件网关客户端需要上传、绑定及按文件 ID 回读；其他用途保持单一 Scope。
func serviceCredentialScopes(requirement updateServiceCredentialRequirement) []string {
	scopes := []string{requirement.PrimaryScope()}
	if requirement.Purpose == application.ServiceCredentialFileGatewayWrite {
		scopes = append(scopes, "platform:file:bind", "platform:file:download")
	}
	return scopes
}

// sameStringSet 比较两个已授权 Scope 集合，忽略顺序但不忽略缺失或多余权限。
func sameStringSet(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	seen := make(map[string]int, len(left))
	for _, value := range left {
		seen[value]++
	}
	for _, value := range right {
		if seen[value] == 0 {
			return false
		}
		seen[value]--
	}
	return true
}

// ensureCatalogPublisherCredential 为受控重试恢复目录发布能力，且绝不尝试读取
// 已存储的 Secret。Secret 是只写的：新版本只为受信任的部署 Agent 铸造，
// 绝不返回给浏览器。
func (handler *SubsystemOnboardingHandler) ensureCatalogPublisherCredential(ctx context.Context, tenantID, applicationID, environmentID, applicationCode, environment, operatorID string) (application.OAuthClientView, string, error) {
	if handler.serviceCredentials == nil {
		return application.OAuthClientView{}, "", application.ErrSubsystemProvisioningUnavailable
	}
	clientID := applicationCode + "-" + environment + "-catalog-publisher"
	clients, err := handler.serviceCredentials.ListOAuthClients(ctx, tenantID)
	if err != nil {
		return application.OAuthClientView{}, "", err
	}
	for _, client := range clients {
		if client.ClientID != clientID {
			continue
		}
		if !strings.EqualFold(client.Status, "ACTIVE") || client.ApplicationID != applicationID || client.EnvironmentID != environmentID {
			return application.OAuthClientView{}, "", application.ErrConflict
		}
		secret, secretErr := handler.serviceCredentials.CreateOAuthClientSecret(ctx, application.OAuthClientSecretCreateInput{TenantID: tenantID, OAuthClientID: client.ID, OperatorID: operatorID})
		if secretErr != nil {
			return application.OAuthClientView{}, "", secretErr
		}
		return client, secret.PlaintextSecret, nil
	}
	created, createErr := handler.serviceCredentials.CreateOAuthClient(ctx, application.OAuthClientCreateInput{
		TenantID: tenantID, ApplicationID: applicationID, EnvironmentID: environmentID, OperatorID: operatorID,
		ClientID: clientID, ClientName: applicationCode + " Authorization Catalog Publisher", ClientType: "service",
		TokenAuthMethod: "client_secret_basic", AccessTokenTTLSeconds: 15 * 60,
		GrantTypes: []string{"client_credentials"}, Scopes: []string{"authorization.catalog.sync"},
	})
	if createErr != nil {
		return application.OAuthClientView{}, "", createErr
	}
	return created.Client, created.PlaintextSecret, nil
}

func (handler *SubsystemOnboardingHandler) updateSubsystem(writer stdhttp.ResponseWriter, request *stdhttp.Request, forcedIssuerAlias, requestedOperation string) {
	extendSubsystemDeploymentWriteDeadline(writer)
	principal, ok := subsystemPrincipal(writer, request)
	if !ok {
		return
	}
	var payload subsystemLifecycleRequest
	if !decodeApplicationManagementJSON(writer, request, &payload) {
		return
	}
	if err := validateLifecycleRequest(payload); err != nil {
		handler.writeError(writer, request, err)
		return
	}
	if forcedIssuerAlias != "" {
		payload.IssuerAlias = forcedIssuerAlias
	}
	applicationCode := strings.TrimSpace(payload.ApplicationCode)
	environment := strings.ToLower(strings.TrimSpace(payload.Environment))
	// 基础平台应用自身是登录与授权控制面根节点，其运行时由平台发布流程管理，不属于
	// subsystems.d 审核清单。若把它的环境交给子系统部署 Agent，生产模式会因为缺少清单
	// 校验和而失败，并在页面上留下误导性的“依赖服务不可用”。这里在写入任何生命周期
	// 状态之前明确拒绝，与应用的退役/环境删除保护保持一致。
	if isBuiltInPlatformApplication(applicationCode) {
		writeBuiltInPlatformRuntimeBlocked(writer, request, applicationCode, environment)
		return
	}
	isRetry := requestedOperation == "RETRY" || strings.HasSuffix(strings.TrimRight(request.URL.Path, "/"), "/subsystem-retry")
	// 平台签发的浏览器密钥不在普通更新中轮换。Keycloak 环境则需重新读取当前
	// RP 密钥并交给 Agent，修复运行配置漂移；读取不是轮换。授权目录发布凭据
	// 是应用绑定的机器身份，
	// 每次受控更新都必须重新下发，才能修复旧环境中遗留的占位值或已失效密钥。
	effectiveIssuerAlias, aliasErr := handler.deploymentIssuerAlias(request.Context(), principal.Tenant.ID, applicationCode, environment, payload.IssuerAlias, forcedIssuerAlias)
	if aliasErr != nil {
		handler.writeError(writer, request, aliasErr)
		return
	}
	// 显式 /switch 请求始终视为一次切换尝试，即使旧版浏览器已先行乐观更新了
	// issuer_alias。否则通用元数据写入可能意外绕过观察期门禁。
	keycloakCutover := strings.EqualFold(effectiveIssuerAlias, "keycloak") && (strings.EqualFold(forcedIssuerAlias, "keycloak") || handler.keycloakCutoverRequired(request.Context(), principal.Tenant.ID, applicationCode, environment))
	if keycloakCutover {
		readiness := handler.keycloakSwitchReadiness(request.Context(), principal.Tenant.ID, applicationCode, environment)
		if !readiness.SwitchReady {
			writeKeycloakSwitchBlocked(writer, request, readiness)
			return
		}
		if handler.keycloakCutover == nil {
			handler.writeError(writer, request, application.ErrSubsystemProvisioningUnavailable)
			return
		}
		if err := handler.keycloakCutover.CanKeycloakCutover(request.Context(), principal.Tenant.ID, applicationCode, environment); err != nil {
			// 安全（SEC-B3）：cause 只进结构化日志，HTTP details 用稳定错误码。
			handler.logger.Warn("Keycloak cutover blocked by observation gate", "application_code", applicationCode, "environment", environment, "error", err)
			writeKeycloakObservationBlocked(writer, request, keycloakObservationReasonCode(err))
			return
		}
	}
	// 与切换同理，显式回滚不得依赖浏览器此前的 issuer_alias 更新。生命周期存储是
	// 回滚截止期限的事实来源，并对仅平台环境安全拒绝。
	keycloakRollback := strings.EqualFold(forcedIssuerAlias, "platform")
	if keycloakRollback {
		if handler.keycloakCutover == nil {
			handler.writeError(writer, request, application.ErrSubsystemProvisioningUnavailable)
			return
		}
		if err := handler.keycloakCutover.CanKeycloakRollback(request.Context(), principal.Tenant.ID, applicationCode, environment); err != nil {
			// 安全（SEC-B3）：cause 只进结构化日志，HTTP details 用稳定错误码。
			handler.logger.Warn("Keycloak rollback blocked by observation gate", "application_code", applicationCode, "environment", environment, "error", err)
			writeKeycloakObservationBlocked(writer, request, keycloakObservationReasonCode(err))
			return
		}
	}
	issuer, err := handler.issuerForAlias(effectiveIssuerAlias)
	if err != nil {
		handler.writeError(writer, request, err)
		return
	}
	// 网关字段的裁剪与派生规则在应用层统一实现（BuildSubsystemUpdateProvisioningInput）；
	// 这里保留局部变量供后续受控步骤（凭据刷新、发布者凭据）复用。
	publicBaseURL := strings.TrimRight(strings.TrimSpace(payload.PublicBaseURL), "/")
	upstreamURL := strings.TrimRight(strings.TrimSpace(payload.UpstreamURL), "/")
	pathPrefix := strings.TrimRight(strings.TrimSpace(payload.PathPrefix), "/")
	// 装配逻辑提取到应用层（BuildSubsystemUpdateProvisioningInput）：统一入口与旧路径共用同一份派生与校验，
	// 避免两套实现漂移；凭据仍在后续受控步骤中解析后填充。
	updateInput, err := application.BuildSubsystemUpdateProvisioningInput(application.SubsystemUpdateProvisioningAssembly{
		TenantID: principal.Tenant.ID, ApplicationCode: applicationCode, Environment: environment,
		ManifestChecksum: handler.manifestChecksumForTarget(applicationCode, environment),
		Issuer:           issuer, AuthenticationProvider: effectiveIssuerAlias,
		AuthenticationRuntimeUpdate: keycloakCutover || keycloakRollback,
		PublicBaseURL:               payload.PublicBaseURL, UpstreamURL: payload.UpstreamURL, PathPrefix: payload.PathPrefix,
	})
	if err != nil {
		handler.writeError(writer, request, err)
		return
	}
	// 生产模式下 Agent 只接受随发布包审核的清单目标，并要求请求携带清单校验和。API 侧
	// 若解析不到目标（清单缺失，或 platform-api 与 subsystem-provisioner 的部署模式
	// 不一致），继续调用 Agent 只会得到无信息的拒绝并污染部署状态。这里先给出可执行的
	// 诊断，避免用户看到重复的 "subsystem provisioning unavailable"。
	if capabilityProvider, ok := handler.provisioner.(subsystemProvisioningCapabilityProvider); ok {
		if strings.EqualFold(strings.TrimSpace(capabilityProvider.Capabilities().Mode), "production") &&
			strings.TrimSpace(updateInput.ManifestChecksum) == "" {
			writeProductionManifestTargetMissing(writer, request, applicationCode, environment)
			return
		}
	}
	// 每次携带显式网关字段的受控 Keycloak 重建都要调和 RP 凭据，而不只是 ADOPT/RETRY。
	// 此前普通 UPDATE 会保留过期的运行时 Secret，即使 Client 映射与容器都健康，
	// OIDC 回调仍会报 unauthorized_client。EnsureClient 只读取既有 Secret，
	// 不轮换、也不暴露给浏览器。
	keycloakRuntimeCredentialRefresh := strings.EqualFold(effectiveIssuerAlias, "keycloak") &&
		!keycloakCutover && strings.TrimSpace(updateInput.RedirectURI) != ""
	ensureKeycloakCredentials := func(applicationID, environmentID string) bool {
		if keycloakRuntimeCredentialRefresh {
			if handler.keycloakControl == nil {
				handler.writeError(writer, request, application.ErrSubsystemProvisioningUnavailable)
				return false
			}
			canonicalClientID := strings.ToLower(applicationCode) + "-" + strings.ToLower(environment) + "-web"
			resolution := KeycloakClientResolution{ClientID: canonicalClientID, CanonicalClientID: canonicalClientID, Source: "canonical"}
			if resolver, ok := handler.keycloakMappings.(keycloakClientCompatibilityResolver); ok {
				effective, resolveErr := resolver.ResolveEffectiveKeycloakClient(request.Context(), principal.Tenant.ID, applicationID, environmentID, canonicalClientID)
				if resolveErr != nil {
					handler.writeError(writer, request, application.ErrSubsystemProvisioningUnavailable)
					return false
				}
				if strings.TrimSpace(effective.ClientID) != "" {
					resolution = effective
				}
			}
			client, clientErr := handler.keycloakControl.EnsureClient(request.Context(), resolution.ClientID, "Basic Platform "+resolution.ClientID, updateInput.RedirectURI)
			if clientErr != nil || client.ClientID != resolution.ClientID || strings.TrimSpace(client.ClientSecret) == "" {
				handler.writeError(writer, request, application.ErrSubsystemProvisioningUnavailable)
				return false
			}
			updateInput.ClientID, updateInput.ClientSecret = client.ClientID, client.ClientSecret
			updateInput.AuthenticationRuntimeUpdate = true
		}
		if keycloakCutover {
			transport, transportErr := application.ValidateKeycloakCutoverTransport(publicBaseURL, pathPrefix, handler.keycloakRequireHTTPS)
			if transportErr != nil {
				handler.writeError(writer, request, transportErr)
				return false
			}
			updateInput.PublicURL = transport.PublicURL
			updateInput.RedirectURI = transport.RedirectURI
			if handler.keycloakControl == nil || handler.keycloakBroker == nil || publicBaseURL == "" || pathPrefix == "" {
				handler.writeError(writer, request, application.ErrValidation)
				return false
			}
			if err := handler.reconcileKeycloakBroker(request.Context(), principal.Tenant.ID, principal.User.ID); err != nil {
				handler.writeError(writer, request, application.ErrSubsystemProvisioningUnavailable)
				return false
			}
			canonicalClientID := strings.ToLower(applicationCode) + "-" + strings.ToLower(environment) + "-web"
			resolution := KeycloakClientResolution{ClientID: canonicalClientID, CanonicalClientID: canonicalClientID, Source: "canonical"}
			if applicationID, environmentID, resolved := handler.resolveApplicationContext(writer, request, applicationCode, environment); resolved {
				if compatibilityResolver, supported := handler.keycloakMappings.(keycloakClientCompatibilityResolver); supported {
					if effective, resolveErr := compatibilityResolver.ResolveEffectiveKeycloakClient(request.Context(), principal.Tenant.ID, applicationID, environmentID, canonicalClientID); resolveErr != nil {
						handler.logger.Warn("Keycloak Client compatibility resolution failed", "application_code", applicationCode, "environment", environment, "error", resolveErr)
					} else if strings.TrimSpace(effective.ClientID) != "" {
						resolution = effective
					}
				}
			}
			client, clientErr := handler.keycloakControl.EnsureClient(request.Context(), resolution.ClientID, "Basic Platform "+resolution.ClientID, updateInput.RedirectURI)
			if clientErr != nil {
				handler.logger.Warn("Keycloak Client synchronization before switch failed", "application_code", applicationCode, "environment", environment, "error", clientErr)
				handler.writeError(writer, request, application.ErrSubsystemProvisioningUnavailable)
				return false
			}
			updateInput.ClientID, updateInput.ClientSecret = client.ClientID, client.ClientSecret
		}
		return true
	}
	// 标识符从部署控制面解析，而不是门户投影：失败与更新中的环境被刻意隐藏在
	// 用户可见的门户目录之外。
	var deploymentContext application.SubsystemDeploymentState
	environmentID := ""
	if handler.deploymentState != nil {
		state, err := handler.deploymentState.GetSubsystemDeploymentContext(request.Context(), principal.Tenant.ID, applicationCode, environment)
		if err != nil {
			// 仅目录登记的应用/环境还没有部署状态行。首次部署失败后 UI 以"重试"暴露
			// 恢复动作，因此在该边界把 RETRY 视同 ADOPT。ResolveApplicationEnvironment
			// 仍会校验目标是已知的租户内应用；未知目标保持 404。
			if requestedOperation != "ADOPT" && !isRetry || !errors.Is(err, application.ErrNotFound) {
				handler.writeError(writer, request, err)
				return
			}
			applicationID, resolvedEnvironmentID, resolveErr := handler.service.ResolveApplicationEnvironment(request.Context(), principal.Tenant.ID, applicationCode, environment)
			if resolveErr != nil {
				handler.writeError(writer, request, resolveErr)
				return
			}
			deploymentContext = application.SubsystemDeploymentState{TenantID: principal.Tenant.ID, ApplicationID: applicationID, EnvironmentID: resolvedEnvironmentID, ApplicationCode: applicationCode, Environment: environment}
			updateInput.ApplicationID = applicationID
			environmentID = resolvedEnvironmentID
		} else {
			deploymentContext = state
			updateInput.ApplicationID = state.ApplicationID
			environmentID = state.EnvironmentID
		}
	} else if applicationID, resolvedEnvironmentID, ok := handler.resolveApplicationContext(writer, request, applicationCode, environment); ok {
		updateInput.ApplicationID = applicationID
		environmentID = resolvedEnvironmentID
	}
	if handler.deploymentState != nil && (deploymentContext.Status == application.SubsystemDeploymentStatusProvisioning ||
		deploymentContext.Status == application.SubsystemDeploymentStatusUpdating ||
		deploymentContext.Status == application.SubsystemDeploymentStatusVerifying) {
		if staleSubsystemDeployment(deploymentContext, time.Now().UTC()) {
			// 过期的在途状态先按既有语义收口为失败，让本次受控操作可以继续。
			deploymentContext = handler.recoverStaleSubsystemDeployment(request.Context(), deploymentContext)
		}
		if deploymentContext.Status == application.SubsystemDeploymentStatusProvisioning ||
			deploymentContext.Status == application.SubsystemDeploymentStatusUpdating ||
			deploymentContext.Status == application.SubsystemDeploymentStatusVerifying {
			// 真正的在途部署：拒绝并发编排，而不是让两个部署互相覆盖状态机。
			httpresponse.WriteError(writer, request, stdhttp.StatusConflict, httperror.New(
				"SUBSYSTEM_DEPLOYMENT_IN_PROGRESS",
				"该应用环境的部署正在进行中；请等待完成或稍后刷新状态",
				map[string]string{"application_code": applicationCode, "environment": environment}))
			return
		}
	}
	if strings.TrimSpace(updateInput.ApplicationID) == "" {
		handler.writeError(writer, request, application.ErrNotFound)
		return
	}
	if publisherID, publisherSecret, ok := readCatalogPublisherCredentials(); ok {
		updateInput.CatalogPublisherClientID = publisherID
		updateInput.CatalogPublisherClientSecret = publisherSecret
	}
	operation := requestedOperation
	if operation == "" {
		operation = "UPDATE"
	}
	if isRetry {
		operation = "RETRY"
	}
	if provider, ok := handler.provisioner.(subsystemProvisioningCapabilityProvider); ok {
		updateInput.InitialRuntimeProvisioning = handler.deploymentState != nil && strings.EqualFold(provider.Capabilities().Mode, "production") &&
			(operation == "ADOPT" || (operation == "RETRY" && deploymentContext.Status == application.SubsystemDeploymentStatusFailed)) && deploymentContext.AppliedManifestChecksum == "" && deploymentContext.ManifestLastAppliedAt == nil
	}
	if requiresCatalogPublisherCredential(applicationCode) && handler.serviceCredentials == nil {
		// 授权目录发布凭据为只写密钥，缺少凭据管理器时绝不能假装更新成功并让
		// Agent 启动一个仍使用占位值的容器。
		handler.writeError(writer, request, application.ErrSubsystemProvisioningUnavailable)
		return
	}
	// 无变化跳过：纯“更新运行时”重放（未携带任何网关字段变更、未切换认证方），且期望
	// 清单与已应用清单一致、环境本就 READY 时，受控部署等价于对同一清单再跑一次
	// compose build——既浪费数分钟构建窗口，也让离线/受限网络环境因构建出网失败而无法
	// 完成本应成功的操作。直接确认现状并保持 READY；需要强制重建时使用 RETRY（全量）。
	if operation == "UPDATE" && !keycloakCutover && !keycloakRollback &&
		strings.TrimSpace(publicBaseURL) == "" && strings.TrimSpace(upstreamURL) == "" && strings.TrimSpace(pathPrefix) == "" &&
		deploymentContext.Status == application.SubsystemDeploymentStatusReady &&
		deploymentContext.DesiredManifestChecksum != "" &&
		deploymentContext.DesiredManifestChecksum == deploymentContext.AppliedManifestChecksum &&
		deploymentContext.DesiredManifestChecksum == updateInput.ManifestChecksum {
		handler.logger.Info("subsystem update skipped: deployment manifest unchanged",
			"application_code", applicationCode, "environment", environment,
			"manifest_checksum", updateInput.ManifestChecksum)
		writer.Header().Set("Cache-Control", "no-store, private")
		writer.Header().Set("Pragma", "no-cache")
		httpresponse.WriteSuccess(writer, request, stdhttp.StatusOK, "部署清单无变化，运行时保持现状", map[string]string{
			"status":           application.SubsystemDeploymentStatusReady,
			"operation":        operation,
			"application_code": applicationCode,
			"environment":      environment,
		})
		return
	}
	generation := uint64(0)
	if handler.deploymentState != nil {
		var claimErr error
		generation, claimErr = handler.deploymentState.ClaimSubsystemDeployment(request.Context(), principal.Tenant.ID, applicationCode, environment, operation, time.Now().UTC())
		if claimErr != nil {
			if errors.Is(claimErr, application.ErrSubsystemDeploymentTransition) {
				httpresponse.WriteError(writer, request, stdhttp.StatusConflict, httperror.New("SUBSYSTEM_DEPLOYMENT_IN_PROGRESS", "该应用环境的部署正在进行中", nil))
			} else {
				handler.writeError(writer, request, claimErr)
			}
			return
		}
	}
	accepted := false
	defer func() {
		if !accepted {
			handler.markDeploymentFailureDetached(subsystemDeploymentJob{TenantID: principal.Tenant.ID, ApplicationCode: applicationCode, Environment: environment, Operation: operation, Generation: generation}, "DEPLOYMENT_PREPARATION_FAILED", "部署准备失败，请点击重试")
		}
	}()
	relatedClients, relatedErr := handler.relatedServiceClients(request.Context(), principal.Tenant.ID, applicationCode, environment)
	if relatedErr != nil {
		handler.writeError(writer, request, relatedErr, "contract integration dependency")
		return
	}
	updateInput.RelatedServiceClients = relatedClients
	if !ensureKeycloakCredentials(updateInput.ApplicationID, environmentID) {
		return
	}
	if updateInput.InitialRuntimeProvisioning && !strings.EqualFold(effectiveIssuerAlias, "keycloak") {
		client, secret, err := handler.ensureInitialPlatformBrowserCredential(request.Context(), principal.Tenant.ID, updateInput.ApplicationID, environmentID, applicationCode, environment, principal.User.ID, updateInput.RedirectURI)
		if err != nil {
			handler.writeError(writer, request, err)
			return
		}
		updateInput.ClientID, updateInput.ClientSecret = client.ClientID, secret
		updateInput.AuthenticationRuntimeUpdate = true
	}
	serviceCredentials, credentialErr := handler.ensureUpdateServiceCredentials(
		request.Context(), principal.Tenant.ID, updateInput.ApplicationID, environmentID,
		applicationCode, environment, principal.User.ID, operation,
	)
	if credentialErr != nil {
		handler.writeError(writer, request, credentialErr)
		return
	}
	updateInput.ServiceCredentials = serviceCredentials
	updateInput.EnvironmentID = environmentID

	if handler.serviceCredentials != nil && requiresCatalogPublisherCredential(applicationCode) {
		publisher, publisherSecret, publisherErr := handler.ensureCatalogPublisherCredential(
			request.Context(), principal.Tenant.ID, updateInput.ApplicationID, environmentID, applicationCode, environment, principal.User.ID,
		)
		if publisherErr != nil {
			handler.writeError(writer, request, publisherErr)
			return
		}
		updateInput.CatalogPublisherClientID = publisher.ClientID
		updateInput.CatalogPublisherClientSecret = publisherSecret
	}
	initialAccessUserID := principal.User.ID
	needsInitialAccess := operation == "ADOPT"
	if handler.deploymentState != nil && operation != "ADOPT" {
		if storedAdminUserID := strings.TrimSpace(deploymentContext.InitialAdminUserID); storedAdminUserID != "" {
			initialAccessUserID = storedAdminUserID
		}
		// UPDATE 与 RETRY 都是受控部署操作。升级迁移可以把历史上被错误标记为
		// “已分配”的初始访问重置为待补偿；无论用户从页面选择更新还是重试，均应在
		// Agent 发布最新角色目录后幂等补发一次。非空 marker 仍保持“不主动恢复已撤销
		// 权限”的既有语义。
		needsInitialAccess = deploymentContext.InitialAccessAssignedAt == nil
	}
	if err := handler.setDesiredManifest(request.Context(), principal.Tenant.ID, applicationCode, environment, updateInput.ManifestChecksum); err != nil {
		handler.writeError(writer, request, err)
		return
	}
	jobApplicationID := deploymentContext.ApplicationID
	if jobApplicationID == "" {
		jobApplicationID = updateInput.ApplicationID
	}
	jobEnvironmentID := deploymentContext.EnvironmentID
	if jobEnvironmentID == "" {
		jobEnvironmentID = environmentID
	}
	if isRetry && handler.keycloakBroker != nil {
		// 重试通常紧随一次失败的部署：SSO 末端的 broker 凭据可能仍处缺口（自愈巡检
		// 有周期）。这里尽力补齐一次；失败仅告警——broker 缺口不阻断部署本身，只影响
		// 部署完成后的首次统一登录。
		if err := handler.reconcileKeycloakBroker(request.Context(), principal.Tenant.ID, principal.User.ID); err != nil {
			handler.logger.Warn("retry could not reconcile Keycloak broker credential",
				"application_code", applicationCode, "environment", environment, "error", err)
		}
	}
	handler.startSubsystemDeploymentJob(request, subsystemDeploymentJob{
		Operation:          operation,
		Generation:         generation,
		TenantID:           principal.Tenant.ID,
		OperatorUserID:     principal.User.ID,
		ApplicationCode:    applicationCode,
		ApplicationName:    applicationCode,
		ApplicationID:      jobApplicationID,
		EnvironmentID:      jobEnvironmentID,
		Environment:        environment,
		Input:              updateInput,
		InitialAdminUserID: initialAccessUserID,
		NeedsInitialAccess: needsInitialAccess,
		KeycloakCutover:    keycloakCutover,
		KeycloakRollback:   keycloakRollback,
		ManifestChecksum:   updateInput.ManifestChecksum,
	})
	accepted = true
	writer.Header().Set("Cache-Control", "no-store, private")
	writer.Header().Set("Pragma", "no-cache")
	httpresponse.WriteSuccess(writer, request, stdhttp.StatusAccepted, "部署已受理，正在后台执行；状态请查询 subsystem-status", map[string]string{
		"status":           application.SubsystemDeploymentStatusUpdating,
		"operation":        operation,
		"application_code": applicationCode,
		"environment":      environment,
	})
	handler.logger.Info("subsystem deployment accepted", "path", request.URL.Path,
		"application_code", applicationCode, "environment", environment, "operation", operation,
		"actor_user_id", principal.User.ID, "actor_tenant_id", principal.Tenant.ID,
	)
}

// requiresCatalogPublisherCredential 判断子系统是否会在启动时发布自有授权目录。
// 该机器凭据是只写数据，部署中断后不能从运行时文件反向恢复，因此每次受控生命周期操作
// 都重新下发，避免旧环境继续使用占位值或失效密钥。
func requiresCatalogPublisherCredential(applicationCode string) bool {
	switch applicationCode {
	case "contract_management", "customer_and_opportunity", "customer_portal", "data_analysis", "project_management", "settlement":
		return true
	default:
		return false
	}
}

// TeardownSubsystem 处理 POST /api/v1/subsystem-teardown。停止容器、删除 .env.local、
// 移除门户网关 include 并重载 nginx。HTTP 层不在此删除对应数据库行：脚本随后调用
// DELETE /environments 与（可选的）DELETE /applications，让审计留痕保留每个清理步骤。
func (handler *SubsystemOnboardingHandler) TeardownSubsystem(writer stdhttp.ResponseWriter, request *stdhttp.Request) {
	extendSubsystemDeploymentWriteDeadline(writer)
	principal, ok := subsystemPrincipal(writer, request)
	if !ok {
		return
	}
	var payload subsystemLifecycleRequest
	if !decodeApplicationManagementJSON(writer, request, &payload) {
		return
	}
	if err := validateLifecycleRequest(payload); err != nil {
		handler.writeError(writer, request, err)
		return
	}
	applicationCode := strings.TrimSpace(payload.ApplicationCode)
	environment := strings.ToLower(strings.TrimSpace(payload.Environment))
	// 基础平台自身的运行时不由子系统部署 Agent 管理；与更新路径一致，在清理任何
	// 运行时资源或写入生命周期状态之前拒绝，避免误停平台自身服务。
	if isBuiltInPlatformApplication(applicationCode) {
		writeBuiltInPlatformRuntimeBlocked(writer, request, applicationCode, environment)
		return
	}
	if err := handler.transitionDeployment(request.Context(), principal.Tenant.ID, applicationCode, environment, application.SubsystemDeploymentStatusDraining, "TEARDOWN", "", ""); err != nil {
		handler.writeError(writer, request, err)
		return
	}
	if err := handler.provisioner.Teardown(request.Context(), principal.Tenant.ID, applicationCode, environment); err != nil {
		handler.markDeploymentFailed(request.Context(), principal.Tenant.ID, applicationCode, environment, "TEARDOWN", "DEPLOYMENT_AGENT_FAILED", "部署 Agent 执行失败")
		handler.writeError(writer, request, err)
		return
	}
	if err := handler.transitionDeployment(request.Context(), principal.Tenant.ID, applicationCode, environment, application.SubsystemDeploymentStatusOffboarded, "TEARDOWN", "", ""); err != nil {
		handler.logger.Error("subsystem teardown completed but state update failed", "application_code", applicationCode, "environment", environment, "error", err)
		handler.writeError(writer, request, err)
		return
	}
	writer.Header().Set("Cache-Control", "no-store, private")
	writer.Header().Set("Pragma", "no-cache")
	httpresponse.WriteSuccess(writer, request, stdhttp.StatusOK, "子系统已拆解", map[string]string{
		"status":           "torn_down",
		"application_code": applicationCode,
		"environment":      environment,
	})
	handler.logger.Warn("subsystem torn down", "path", request.URL.Path,
		"application_code", applicationCode,
		"environment", environment,
		"actor_user_id", principal.User.ID, "actor_tenant_id", principal.Tenant.ID,
	)
}
