// subsystem_onboarding_cutover_handler.go：Keycloak 认证切换（keycloak-cutover）——切换门禁核验、代理登录证据校验、Client 同步与回滚入口。
package http

import (
	"context"
	stdhttp "net/http"
	"strings"

	"github.com/J-S-Te/Basic-Platform/internal/platform/applicationregistry/application"
	"github.com/J-S-Te/Basic-Platform/internal/shared/authctx"
	"github.com/J-S-Te/Basic-Platform/internal/shared/httperror"
	"github.com/J-S-Te/Basic-Platform/internal/shared/httpresponse"
	"github.com/J-S-Te/Basic-Platform/internal/shared/keycloakctx"
	"github.com/J-S-Te/Basic-Platform/internal/shared/requestctx"
)

func unverifiedKeycloakSwitchReadiness() KeycloakSwitchReadiness {
	return KeycloakSwitchReadiness{Gates: []KeycloakSwitchGate{
		{Key: "client_ready", Label: "Client 已就绪", Detail: "尚未针对当前应用环境验证 Realm Client。", NextAction: "先执行“同步 Keycloak”。"},
		{Key: "role_catalog_synced", Label: "角色目录已同步", Detail: "尚未针对当前应用环境验证角色目录。", NextAction: "同步 Keycloak 后检查角色目录。"},
		{Key: "user_projection_completed", Label: "用户投影已完成", Detail: "服务器没有可审计的用户投影完成证据。", NextAction: "完成用户投影，并接入服务器端状态回传。"},
		{Key: "broker_login_verified", Label: "Broker 登录验证已通过", Detail: "服务器没有可审计的 Broker 登录验证证据。", NextAction: "使用目标应用完成一次 Broker 登录验证，并接入服务器端状态回传。"},
	}, NextAction: "四项门禁均需由服务器验证后才能切换 Issuer。"}
}

func keycloakSyncReadiness() KeycloakSwitchReadiness {
	readiness := unverifiedKeycloakSwitchReadiness()
	readiness.Gates[0] = KeycloakSwitchGate{Key: "client_ready", Label: "Client 已就绪", Passed: true, Detail: "本次同步已成功创建或更新 Realm Client。"}
	readiness.Gates[1] = KeycloakSwitchGate{Key: "role_catalog_synced", Label: "角色目录已同步", Passed: true, Detail: "本次同步已成功写入当前应用角色目录。"}
	return readiness
}

func (handler *SubsystemOnboardingHandler) keycloakSwitchReadiness(ctx context.Context, tenantID, applicationCode, environment string) KeycloakSwitchReadiness {
	if handler.keycloakReadiness == nil {
		return unverifiedKeycloakSwitchReadiness()
	}
	readiness, err := handler.keycloakReadiness.InspectKeycloakSwitchReadiness(ctx, tenantID, applicationCode, environment)
	if err != nil || len(readiness.Gates) == 0 {
		return unverifiedKeycloakSwitchReadiness()
	}
	required := map[string]bool{"client_ready": false, "role_catalog_synced": false, "user_projection_completed": false, "broker_login_verified": false}
	for _, gate := range readiness.Gates {
		if _, requiredGate := required[gate.Key]; requiredGate {
			required[gate.Key] = gate.Passed
		}
		if !gate.Passed {
			readiness.SwitchReady = false
		}
	}
	for _, passed := range required {
		if !passed {
			readiness.SwitchReady = false
			return readiness
		}
	}
	readiness.SwitchReady = true
	return readiness
}

func writeKeycloakSwitchBlocked(writer stdhttp.ResponseWriter, request *stdhttp.Request, readiness KeycloakSwitchReadiness) {
	httpresponse.WriteError(writer, request, stdhttp.StatusConflict, httperror.New(
		"IAM_AUTH_PROVIDER_SWITCH_NOT_READY",
		"认证提供方尚未满足切换门禁，Issuer 未下发到运行时",
		map[string]any{"switch_gates": readiness.Gates, "next_action": readiness.NextAction},
	))
}

// writeKeycloakObservationBlocked 以稳定错误码回应观察期门禁失败。安全（SEC-B3）：
// 底层错误原文（可能含表名、SQL state、文件路径）只允许进入结构化日志，HTTP details
// 仅暴露稳定 reason_code 与本地化提示，遵守 httperror 的客户端安全契约。
func writeKeycloakObservationBlocked(writer stdhttp.ResponseWriter, request *stdhttp.Request, reasonCode string) {
	httpresponse.WriteError(writer, request, stdhttp.StatusConflict, httperror.New(
		"IAM_AUTH_PROVIDER_SWITCH_OBSERVATION_REQUIRED",
		"认证提供方尚未完成七天观察期，Issuer 未下发到运行时",
		map[string]any{"reason_code": reasonCode, "observation_window_days": 7, "next_action": "在 Keycloak 认证接入中发起观察；观察期完成后再切换。"},
	))
}

// keycloakObservationReasonCode 把观察期门禁错误归类为稳定代码；错误原文不出本函数。
func keycloakObservationReasonCode(err error) string {
	if err == nil {
		return "OBSERVATION_STATE_UNKNOWN"
	}
	message := strings.ToLower(err.Error())
	switch {
	case strings.Contains(message, "window has not completed"):
		return "OBSERVATION_WINDOW_NOT_COMPLETED"
	case strings.Contains(message, "has not started"):
		return "OBSERVATION_NOT_STARTED"
	case strings.Contains(message, "already exists"):
		return "OBSERVATION_ALREADY_EXISTS"
	case strings.Contains(message, "duration must be positive"):
		return "OBSERVATION_INVALID_DURATION"
	default:
		// 其余原因可能是底层 DB/GORM 错误的包装（SEC-B3），绝不透出原文。
		return "OBSERVATION_STATE_UNAVAILABLE"
	}
}

func (handler *SubsystemOnboardingHandler) keycloakCutoverRequired(ctx context.Context, tenantID, applicationCode, environment string) bool {
	// 常规装配下环境聚合由接入服务持有；保留状态存储回退，兼容把该可选查询
	// 暴露在状态存储上的旧装配。
	for _, candidate := range []any{handler.service, handler.deploymentState} {
		resolver, ok := candidate.(subsystemEnvironmentIssuerResolver)
		if !ok {
			continue
		}
		currentAlias, err := resolver.ResolveEnvironmentIssuerAlias(ctx, tenantID, applicationCode, environment)
		if err == nil {
			return !strings.EqualFold(strings.TrimSpace(currentAlias), "keycloak")
		}
	}
	// 未知状态必须保持失败关闭：生产装配注入持久化 resolver，
	// 轻量调用方/测试沿用"首次切换即切换"规则。
	return true
}

// VerifyKeycloakBrokerLogin 从专用 Keycloak 用户 JWT 校验路由记录证据；
// 路由中间件先固定 Issuer 并验签，本 handler 再把令牌身份与受众绑定到
// 单个应用环境。
func (handler *SubsystemOnboardingHandler) VerifyKeycloakBrokerLogin(writer stdhttp.ResponseWriter, request *stdhttp.Request) {
	claims, ok := keycloakctx.BrokerClaimsFromContext(request.Context())
	if !ok {
		httpresponse.WriteError(writer, request, stdhttp.StatusUnauthorized, httperror.Unauthenticated)
		return
	}
	if strings.TrimSpace(claims.SessionID) == "" {
		httpresponse.WriteError(writer, request, stdhttp.StatusUnauthorized, httperror.Unauthenticated)
		return
	}
	var payload keycloakBrokerLoginVerificationRequest
	if !decodeApplicationManagementJSON(writer, request, &payload) {
		return
	}
	payload.ApplicationCode = strings.TrimSpace(payload.ApplicationCode)
	payload.Environment = strings.ToLower(strings.TrimSpace(payload.Environment))
	payload.IdentityID = strings.TrimSpace(payload.IdentityID)
	payload.Issuer = strings.TrimRight(strings.TrimSpace(payload.Issuer), "/")
	payload.ClientID = strings.TrimSpace(payload.ClientID)
	if payload.ApplicationCode == "" || payload.Environment == "" || payload.IdentityID == "" || payload.Issuer == "" || payload.ClientID == "" || payload.IdentityID != claims.IdentityID || !containsExactAudience(claims.Audience, payload.ClientID) {
		handler.writeError(writer, request, application.ErrValidation)
		return
	}
	if !handler.keycloakEnabled || handler.keycloakIssuer == "" || payload.Issuer != claims.Issuer || !strings.EqualFold(payload.Issuer, handler.keycloakIssuer) {
		handler.writeError(writer, request, application.ErrValidation)
		return
	}
	applicationID, environmentID, found := handler.resolveApplicationContextForIdentity(request, claims.TenantID, claims.IdentityID, payload.ApplicationCode, payload.Environment)
	if !found {
		// 不得泄露当前会话门户可见范围之外的目标是否存在。
		handler.writeError(writer, request, application.ErrValidation)
		return
	}
	recorder, ok := handler.keycloakReadiness.(keycloakBrokerLoginVerificationRecorder)
	if !ok {
		// 缺少持久化记录器属于失败关闭的部署配置。
		handler.writeError(writer, request, application.ErrSubsystemProvisioningUnavailable)
		return
	}
	if err := recorder.RecordBrokerLoginVerification(request.Context(), KeycloakBrokerLoginVerification{
		TenantID: claims.TenantID, ApplicationID: applicationID, EnvironmentID: environmentID,
		IdentityID: payload.IdentityID, Issuer: payload.Issuer, ClientID: payload.ClientID,
		VerifiedByID: claims.IdentityID, SessionID: claims.SessionID,
	}); err != nil {
		handler.logger.Warn("Keycloak broker login verification rejected", "application_code", payload.ApplicationCode, "environment", payload.Environment, "identity_id", payload.IdentityID, "error", err)
		handler.writeError(writer, request, application.ErrValidation)
		return
	}
	writer.Header().Set("Cache-Control", "no-store, private")
	writer.Header().Set("Pragma", "no-cache")
	httpresponse.WriteSuccess(writer, request, stdhttp.StatusOK, "Keycloak Broker 登录验证已记录；Issuer 仍受其余门禁保护", map[string]string{"status": "verified", "application_code": payload.ApplicationCode, "environment": payload.Environment})
}

func containsExactAudience(audience []string, expected string) bool {
	for _, value := range audience {
		if value == expected {
			return true
		}
	}
	return false
}

// SyncKeycloakClient 创建或更新所选子系统的 Keycloak RP 及其必需的 token claim mapper。
// 刻意与 Issuer 切换分离：Admin API 写入成功不能证明代理登录已被实测，
// 最终运行时切换保持失败关闭。
func (handler *SubsystemOnboardingHandler) SyncKeycloakClient(writer stdhttp.ResponseWriter, request *stdhttp.Request) {
	if _, ok := subsystemPrincipal(writer, request); !ok {
		return
	}
	if handler.keycloakControl == nil || handler.keycloakBroker == nil || !handler.keycloakEnabled {
		handler.writeError(writer, request, application.ErrValidation)
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
	principal, _ := authctx.PrincipalFromContext(request.Context())
	applicationID, environmentID, found := handler.resolveApplicationContext(writer, request, payload.ApplicationCode, payload.Environment)
	if !found || handler.keycloakCatalog == nil || handler.keycloakMappings == nil {
		handler.writeError(writer, request, application.ErrValidation)
		return
	}
	if err := handler.reconcileKeycloakBroker(request.Context(), principal.Tenant.ID, principal.User.ID); err != nil {
		handler.writeKeycloakSyncFailure(writer, request, payload, "broker", err)
		return
	}
	canonicalClientID := strings.ToLower(strings.TrimSpace(payload.ApplicationCode)) + "-" + strings.ToLower(strings.TrimSpace(payload.Environment)) + "-web"
	resolution := KeycloakClientResolution{ClientID: canonicalClientID, CanonicalClientID: canonicalClientID, Source: "canonical"}
	if resolver, ok := handler.keycloakMappings.(keycloakClientCompatibilityResolver); ok {
		resolved, resolveErr := resolver.ResolveEffectiveKeycloakClient(request.Context(), principal.Tenant.ID, applicationID, environmentID, canonicalClientID)
		if resolveErr != nil {
			handler.writeKeycloakSyncFailure(writer, request, payload, "client", resolveErr)
			return
		}
		if strings.TrimSpace(resolved.ClientID) != "" {
			resolution = resolved
		}
	}
	transport, transportErr := application.ValidateKeycloakCutoverTransport(payload.PublicBaseURL, payload.PathPrefix, handler.keycloakRequireHTTPS)
	if transportErr != nil {
		handler.writeError(writer, request, transportErr)
		return
	}
	redirectURI := transport.RedirectURI
	result, err := handler.keycloakControl.EnsureClient(request.Context(), resolution.ClientID, "Basic Platform "+resolution.ClientID, redirectURI)
	if err != nil {
		handler.writeKeycloakSyncFailure(writer, request, payload, "client", err)
		return
	}
	// 同步 Keycloak Client 不能只更新 Keycloak。授权上下文解析依赖平台自身的
	// platform_oauth_client 记录；旧环境如果只有 Keycloak Client，登录时会在
	// azp 校验阶段得到 403。先确认 Keycloak 写入成功，再以同一租户、应用和
	// 环境补齐平台 Web Client，避免失败时留下孤儿授权记录。
	if _, err := handler.ensureWebOAuthClient(request.Context(), principal.Tenant.ID, applicationID, environmentID, principal.User.ID, result.ClientID, payload.ApplicationCode, redirectURI); err != nil {
		handler.writeKeycloakSyncFailure(writer, request, payload, "oauth_client", err)
		return
	}
	roleCodes, err := handler.keycloakCatalog.ListKeycloakRoleCodes(request.Context(), principal.Tenant.ID, applicationID)
	if err != nil {
		handler.writeKeycloakSyncFailure(writer, request, payload, "roles", err)
		return
	}
	if err := handler.keycloakControl.EnsureClientRoles(request.Context(), result.ClientID, roleCodes); err != nil {
		handler.writeKeycloakSyncFailure(writer, request, payload, "roles", err)
		return
	}
	if err := handler.keycloakMappings.SaveKeycloakClientMapping(request.Context(), principal.Tenant.ID, applicationID, environmentID, handler.keycloakRealm, result.ClientID); err != nil {
		handler.writeKeycloakSyncFailure(writer, request, payload, "mapping", err)
		return
	}
	if err := handler.keycloakMappings.BackfillKeycloakAuthorization(request.Context(), principal.Tenant.ID, applicationID, environmentID); err != nil {
		handler.writeKeycloakSyncFailure(writer, request, payload, "backfill", err)
		return
	}
	if updater, ok := handler.keycloakReadiness.(keycloakSwitchReadinessUpdater); ok {
		if err := updater.MarkKeycloakClientAndRoleCatalogSynced(request.Context(), principal.Tenant.ID, applicationID, environmentID); err != nil {
			handler.writeKeycloakSyncFailure(writer, request, payload, "readiness", err)
			return
		}
	}
	writer.Header().Set("Cache-Control", "no-store, private")
	// 上述 Admin API 调用只证明本次请求的前两道门禁。部署若配有权威校验器，
	// 应向其索取完整的环境级证据；否则保留部分结果，不自行放行。
	readiness := keycloakSyncReadiness()
	if handler.keycloakReadiness != nil {
		readiness = handler.keycloakSwitchReadiness(request.Context(), principal.Tenant.ID, payload.ApplicationCode, payload.Environment)
	}
	httpresponse.WriteSuccess(writer, request, stdhttp.StatusOK, "Keycloak Realm Client、身份、组织、角色与权限 Claims 映射已同步；Issuer 仍被四项门禁保护", keycloakClientSyncResponse{Alias: "keycloak", Realm: handler.keycloakRealm, ClientID: result.ClientID, CanonicalClientID: resolution.CanonicalClientID, ClientIDSource: resolution.Source, LegacyCompatibility: resolution.LegacyCompatibility, ClaimsState: "MAPPERS_SYNCED", SwitchReady: readiness.SwitchReady, SwitchGates: readiness.Gates, NextAction: readiness.NextAction})
}

// ensureWebOAuthClient 确保平台授权上下文可以解析子系统使用的浏览器 Client。
// 已存在且绑定一致的 ACTIVE 记录直接复用；缺失记录使用授权码 + PKCE 创建。
// 明文密钥不向上层返回，避免同步接口泄露浏览器 Client 的运行时凭据。
func (handler *SubsystemOnboardingHandler) ensureWebOAuthClient(ctx context.Context, tenantID, applicationID, environmentID, operatorID, clientID, applicationCode, redirectURI string) (application.OAuthClientView, error) {
	if handler.serviceCredentials == nil {
		return application.OAuthClientView{}, application.ErrSubsystemProvisioningUnavailable
	}
	clients, err := handler.serviceCredentials.ListOAuthClients(ctx, tenantID)
	if err != nil {
		return application.OAuthClientView{}, err
	}
	for _, client := range clients {
		if client.ClientID != clientID {
			continue
		}
		if !strings.EqualFold(client.Status, "ACTIVE") || client.ApplicationID != applicationID || client.EnvironmentID != environmentID {
			return application.OAuthClientView{}, application.ErrConflict
		}
		return client, nil
	}
	created, err := handler.serviceCredentials.CreateOAuthClient(ctx, application.OAuthClientCreateInput{
		TenantID: tenantID, ApplicationID: applicationID, EnvironmentID: environmentID, OperatorID: operatorID,
		ClientID: clientID, ClientName: applicationCode + " Web", ClientType: "confidential",
		TokenAuthMethod: "client_secret_basic", AccessTokenTTLSeconds: 15 * 60,
		RequirePKCE: true, GrantTypes: []string{"authorization_code"}, Scopes: []string{"openid", "profile"},
		RedirectURIs: []string{redirectURI},
	})
	if err != nil {
		return application.OAuthClientView{}, err
	}
	return created.Client, nil
}

// writeKeycloakSyncFailure 保持对外契约刻意非敏感，同时保留足够阶段信息让运维
// 选择正确的修复动作。原始错误只在服务端记录，并以请求 ID 关联。
func (handler *SubsystemOnboardingHandler) writeKeycloakSyncFailure(writer stdhttp.ResponseWriter, request *stdhttp.Request, payload subsystemLifecycleRequest, stage string, err error) {
	detail, nextAction := keycloakSyncFailureGuidance(stage)
	handler.logger.Warn("Keycloak Client synchronization failed",
		"request_id", requestctx.RequestID(request.Context()),
		"stage", stage,
		"application_code", payload.ApplicationCode,
		"environment", payload.Environment,
		"error", err,
	)
	httpresponse.WriteError(writer, request, stdhttp.StatusServiceUnavailable, httperror.New(
		httperror.DependencyUnavailable.Code,
		httperror.DependencyUnavailable.Message,
		map[string]string{"stage": stage, "detail": detail, "next_action": nextAction},
	))
}

func keycloakSyncFailureGuidance(stage string) (detail, nextAction string) {
	switch stage {
	case "broker":
		return "Keycloak Broker 同步暂时不可用。", "检查 Keycloak 管理连接和平台 Broker 配置后，在当前环境重新同步。"
	case "client":
		return "Keycloak Realm Client 同步暂时不可用。", "检查目标 Realm、Client 配置与回调地址后，在当前环境重新同步。"
	case "oauth_client":
		return "平台 Web OAuth Client 同步暂时不可用。", "检查平台 OAuth Client 接入记录与租户应用环境关联后，在当前环境重新同步。"
	case "roles":
		return "Keycloak Client 角色目录同步暂时不可用。", "检查应用角色目录与 Keycloak Client Role 管理权限后，在当前环境重新同步。"
	case "mapping":
		return "Keycloak Client 映射保存暂时不可用。", "检查基础平台数据库与 Keycloak Client 映射状态后，在当前环境重新同步。"
	case "backfill":
		return "Keycloak 授权投影回填暂时不可用。", "检查基础平台数据库与授权投影队列后，在当前环境重新同步。"
	case "readiness":
		return "Keycloak 切换门禁状态保存暂时不可用。", "检查基础平台数据库与 Keycloak 就绪状态后，在当前环境重新同步。"
	default:
		return "Keycloak 同步暂时不可用。", "检查 Keycloak 管理连接后，在当前环境重新同步。"
	}
}

// SwitchToKeycloak 是显式的认证集成切换动作。目标 Issuer 由服务端固定下发，
// 浏览器无法把一次 Keycloak 切换请求变成任意的提供方变更。
func (handler *SubsystemOnboardingHandler) SwitchToKeycloak(writer stdhttp.ResponseWriter, request *stdhttp.Request) {
	handler.updateSubsystem(writer, request, "keycloak", "UPDATE")
}

// RollbackToPlatform 是显式的认证集成回滚动作。复用与旧版子系统更新相同的
// 受控部署工作流，同时把目标 Issuer 固定为 Basic Platform。
func (handler *SubsystemOnboardingHandler) RollbackToPlatform(writer stdhttp.ResponseWriter, request *stdhttp.Request) {
	handler.updateSubsystem(writer, request, "platform", "UPDATE")
}
