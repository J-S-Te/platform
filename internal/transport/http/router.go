// Package httptransport 负责装配 HTTP 路由和跨模块中间件，不在传输层复制业务规则。
package httptransport

import (
	"log/slog"
	"net/http"
	"strings"
	"time"

	applicationregistryapplication "github.com/J-S-Te/Basic-Platform/internal/platform/applicationregistry/application"
	applicationregistryhttp "github.com/J-S-Te/Basic-Platform/internal/platform/applicationregistry/interfaces/http"
	audithttp "github.com/J-S-Te/Basic-Platform/internal/platform/audit/interfaces/http"
	applicationaccess "github.com/J-S-Te/Basic-Platform/internal/platform/authorization/applicationaccess"
	authorizationhttp "github.com/J-S-Te/Basic-Platform/internal/platform/authorization/interfaces/http"
	positiongrant "github.com/J-S-Te/Basic-Platform/internal/platform/authorization/positiongrant"
	configurationhttp "github.com/J-S-Te/Basic-Platform/internal/platform/configuration/interfaces/http"
	credentialcrypto "github.com/J-S-Te/Basic-Platform/internal/platform/credentialcrypto"
	credentialhttp "github.com/J-S-Te/Basic-Platform/internal/platform/credentialcrypto/interfaces/http"
	dictionaryhttp "github.com/J-S-Te/Basic-Platform/internal/platform/dictionary/interfaces/http"
	externalidentityhttp "github.com/J-S-Te/Basic-Platform/internal/platform/externalidentity/interfaces/http"
	filetaskhttp "github.com/J-S-Te/Basic-Platform/internal/platform/filetask/interfaces/http"
	identityhttp "github.com/J-S-Te/Basic-Platform/internal/platform/identity/interfaces/http"
	licensehttp "github.com/J-S-Te/Basic-Platform/internal/platform/license/interfaces/http"
	notificationhttp "github.com/J-S-Te/Basic-Platform/internal/platform/notification/interfaces/http"
	oidchttp "github.com/J-S-Te/Basic-Platform/internal/platform/oidc/interfaces/http"
	ownerdirectoryhttp "github.com/J-S-Te/Basic-Platform/internal/platform/ownerdirectory/interfaces/http"
	securityhttp "github.com/J-S-Te/Basic-Platform/internal/platform/security/interfaces/http"
	settingsapplication "github.com/J-S-Te/Basic-Platform/internal/platform/settings/application"
	settingshttp "github.com/J-S-Te/Basic-Platform/internal/platform/settings/interfaces/http"
	tenantclonehttp "github.com/J-S-Te/Basic-Platform/internal/platform/tenantclone/interfaces/http"
	"github.com/J-S-Te/Basic-Platform/internal/shared/config"
	"github.com/J-S-Te/Basic-Platform/internal/shared/httperror"
	"github.com/J-S-Te/Basic-Platform/internal/shared/httpresponse"
	"github.com/J-S-Te/Basic-Platform/internal/transport/http/middleware"
	"github.com/gin-gonic/gin"
	"gorm.io/gorm"
)

// OperationalModules 聚合后续扩展能力，避免组合根继续增长位置参数，也允许路由测试整体省略
// 可选模块。字段为 nil 时只表示该能力未装配，不应注册一个会绕过依赖检查的空路由。
type OperationalModules struct {
	Licenses            *licensehttp.Handler
	RuntimeLicenses     *licensehttp.RuntimeHandler
	LoginTargets        *applicationregistryhttp.LoginTargetManagementHandler
	SubsystemOnboarding *applicationregistryhttp.SubsystemOnboardingHandler
	// SubsystemAccess 是六个子系统共用的统一接入向导（预览/提交/查询/恢复）。
	SubsystemAccess *applicationregistryhttp.SubsystemAccessHandler
	// KeycloakIntegration 有意与应用目录分离：只负责身份提供方同步与 Issuer 切换，
	// 不暴露也不创建浏览器可见的 OAuth client secret。
	KeycloakIntegration *applicationregistryhttp.KeycloakIntegrationHandler
	// BrokerHandler 与 DeploymentStatusHandler 把“读取/校验”两条边界显式化。
	// 迁移期二者可指向旧接入适配器，但路由不再依赖该具体类型。
	BrokerHandler            applicationregistryhttp.BrokerHTTPHandler
	DeploymentStatusHandler  applicationregistryhttp.DeploymentStatusHTTPHandler
	SubsystemServiceRoutes   *applicationregistryhttp.SubsystemServiceRouteHandler
	Notifications            *notificationhttp.Handler
	AsyncJobs                *filetaskhttp.Handler
	ExternalIdentity         *externalidentityhttp.Handler
	OwnerDirectory           *ownerdirectoryhttp.Handler
	BackchannelLogoutURI     *applicationregistryhttp.BackchannelLogoutURIHandler
	PersonnelChanges         *identityhttp.PersonnelChangeHandler
	AuthorizationOverview    *identityhttp.AuthorizationOverviewHandler
	TenantAuthorizationClone *tenantclonehttp.Handler
	// AccessApplier 是管理控制台“对外访问”应用动作的可选部署执行器；
	// 生产/远程部署保持 nil，不得在服务端隐式执行基础设施变更。
	AccessApplier        settingsapplication.AccessApplier
	CredentialEncryption *credentialhttp.Handler
}

// NewRouter 先建立请求标识、可信代理、日志、恢复、CORS、CSRF 与内容类型等统一边界，
// 再通过各领域公开 HTTP 适配器注册路由；具体仓储实现不能泄漏到传输层。
func NewRouter(
	cfg config.Config,
	logger *slog.Logger,
	database *gorm.DB,
	authHandler *identityhttp.Handler,
	bootstrapHandler *identityhttp.BootstrapHandler,
	managementHandler *identityhttp.ManagementHandler,
	accountLifecycleHandler *identityhttp.AccountLifecycleHandler,
	authorizationHandler *authorizationhttp.Handler,
	applicationAccessHandler *applicationaccess.Handler,
	positionGrantHandler *positiongrant.Handler,
	auditHandler *audithttp.Handler,
	configurationHandler *configurationhttp.Handler,
	settingsHandler *settingshttp.Handler,
	dictionaryHandler *dictionaryhttp.Handler,
	securityHandler *securityhttp.Handler,
	applicationTokenHandler *applicationregistryhttp.Handler,
	applicationManagementHandler *applicationregistryhttp.ManagementHandler,
	oauthClientManagementHandler applicationregistryhttp.OAuthClientHTTPHandler,
	applicationAuthenticator *applicationregistryapplication.Service,
	auditRecorder middleware.AuditRecorder,
	oidcHandler *oidchttp.Handler,
	operational OperationalModules,
) *gin.Engine {
	// 先创建裸 Gin engine，再按“公共端点 → 协议端点 → 会话端点 → 业务端点”的顺序装配。
	// 这种顺序让每个路由组的认证边界清晰，也避免某个可选模块缺失时意外暴露半成品接口。
	router := gin.New()
	operationalMetrics := NewOperationalMetrics(database)
	credentialJSON := func(purpose, routeKey string, next http.HandlerFunc) http.HandlerFunc {
		if operational.CredentialEncryption != nil && operational.CredentialEncryption.Enabled() {
			return operational.CredentialEncryption.WrapJSON(purpose, routeKey, next)
		}
		return next
	}
	credentialResourceJSON := func(purpose, routeKey string, next http.HandlerFunc) http.HandlerFunc {
		if operational.CredentialEncryption != nil && operational.CredentialEncryption.Enabled() {
			return operational.CredentialEncryption.WrapJSONResource(purpose, routeKey, nil, next)
		}
		return next
	}
	credentialBatch := func(purpose, routeKey string, next http.HandlerFunc) http.HandlerFunc {
		if operational.CredentialEncryption != nil && operational.CredentialEncryption.Enabled() {
			return operational.CredentialEncryption.WrapJSONOrMultipart(purpose, routeKey, next)
		}
		return next
	}
	router.HandleMethodNotAllowed = true
	if err := router.SetTrustedProxies(cfg.HTTP.TrustedProxies); err != nil {
		panic("invalid trusted proxy configuration: " + err.Error())
	}
	allowedBrowserOrigins := append([]string(nil), cfg.CORSOrigins...)
	allowedBrowserOrigins = append(allowedBrowserOrigins, cfg.Auth.OIDCIssuer)

	router.Use(
		middleware.RequestID(),
		middleware.TraceCorrelation(),
		middleware.ClientIP(cfg.HTTP.TrustedProxies),
		operationalMetrics.Middleware(),
		middleware.Recover(logger),
		middleware.AccessLog(logger),
		middleware.SecurityHeaders(),
		middleware.CORS(cfg.CORSOrigins),
	)
	healthHandler := NewHealthHandler(database, cfg.AppName)
	router.GET("/healthz", healthHandler.Liveness)
	router.GET("/readyz", healthHandler.Readiness)
	// 安全审查 SEC-B4：/metrics 暴露队列积压、授权失败计数与连接池状态——
	// 未认证者不得读取，廉价抓取也不能再放大 DB 负载（计数另有10s TTL 缓存）。
	router.GET("/metrics", middleware.RequireMetricsAccess(cfg.HTTP.MetricsAccessToken), gin.WrapH(operationalMetrics))

	// 完整 OIDC handler 提供浏览器授权和标准协议端点；在迁移期只有机器令牌 handler 时，
	// 仅保留 token 发行接口，避免把不完整的 OIDC 端点注册出来。
	if oidcHandler != nil {
		oauthRateLimit := middleware.FixedWindowRateLimit(60, time.Minute)
		router.GET("/authorize", adaptHandler(oidcHandler.Authorize))
		router.POST("/oauth2/token", oauthRateLimit, adaptHandler(oidcHandler.Token))
		router.POST("/oauth2/par", oauthRateLimit, adaptHandler(oidcHandler.PAR))
		router.POST("/oauth2/revoke", oauthRateLimit, adaptHandler(oidcHandler.Revoke))
		router.GET("/oauth2/consent", adaptHandler(oidcHandler.Consent))
		consentRouter := router.Group("/oauth2/consent")
		consentRouter.Use(middleware.RequireSameOrigin(cfg.Auth.OIDCIssuer))
		consentRouter.POST("/grant", adaptHandler(oidcHandler.GrantConsent))
		consentRouter.DELETE("", adaptHandler(oidcHandler.RevokeConsent))
		router.GET("/.well-known/openid-configuration", adaptHandler(oidcHandler.Discovery))
		router.GET("/oauth2/jwks", adaptHandler(oidcHandler.JWKS))
		router.GET("/oauth2/userinfo", adaptHandler(oidcHandler.UserInfo))
		router.POST("/oauth2/userinfo", adaptHandler(oidcHandler.UserInfo))
		router.GET("/oauth2/authorization-context", adaptHandler(oidcHandler.AuthorizationContext))
		// 安全审查 SEC-B10：Lax Cookie 在顶层 GET 也会被发送——GET 登出可被跨站顶层导航
		// 强制触发（骚扰/反取证降级），必须校验导航来源；POST 维持严格 Origin 校验。
		router.GET("/oauth2/logout", middleware.RequireSameOriginNavigation(cfg.Auth.OIDCIssuer), adaptHandler(oidcHandler.Logout))
		router.POST("/oauth2/logout", middleware.RequireSameOrigin(cfg.Auth.OIDCIssuer), adaptHandler(oidcHandler.Logout))
	} else if applicationTokenHandler != nil {
		// 尚未配置完整 OIDC 依赖图的部署保留既有机器对机器令牌端点，
		// 使存量子系统接入不因迁移中断。
		router.POST("/oauth2/token", middleware.FixedWindowRateLimit(60, time.Minute), adaptHandler(applicationTokenHandler.IssueToken))
	}

	if bootstrapHandler != nil {
		bootstrapRouter := router.Group("/api/v1/iam/bootstrap")
		bootstrapRouter.Use(middleware.RequireSafeWriteContentType())
		bootstrapRouteHandlers := []gin.HandlerFunc{adaptHandler(bootstrapHandler.InitializeFirstSuperAdmin)}
		if strings.TrimSpace(cfg.Identity.BootstrapToken) != "" {
			bootstrapRouteHandlers = append([]gin.HandlerFunc{middleware.FixedWindowRateLimit(5, time.Minute)}, bootstrapRouteHandlers...)
		}
		bootstrapRouter.POST("/first-super-admin", bootstrapRouteHandlers...)
	}

	if authHandler != nil {
		// /api/v1/auth 是登录会话边界；登录接口只做来源/写入校验，登录后的接口再叠加
		// Authentication 中间件。业务 API 使用下面独立的 apiRouter，避免认证策略混用。
		authRouter := router.Group("/api/v1/auth")
		authRouter.Use(middleware.RequireAllowedOriginForUnsafeMethods(allowedBrowserOrigins...), middleware.RequireSafeWriteContentType())
		// 安全审查 SEC-B7：登录改双桶限流——账号维度30/min先限（换 IP 不豁免）、
		// IP 维度120/min兜底（随机换号撞库有界），消灭原“全客户端共用一个30/min IP 桶、
		// 单攻击者可触发全员锁定”的问题。多副本与反代配置要求见 platform/docs/rate-limiting.md。
		if operational.CredentialEncryption != nil && operational.CredentialEncryption.Enabled() {
			authRouter.POST("/credential-challenges", adaptHandler(operational.CredentialEncryption.IssueChallenge))
		}
		loginHandler := authHandler.Login
		if operational.CredentialEncryption != nil && operational.CredentialEncryption.Enabled() {
			loginHandler = operational.CredentialEncryption.WrapJSON(credentialcrypto.PurposeLogin, "auth.login", loginHandler)
		}
		authRouter.POST("/login", middleware.LoginRateLimit("account", 30, 120, time.Minute), adaptHandler(loginHandler))
		// 安全审查 AUD-2026-010：GET /login 与 GET /oidc/callback 每次都会触发对身份源的
		// 出站协议动作（授权重定向 / token 交换 + userinfo + DB 查询），复用 /oauth2/token 的
		// IP 固定窗口限流（60/min）约束出站放大面。与 protocolRouter 共享同一限流器实例，
		// 使登录跳转与回调共用一个 IP 配额。
		authFlowRateLimit := middleware.FixedWindowRateLimit(60, time.Minute)
		authRouter.GET("/login", authFlowRateLimit, adaptHandler(authHandler.BeginOIDCLogin))
		authRouter.GET("/oidc/callback", authFlowRateLimit, adaptHandler(authHandler.OIDCCallback))

		protected := authRouter.Group("")
		protected.Use(middleware.Authentication(authHandler, authHandler.CookieName()))
		protected.POST("/token/refresh", adaptHandler(authHandler.Refresh))
		protected.POST("/activity", adaptHandler(authHandler.Activity))
		protected.POST("/logout", adaptHandler(authHandler.Logout))
		protected.GET("/me", adaptHandler(authHandler.Me))
	}

	// 有意只建这一条 Keycloak JWT 路由边界。不得并入控制台会话路由组：
	// 在别处接受 Keycloak bearer 会悄然扩大平台认证面。
	if operational.SubsystemOnboarding != nil && cfg.Keycloak.Enabled {
		realmPath := "/realms/" + strings.TrimSpace(cfg.Keycloak.Realm)
		issuer := strings.TrimRight(cfg.Keycloak.PublicURL, "/") + realmPath
		backchannelIssuer := strings.TrimRight(cfg.Keycloak.AdminURL, "/") + realmPath
		verifier, err := middleware.NewKeycloakBrokerJWTVerifierWithBackchannel(issuer, backchannelIssuer, nil)
		if err != nil {
			panic("invalid Keycloak broker JWT verifier configuration: " + err.Error())
		}
		brokerVerificationRouter := router.Group("/api/v1/keycloak")
		brokerVerificationRouter.Use(middleware.RequireSafeWriteContentType(), middleware.KeycloakBrokerAuthentication(verifier))
		brokerHandler := operational.BrokerHandler
		if brokerHandler == nil {
			brokerHandler = operational.SubsystemOnboarding
		}
		brokerVerificationRouter.POST("/broker-login-verifications", adaptHandler(brokerHandler.VerifyKeycloakBrokerLogin))
		// 新端点是 Keycloak 认证集成边界。上方 /api/v1/keycloak/... 仅为兼容既有
		// broker 回调客户端而保留，新调用方一律使用这一显式命名空间。
		if operational.KeycloakIntegration != nil {
			integrationBrokerRouter := router.Group("/api/v1/keycloak-integration")
			integrationBrokerRouter.Use(middleware.RequireSafeWriteContentType(), middleware.KeycloakBrokerAuthentication(verifier))
			integrationBrokerRouter.POST("/broker-login-verifications", adaptHandler(operational.KeycloakIntegration.VerifyBrokerLogin))
		}
	}

	if authHandler != nil {
		apiRouter := router.Group("/api/v1")
		// 所有管理 API 共享来源、写入内容类型、会话认证和可选审计链；具体权限由各路由
		// 在注册点声明，便于审查“哪个接口需要什么能力”。
		apiRouter.Use(middleware.RequireAllowedOriginForUnsafeMethods(allowedBrowserOrigins...), middleware.RequireSafeWriteContentType())
		apiRouter.Use(middleware.Authentication(authHandler, authHandler.CookieName()))
		if auditRecorder != nil {
			apiRouter.Use(middleware.AuditTrail(auditRecorder, logger, middleware.AuditSource{ApplicationCode: cfg.Audit.ApplicationCode, EnvironmentCode: cfg.Audit.EnvironmentCode}))
		}
		if operational.RuntimeLicenses != nil {
			apiRouter.GET("/licenses/installation-evidence", middleware.RequirePermission("platform:license:read"), adaptHandler(operational.RuntimeLicenses.InstallationEvidence))
			apiRouter.GET("/licenses/enforcement/:application", middleware.RequirePermission("platform:license:read"), adaptHandler(operational.RuntimeLicenses.Status))
			apiRouter.GET("/licenses/enforcement/:application/evidence", middleware.RequirePermission("platform:license:read"), adaptHandler(operational.RuntimeLicenses.Evidence))
			apiRouter.POST("/licenses/enforcement/:application/activation", middleware.RequirePermission("platform:license:manage"), adaptHandler(operational.RuntimeLicenses.BeginActivation))
		}
		if operational.Licenses != nil {
			licenses := apiRouter.Group("/licenses")
			licenses.GET("/status", middleware.RequirePermission("platform:license:read"), adaptHandler(operational.Licenses.Status))
			licenses.GET("/artifact", middleware.RequirePermission("platform:license:read"), adaptHandler(operational.Licenses.Artifact))
			licenses.GET("/request", middleware.RequirePermission("platform:license:read"), adaptHandler(operational.Licenses.Request))
			licenses.GET("/events", middleware.RequirePermission("platform:license:read"), adaptHandler(operational.Licenses.Events))
			licenses.POST("/initialize", middleware.RequirePermission("platform:license:manage"), adaptHandler(operational.Licenses.Initialize))
			licenses.POST("/imports/preview", middleware.RequirePermission("platform:license:manage"), adaptHandler(operational.Licenses.Preview))
			licenses.POST("/imports/commit", middleware.RequirePermission("platform:license:manage"), adaptHandler(operational.Licenses.Commit))
			licenses.POST("/clock-recoveries", middleware.RequirePermission("platform:license:manage"), adaptHandler(operational.Licenses.RestoreClock))
		}

		if operational.AuthorizationOverview != nil {
			apiRouter.GET("/people/:user_id/authorization-overview", middleware.RequirePermission("platform:user:read"), adaptHandler(operational.AuthorizationOverview.Get))
		}

		if operational.PersonnelChanges != nil {
			apiRouter.GET("/personnel-changes", middleware.RequirePermission("platform:user:read"), adaptHandler(operational.PersonnelChanges.List))
			apiRouter.POST("/personnel-changes", middleware.RequirePermission("platform:user:update"), adaptHandler(operational.PersonnelChanges.Create))
			apiRouter.POST("/personnel-changes/preview", middleware.RequirePermission("platform:user:read"), adaptHandler(operational.PersonnelChanges.PreviewDraft))
			apiRouter.POST("/personnel-changes/:change_id/transition", middleware.RequirePermission("platform:user:update"), middleware.RequirePermission("platform:approval:process"), adaptHandler(operational.PersonnelChanges.Transition))
			apiRouter.POST("/personnel-changes/:change_id/submit", middleware.RequirePermission("platform:user:update"), adaptHandler(operational.PersonnelChanges.Submit))
			apiRouter.POST("/personnel-changes/:change_id/cancel", middleware.RequirePermission("platform:user:update"), adaptHandler(operational.PersonnelChanges.Cancel))
			apiRouter.GET("/personnel-changes/:change_id/preview", middleware.RequirePermission("platform:user:read"), adaptHandler(operational.PersonnelChanges.Preview))
			apiRouter.GET("/personnel-changes/:change_id/handover-items", middleware.RequirePermission("platform:user:read"), adaptHandler(operational.PersonnelChanges.ListHandoverItems))
			apiRouter.POST("/personnel-changes/:change_id/handover-items/:item_id/complete", middleware.RequirePermission("platform:user:update"), middleware.RequirePermission("platform:approval:process"), adaptHandler(operational.PersonnelChanges.CompleteHandoverItem))
			apiRouter.POST("/personnel-changes/:change_id/handover/retry", middleware.RequirePermission("platform:user:update"), middleware.RequirePermission("platform:approval:process"), adaptHandler(operational.PersonnelChanges.RetryHandover))
		}

		if managementHandler != nil {
			apiRouter.GET("/users", middleware.RequirePermission("platform:user:read"), adaptHandler(managementHandler.ListUsers))
			apiRouter.POST("/users", middleware.RequirePermission("platform:user:create"), adaptHandler(managementHandler.CreateUser))
			apiRouter.POST("/employees", middleware.RequirePermission("platform:user:create"), adaptHandler(credentialJSON(credentialcrypto.PurposeCreateEmployee, "employee.create", managementHandler.CreateEmployee)))
			apiRouter.POST("/employees/batch", middleware.RequirePermission("platform:user:create"), adaptHandler(credentialBatch(credentialcrypto.PurposeCreateEmployees, "employee.batch", managementHandler.CreateEmployeesBatch)))
			apiRouter.POST("/users/batch", middleware.RequirePermission("platform:user:create"), adaptHandler(managementHandler.CreateUsersBatch))
			apiRouter.GET("/users/:user_id", middleware.RequirePermission("platform:user:read"), adaptHandler(managementHandler.GetUser))
			apiRouter.PATCH("/users/:user_id", middleware.RequirePermission("platform:user:update"), adaptHandler(managementHandler.UpdateUser))
			apiRouter.DELETE("/users/:user_id", middleware.RequirePermission("platform:user:delete"), adaptHandler(managementHandler.DeleteUser))
			apiRouter.GET("/accounts", middleware.RequirePermission("platform:account:read"), adaptHandler(managementHandler.ListAccounts))
			apiRouter.PATCH("/accounts/:account_id", middleware.RequirePermission("platform:account:update"), adaptHandler(managementHandler.UpdateAccount))
			// 安全审查 SEC-B8：组织/岗位/任职10条路由在注册点显式声明路由级权限
			// （与相邻路由同风格、与 handler 内 requireManagementScope 同一权限码，
			// 属纵深防御——未来重构 handler 不会静默丢失授权）。
			apiRouter.GET("/org-units", middleware.RequirePermission("platform:organization:read"), adaptHandler(managementHandler.ListOrgUnits))
			apiRouter.POST("/org-units", middleware.RequirePermission("platform:organization:create"), adaptHandler(managementHandler.CreateOrgUnit))
			apiRouter.PATCH("/org-units/:org_unit_id", middleware.RequirePermission("platform:organization:update"), adaptHandler(managementHandler.UpdateOrgUnit))
			apiRouter.DELETE("/org-units/:org_unit_id", middleware.RequirePermission("platform:organization:delete"), adaptHandler(managementHandler.DeleteOrgUnit))
			apiRouter.GET("/positions", middleware.RequirePermission("platform:position:read"), adaptHandler(managementHandler.ListPositions))
			apiRouter.POST("/positions", middleware.RequirePermission("platform:position:create"), adaptHandler(managementHandler.CreatePosition))
			apiRouter.DELETE("/positions/:position_id", middleware.RequirePermission("platform:position:delete"), adaptHandler(managementHandler.DeletePosition))
			apiRouter.GET("/memberships", middleware.RequirePermission("platform:membership:read"), adaptHandler(managementHandler.ListMemberships))
			apiRouter.POST("/memberships", middleware.RequirePermission("platform:membership:create"), adaptHandler(managementHandler.CreateMembership))
			apiRouter.PATCH("/memberships/:membership_id", middleware.RequirePermission("platform:membership:update"), adaptHandler(managementHandler.UpdateMembership))
		}

		if applicationManagementHandler != nil {
			apiRouter.GET("/applications", middleware.RequirePermission("platform:application:read"), adaptHandler(applicationManagementHandler.ListApplications))
			apiRouter.POST("/applications", middleware.RequirePermission("platform:application:create"), adaptHandler(applicationManagementHandler.CreateApplication))
			apiRouter.GET("/applications/:application_id", middleware.RequirePermission("platform:application:read"), adaptHandler(applicationManagementHandler.GetApplication))
			apiRouter.PATCH("/applications/:application_id", middleware.RequirePermission("platform:application:update"), adaptHandler(applicationManagementHandler.UpdateApplication))
			apiRouter.DELETE("/applications/:application_id", middleware.RequirePermission("platform:application:update"), adaptHandler(applicationManagementHandler.DeleteApplication))
			apiRouter.GET("/applications/:application_id/environments", middleware.RequirePermission("platform:application-environment:read"), adaptHandler(applicationManagementHandler.ListEnvironments))
			apiRouter.POST("/applications/:application_id/environments", middleware.RequirePermission("platform:application-environment:create"), adaptHandler(applicationManagementHandler.CreateEnvironment))
			apiRouter.PATCH("/applications/:application_id/environments/:environment_id", middleware.RequirePermission("platform:application-environment:update"), adaptHandler(applicationManagementHandler.UpdateEnvironment))
			apiRouter.DELETE("/applications/:application_id/environments/:environment_id", middleware.RequirePermission("platform:application-environment:delete"), adaptHandler(applicationManagementHandler.DeleteEnvironment))
			apiRouter.POST("/applications/:application_id/environments/:environment_id/purge", middleware.RequirePermission("platform:application-environment:delete"), adaptHandler(applicationManagementHandler.PurgeEnvironment))
		}

		if operational.TenantAuthorizationClone != nil {
			// 克隆会创建完整静态授权目录，调用方必须同时具备应用和角色创建权限；
			// Handler 只允许以当前租户为模板，不能从请求中选择任意源租户。
			apiRouter.POST("/tenants/:tenant_id/authorization-catalog-clone",
				middleware.RequirePermission("platform:application:create"),
				middleware.RequirePermission("platform:role:create"),
				adaptHandler(operational.TenantAuthorizationClone.CloneAuthorizationCatalog),
			)
		}

		if operational.SubsystemAccess != nil {
			// 统一入口：路由级只要求只读权限，动作级权限（首次接入/接管/更新/恢复）由处理器按服务端判定的动作检查。
			apiRouter.POST("/subsystem-access/preview", middleware.RequirePermission("platform:application:read"), adaptHandler(operational.SubsystemAccess.Preview))
			apiRouter.POST("/subsystem-access/operations", middleware.RequirePermission("platform:application:read"), adaptHandler(operational.SubsystemAccess.Submit))
			apiRouter.GET("/subsystem-access/operations", middleware.RequirePermission("platform:application:read"), adaptHandler(operational.SubsystemAccess.FindByTarget))
			apiRouter.GET("/subsystem-access/operations/:operation_id", middleware.RequirePermission("platform:application:read"), adaptHandler(operational.SubsystemAccess.Get))
			apiRouter.POST("/subsystem-access/operations/:operation_id/resume", middleware.RequirePermission("platform:application:read"), adaptHandler(operational.SubsystemAccess.Resume))
		}
		if operational.SubsystemOnboarding != nil {
			apiRouter.GET("/portal/applications", adaptHandler(operational.SubsystemOnboarding.ListPortalApplications))
			apiRouter.GET("/subsystem-capabilities", middleware.RequirePermission("platform:application:read"), adaptHandler(operational.SubsystemOnboarding.GetSubsystemCapabilities))
			apiRouter.GET("/subsystem-discovery", middleware.RequirePermission("platform:application:read"), adaptHandler(operational.SubsystemOnboarding.DiscoverSubsystemCandidates))
			deploymentStatusHandler := operational.DeploymentStatusHandler
			if deploymentStatusHandler == nil {
				deploymentStatusHandler = operational.SubsystemOnboarding
			}
			apiRouter.GET("/subsystem-status", middleware.RequirePermission("platform:application:read"), adaptHandler(deploymentStatusHandler.GetSubsystemStatus))
			apiRouter.POST("/subsystem-adoption",
				middleware.RequirePermission("platform:application:update"),
				middleware.RequirePermission("platform:application-environment:update"),
				middleware.RequirePermission("platform:application-login-target:update"),
				middleware.RequirePermission("platform:oauth-client:disable"),
				adaptHandler(operational.SubsystemOnboarding.AdoptSubsystem),
			)
			// V2 目录注册有意止步于任何 OIDC Client、凭据或部署变更之前：
			// client 生命周期由专用 keycloak-integration 端点管理。
			apiRouter.POST("/subsystem-directory",
				middleware.RequirePermission("platform:application:create"),
				middleware.RequirePermission("platform:application-environment:create"),
				middleware.RequirePermission("platform:application-login-target:create"),
				adaptHandler(operational.SubsystemOnboarding.RegisterSubsystemDirectory),
			)
			apiRouter.POST("/subsystem-onboarding",
				middleware.RequirePermission("platform:application:create"),
				middleware.RequirePermission("platform:application-environment:create"),
				middleware.RequirePermission("platform:application-login-target:create"),
				middleware.RequirePermission("platform:oauth-client:create"),
				middleware.RequirePermission("platform:role-binding:update"),
				adaptHandler(operational.SubsystemOnboarding.OnboardSubsystem),
			)
			// 重新落地已接入的子系统：重写 .env.local、重建容器、刷新门户网关 include。
			// 若 BaseURL/UpstreamURL/PathPrefix 变化，调用方应已通过 PATCH /environments
			// 与 /oauth-clients 更新数据。选用 oauth-client:disable（而非 :update）是因为
			// 权限目录对该资源只有细粒度更新权限（scope/redirect-uri/jwk），没有通用
			// :update；重新供给等同于重发运行中的 OAuth client 绑定，:disable 是最接近的现存权限。
			apiRouter.POST("/subsystem-update",
				middleware.RequirePermission("platform:application:update"),
				middleware.RequirePermission("platform:application-environment:update"),
				middleware.RequirePermission("platform:application-login-target:update"),
				middleware.RequirePermission("platform:oauth-client:disable"),
				adaptHandler(operational.SubsystemOnboarding.UpdateSubsystem),
			)
			apiRouter.POST("/subsystem-keycloak/sync",
				middleware.RequirePermission("platform:application:update"),
				middleware.RequirePermission("platform:application-environment:update"),
				adaptHandler(operational.SubsystemOnboarding.SyncKeycloakClient),
			)
			// 重试与 update 走同一条安全重放路径，但在持久化部署状态中记录 RETRY，
			// 让运维能区分一次故障恢复与例行的重新落地。
			apiRouter.POST("/subsystem-retry",
				middleware.RequirePermission("platform:application:update"),
				middleware.RequirePermission("platform:application-environment:update"),
				middleware.RequirePermission("platform:application-login-target:update"),
				middleware.RequirePermission("platform:oauth-client:disable"),
				middleware.RequirePermission("platform:role-binding:update"),
				adaptHandler(operational.SubsystemOnboarding.UpdateSubsystem),
			)
			// 拆除已接入子系统：停容器、删 .env.local、移除门户网关 include 并重载 nginx。
			// HTTP 层不删数据库行：脚本应随后调用 DELETE /environments（必要时再调
			// DELETE /applications），使审计线索按步骤完整保留。选用生命周期更新权限
			// 而非 :delete，因为本端点变更的是基础设施状态（容器/文件/网关）而非数据库行；
			// 数据库删除由常规 DELETE /environments 端点自身的 :delete 权限把关。
			apiRouter.POST("/subsystem-teardown",
				middleware.RequirePermission("platform:application:update"),
				middleware.RequirePermission("platform:application-environment:update"),
				middleware.RequirePermission("platform:application-login-target:update"),
				middleware.RequirePermission("platform:oauth-client:disable"),
				adaptHandler(operational.SubsystemOnboarding.TeardownSubsystem),
			)
			// 丢弃卡在 PROVISION_FAILED 的部署：尽力清理基础设施并删除持久化生命周期记录，
			// 使 code+environment 组合可重新接入。需随后调用 DELETE /applications 与
			// /environments 释放路径前缀。
			apiRouter.POST("/subsystem-deployment-discard",
				middleware.RequirePermission("platform:application:update"),
				middleware.RequirePermission("platform:application-environment:update"),
				middleware.RequirePermission("platform:application-login-target:update"),
				middleware.RequirePermission("platform:oauth-client:disable"),
				adaptHandler(operational.SubsystemOnboarding.DiscardFailedSubsystemDeployment),
			)
		}
		if operational.KeycloakIntegration != nil {
			// 认证提供方操作使用独立命名空间：应用目录继续只负责应用/环境数据、
			// 角色目录与最终授权分配，不再承担身份提供方职责。
			keycloakIntegrationRouter := apiRouter.Group("/keycloak-integration")
			keycloakIntegrationRouter.GET("/capabilities", middleware.RequirePermission("platform:application:read"), adaptHandler(operational.KeycloakIntegration.Capabilities))
			keycloakIntegrationRouter.GET("/status", middleware.RequirePermission("platform:application:read"), adaptHandler(operational.KeycloakIntegration.Status))
			keycloakIntegrationRouter.GET("/health-dashboard", middleware.RequirePermission("platform:application:read"), adaptHandler(operational.KeycloakIntegration.HealthDashboard))
			keycloakIntegrationRouter.GET("/sync-status", middleware.RequirePermission("platform:application:read"), adaptHandler(operational.KeycloakIntegration.SyncStatus))
			// FAILED 投影是切换期高危阻塞点。读取遵循应用可见性；重放额外要求
			// role-binding 更新权限，因为它会把平台授权事实回写进 Keycloak Client。
			keycloakIntegrationRouter.GET("/projection-failures", middleware.RequirePermission("platform:application:read"), adaptHandler(operational.KeycloakIntegration.ListProjectionFailures))
			keycloakIntegrationRouter.GET("/projection-alerts", middleware.RequirePermission("platform:application:read"), adaptHandler(operational.KeycloakIntegration.ProjectionAlerts))
			keycloakIntegrationRouter.POST("/projection-failures/:event_id/replay", middleware.RequirePermission("platform:application:update"), middleware.RequirePermission("platform:application-environment:update"), middleware.RequirePermission("platform:role-binding:update"), adaptHandler(operational.KeycloakIntegration.ReplayProjectionFailure))
			keycloakIntegrationRouter.POST("/observation", middleware.RequirePermission("platform:application:update"), middleware.RequirePermission("platform:application-environment:update"), adaptHandler(operational.KeycloakIntegration.StartObservation))
			keycloakIntegrationRouter.POST("/sync", middleware.RequirePermission("platform:application:update"), middleware.RequirePermission("platform:application-environment:update"), adaptHandler(operational.KeycloakIntegration.SyncClient))
			keycloakIntegrationRouter.POST("/switch", middleware.RequirePermission("platform:application:update"), middleware.RequirePermission("platform:application-environment:update"), middleware.RequirePermission("platform:application-login-target:update"), middleware.RequirePermission("platform:oauth-client:disable"), adaptHandler(operational.KeycloakIntegration.Switch))
			keycloakIntegrationRouter.POST("/rollback", middleware.RequirePermission("platform:application:update"), middleware.RequirePermission("platform:application-environment:update"), middleware.RequirePermission("platform:application-login-target:update"), middleware.RequirePermission("platform:oauth-client:disable"), adaptHandler(operational.KeycloakIntegration.Rollback))
		}
		if operational.SubsystemServiceRoutes != nil {
			apiRouter.GET("/subsystem-service-route", middleware.RequirePermission("platform:application:read"), adaptHandler(operational.SubsystemServiceRoutes.Resolve))
			apiRouter.Any("/subsystems/:application_code/*path", middleware.RequirePermission("platform:application:read"), adaptHandler(operational.SubsystemServiceRoutes.Proxy))
		}

		if operational.LoginTargets != nil {
			apiRouter.GET("/applications/:application_id/environments/:environment_id/login-targets", middleware.RequirePermission("platform:application-login-target:read"), adaptHandler(operational.LoginTargets.ListLoginTargets))
			apiRouter.POST("/applications/:application_id/environments/:environment_id/login-targets", middleware.RequirePermission("platform:application-login-target:create"), adaptHandler(operational.LoginTargets.CreateLoginTarget))
			apiRouter.GET("/applications/:application_id/environments/:environment_id/login-targets/:login_target_id", middleware.RequirePermission("platform:application-login-target:read"), adaptHandler(operational.LoginTargets.GetLoginTarget))
			apiRouter.PATCH("/applications/:application_id/environments/:environment_id/login-targets/:login_target_id", middleware.RequirePermission("platform:application-login-target:update"), adaptHandler(operational.LoginTargets.UpdateLoginTarget))
		}

		if oauthClientManagementHandler != nil {
			apiRouter.GET("/oauth-clients", middleware.RequirePermission("platform:oauth-client:read"), adaptHandler(oauthClientManagementHandler.ListOAuthClients))
			apiRouter.POST("/oauth-clients", middleware.RequirePermission("platform:oauth-client:create"), adaptHandler(oauthClientManagementHandler.CreateOAuthClient))
			apiRouter.GET("/oauth-clients/:oauth_client_id", middleware.RequirePermission("platform:oauth-client:read"), adaptHandler(oauthClientManagementHandler.GetOAuthClient))
			apiRouter.PUT("/oauth-clients/:oauth_client_id/scopes", middleware.RequirePermission("platform:oauth-client:scope-update"), adaptHandler(oauthClientManagementHandler.UpdateOAuthClientScopes))
			apiRouter.PUT("/oauth-clients/:oauth_client_id/redirect-uris", middleware.RequirePermission("platform:oauth-client:redirect-uri-update"), adaptHandler(oauthClientManagementHandler.UpdateOAuthClientRedirectURIs))
			if operational.BackchannelLogoutURI != nil {
				apiRouter.GET("/oauth-clients/:oauth_client_id/backchannel-logout-uri", middleware.RequirePermission("platform:oauth-client:read"), adaptHandler(operational.BackchannelLogoutURI.Handle))
				apiRouter.PUT("/oauth-clients/:oauth_client_id/backchannel-logout-uri", middleware.RequirePermission("platform:oauth-client:redirect-uri-update"), adaptHandler(operational.BackchannelLogoutURI.Handle))
				apiRouter.DELETE("/oauth-clients/:oauth_client_id/backchannel-logout-uri", middleware.RequirePermission("platform:oauth-client:redirect-uri-update"), adaptHandler(operational.BackchannelLogoutURI.Handle))
			}
			apiRouter.GET("/oauth-clients/:oauth_client_id/post-logout-redirect-uris", middleware.RequirePermission("platform:oauth-client:read"), adaptHandler(oauthClientManagementHandler.GetOAuthClientPostLogoutRedirectURIs))
			apiRouter.PUT("/oauth-clients/:oauth_client_id/post-logout-redirect-uris", middleware.RequirePermission("platform:oauth-client:post-logout-redirect-uri-update"), adaptHandler(oauthClientManagementHandler.UpdateOAuthClientPostLogoutRedirectURIs))
			apiRouter.GET("/oauth-clients/:oauth_client_id/jwks", middleware.RequirePermission("platform:oauth-client:read"), adaptHandler(oauthClientManagementHandler.GetOAuthClientJWKs))
			apiRouter.PUT("/oauth-clients/:oauth_client_id/jwks", middleware.RequirePermission("platform:oauth-client:jwk-update"), adaptHandler(oauthClientManagementHandler.UpdateOAuthClientJWKs))
			apiRouter.POST("/oauth-clients/:oauth_client_id/disable", middleware.RequirePermission("platform:oauth-client:disable"), adaptHandler(oauthClientManagementHandler.DisableOAuthClient))
			apiRouter.POST("/oauth-clients/:oauth_client_id/credentials", middleware.RequirePermission("platform:oauth-client-credential:create"), adaptHandler(oauthClientManagementHandler.CreateCredential))
			apiRouter.POST("/oauth-clients/:oauth_client_id/credentials/rotate", middleware.RequirePermission("platform:oauth-client-credential:rotate"), adaptHandler(oauthClientManagementHandler.RotateCredential))
			apiRouter.POST("/oauth-clients/:oauth_client_id/credentials/:credential_id/disable", middleware.RequirePermission("platform:oauth-client-credential:disable"), adaptHandler(oauthClientManagementHandler.DisableCredential))
		}

		if accountLifecycleHandler != nil {
			apiRouter.POST("/accounts", middleware.RequirePermission("platform:account:create"), adaptHandler(credentialJSON(credentialcrypto.PurposeCreateAccount, "account.create", accountLifecycleHandler.CreateLocalAccount)))
			apiRouter.POST("/accounts/:account_id/password/initialize", middleware.RequirePermission("platform:account:password-initialize"), adaptHandler(credentialResourceJSON(credentialcrypto.PurposeInitialize, "account.password.initialize", accountLifecycleHandler.InitializePassword)))
			apiRouter.POST("/accounts/:account_id/password/reset", middleware.RequirePermission("platform:account:password-reset"), adaptHandler(credentialResourceJSON(credentialcrypto.PurposeReset, "account.password.reset", accountLifecycleHandler.ResetPassword)))
			apiRouter.POST("/auth/password/change", adaptHandler(credentialJSON(credentialcrypto.PurposeChangePassword, "auth.password.change", accountLifecycleHandler.ChangeOwnPassword)))
		}

		if applicationAccessHandler != nil {
			apiRouter.GET("/users/:user_id/applications/:application_code/access", middleware.RequirePermission("platform:role-binding:read"), adaptHandler(applicationAccessHandler.GetAccess))
			apiRouter.PUT("/users/:user_id/applications/:application_code/access", middleware.RequirePermission("platform:role-binding:update"), adaptHandler(applicationAccessHandler.UpdateAccess))
			apiRouter.DELETE("/users/:user_id/applications/:application_code/access", middleware.RequirePermission("platform:role-binding:update"), adaptHandler(applicationAccessHandler.DeleteAccess))
			apiRouter.GET("/authorization-subjects/:subject_type/:subject_id/applications/:application_code/access", middleware.RequirePermission("platform:role-binding:read"), adaptHandler(applicationAccessHandler.GetSubjectAccess))
			apiRouter.PUT("/authorization-subjects/:subject_type/:subject_id/applications/:application_code/access", middleware.RequirePermission("platform:role-binding:update"), adaptHandler(applicationAccessHandler.UpdateSubjectAccess))
			apiRouter.DELETE("/authorization-subjects/:subject_type/:subject_id/applications/:application_code/access", middleware.RequirePermission("platform:role-binding:update"), adaptHandler(applicationAccessHandler.DeleteSubjectAccess))
		}

		if positionGrantHandler != nil {
			apiRouter.GET("/position-authorization-targets", middleware.RequirePermission("platform:role-binding:read"), adaptHandler(positionGrantHandler.ListAuthorizationTargets))
			apiRouter.GET("/position-authorization-positions", middleware.RequirePermission("platform:role-binding:read"), adaptHandler(positionGrantHandler.ListAuthorizationPositions))
			apiRouter.GET("/position-authorization-templates", middleware.RequirePermission("platform:role-binding:read"), adaptHandler(positionGrantHandler.List))
			apiRouter.POST("/position-authorization-templates", middleware.RequirePermission("platform:role-binding:update"), adaptHandler(positionGrantHandler.Create))
			apiRouter.GET("/position-authorization-templates/:template_id", middleware.RequirePermission("platform:role-binding:read"), adaptHandler(positionGrantHandler.Get))
			apiRouter.PATCH("/position-authorization-templates/:template_id", middleware.RequirePermission("platform:role-binding:update"), adaptHandler(positionGrantHandler.Update))
			apiRouter.DELETE("/position-authorization-templates/:template_id", middleware.RequirePermission("platform:role-binding:update"), adaptHandler(positionGrantHandler.Delete))
			apiRouter.GET("/positions/:position_id/authorization-templates", middleware.RequirePermission("platform:role-binding:read"), adaptHandler(positionGrantHandler.ListPositionAssignments))
			apiRouter.PUT("/positions/:position_id/authorization-templates", middleware.RequirePermission("platform:role-binding:update"), adaptHandler(positionGrantHandler.ReplacePositionAssignments))
			apiRouter.POST("/position-authorization-preview", middleware.RequirePermission("platform:role-binding:read"), adaptHandler(positionGrantHandler.Preview))
		}

		if auditHandler != nil {
			auditQueryRateLimit := middleware.FixedWindowRateLimit(60, time.Minute)
			apiRouter.GET("/audit/events", middleware.RequirePermission("platform:audit:view"), auditQueryRateLimit, adaptHandler(auditHandler.ListEvents))
			apiRouter.GET("/audit/events/:event_id", middleware.RequirePermission("platform:audit:view"), auditQueryRateLimit, adaptHandler(auditHandler.GetEvent))
			apiRouter.GET("/audit/chain-verify", middleware.RequirePermission("platform:audit:view"), auditQueryRateLimit, adaptHandler(auditHandler.VerifyEventChain))
			apiRouter.POST("/audit/events/:event_id/hash-check", middleware.RequirePermission("platform:audit:view"), auditQueryRateLimit, adaptHandler(auditHandler.VerifyEventHash))
			apiRouter.GET("/audit/ingestion-health", middleware.RequirePermission("platform:audit:view"), auditQueryRateLimit, adaptHandler(auditHandler.IngestionHealth))
			apiRouter.POST("/audit/export-jobs", middleware.RequirePermission("platform:audit:export"), adaptHandler(auditHandler.CreateExportJob))
			apiRouter.GET("/audit/export-jobs/:job_id", middleware.RequirePermission("platform:audit:export"), adaptHandler(auditHandler.GetExportJob))
			apiRouter.GET("/audit/export-jobs/:job_id/download", middleware.RequirePermission("platform:audit:export"), adaptHandler(auditHandler.DownloadExport))
		}

		if operational.Notifications != nil {
			apiRouter.GET("/notifications/templates", middleware.RequirePermission("platform:notification:template:read"), adaptHandler(operational.Notifications.ListTemplates))
			apiRouter.POST("/notifications/templates", middleware.RequirePermission("platform:notification:template:create"), adaptHandler(operational.Notifications.CreateTemplate))
			apiRouter.POST("/notifications/templates/:template_id/versions", middleware.RequirePermission("platform:notification:template:update"), adaptHandler(operational.Notifications.CreateTemplateVersion))
			apiRouter.PATCH("/notifications/templates/:template_id/status", middleware.RequirePermission("platform:notification:template:update"), adaptHandler(operational.Notifications.ChangeTemplateStatus))
			apiRouter.POST("/notifications/messages", middleware.RequirePermission("platform:notification:operate"), adaptHandler(operational.Notifications.CreateMessage))
			apiRouter.GET("/notifications/deliveries", middleware.RequirePermission("platform:notification:operate"), adaptHandler(operational.Notifications.ListDeliveries))
			apiRouter.POST("/notifications/deliveries/retry", middleware.RequirePermission("platform:notification:operate"), adaptHandler(operational.Notifications.RetryFailed))
			apiRouter.GET("/notifications/inbox", adaptHandler(operational.Notifications.ListInbox))
			apiRouter.GET("/notifications/inbox/unread-count", adaptHandler(operational.Notifications.UnreadCount))
			apiRouter.GET("/notifications/inbox/:delivery_id", adaptHandler(operational.Notifications.GetInboxItem))
			apiRouter.POST("/notifications/inbox/:delivery_id/read", adaptHandler(operational.Notifications.MarkRead))
			apiRouter.POST("/notifications/inbox/read-all", adaptHandler(operational.Notifications.MarkAllRead))
		}

		if operational.AsyncJobs != nil {
			// 文件读写、绑定、清理和对账只由独立 File Gateway 暴露。平台 API 保留自身
			// async_job 运营边界，避免重新连接网关数据库或对象存储。
			apiRouter.POST("/async-jobs", middleware.RequirePermission("platform:async-job:create"), adaptHandler(operational.AsyncJobs.CreateJob))
			apiRouter.GET("/async-jobs", middleware.RequirePermission("platform:async-job:read"), adaptHandler(operational.AsyncJobs.ListJobs))
			apiRouter.POST("/async-jobs/:job_id/cancel", middleware.RequirePermission("platform:async-job:cancel"), adaptHandler(operational.AsyncJobs.CancelJob))
			apiRouter.POST("/async-jobs/:job_id/retry", middleware.RequirePermission("platform:async-job:retry"), adaptHandler(operational.AsyncJobs.RetryJob))
			apiRouter.POST("/async-jobs/:job_id/rerun", middleware.RequirePermission("platform:async-job:rerun"), adaptHandler(operational.AsyncJobs.RerunJob))
		}

		if securityHandler != nil {
			apiRouter.GET("/security/login-policy", middleware.RequirePermission("platform:security-policy:read"), adaptHandler(securityHandler.GetLoginPolicy))
			apiRouter.PUT("/security/login-policy", middleware.RequirePermission("platform:security-policy:update"), adaptHandler(securityHandler.UpdateLoginPolicy))
			apiRouter.GET("/security/locked-accounts", middleware.RequirePermission("platform:locked-account:read"), adaptHandler(securityHandler.ListLockedAccounts))
			apiRouter.POST("/security/locked-accounts/:account_id/unlock", middleware.RequirePermission("platform:locked-account:unlock"), adaptHandler(securityHandler.UnlockAccount))
		}

		if configurationHandler != nil {
			apiRouter.GET("/config/namespaces", middleware.RequirePermission("platform:config-namespace:read"), adaptHandler(configurationHandler.ListNamespaces))
			apiRouter.POST("/config/namespaces", middleware.RequirePermission("platform:config-namespace:create"), adaptHandler(configurationHandler.CreateNamespace))
			apiRouter.GET("/config/items", middleware.RequirePermission("platform:config-item:read"), adaptHandler(configurationHandler.ListItems))
			apiRouter.POST("/config/items", middleware.RequirePermission("platform:config-item:create"), adaptHandler(configurationHandler.CreateItem))
			apiRouter.PATCH("/config/items/:item_id", middleware.RequirePermission("platform:config-item:update"), adaptHandler(configurationHandler.UpdateItem))
			apiRouter.POST("/config/releases", middleware.RequirePermission("platform:config-release:publish"), adaptHandler(configurationHandler.CreateRelease))
			apiRouter.GET("/config/releases/:release_id", middleware.RequirePermission("platform:config-release:read"), adaptHandler(configurationHandler.GetRelease))
			apiRouter.GET("/config/applications/:application_code/namespaces/:namespace_code", middleware.RequirePermission("platform:config:read"), adaptHandler(configurationHandler.GetPublished))
		}

		if settingsHandler != nil {
			apiRouter.GET("/settings/platform", middleware.RequirePermission("platform:settings:read"), adaptHandler(settingsHandler.GetPlatformSettings))
			apiRouter.PUT("/settings/platform", middleware.RequirePermission("platform:settings:update"), adaptHandler(settingsHandler.UpdatePlatformSettings))
			apiRouter.GET("/settings/notifications", middleware.RequirePermission("platform:notification-setting:read"), adaptHandler(settingsHandler.GetNotificationSettings))
			apiRouter.PUT("/settings/notifications", middleware.RequirePermission("platform:notification-setting:update"), adaptHandler(settingsHandler.UpdateNotificationSettings))
			apiRouter.GET("/settings/access", middleware.RequirePermission("platform:settings:read"), adaptHandler(settingsHandler.GetAccessSettings))
			apiRouter.PUT("/settings/access", middleware.RequirePermission("platform:settings:update"), adaptHandler(settingsHandler.UpdateAccessSettings))
			apiRouter.POST("/settings/access/apply", middleware.RequirePermission("platform:settings:update"), adaptHandler(settingsHandler.ApplyAccessSettings))
		}

		if dictionaryHandler != nil {
			apiRouter.GET("/dictionaries", middleware.RequirePermission("platform:dictionary:read"), adaptHandler(dictionaryHandler.ListDictionaries))
			apiRouter.POST("/dictionaries", middleware.RequirePermission("platform:dictionary:create"), adaptHandler(dictionaryHandler.CreateDictionary))
			// 业务表单的运行时选择接口：认证由上方 apiRouter 组级中间件兜底，不再要求
			// dictionary:read——业务用户通常没有字典管理权限。租户隔离基于会话主体，
			// 且服务层只返回启用字典的启用项，不暴露停用值与管理元数据。
			apiRouter.GET("/dictionaries/code/:dictionary_code/items", adaptHandler(dictionaryHandler.ListActiveItemsByCode))
			apiRouter.GET("/dictionaries/:dictionary_id", middleware.RequirePermission("platform:dictionary:read"), adaptHandler(dictionaryHandler.GetDictionary))
			apiRouter.PATCH("/dictionaries/:dictionary_id", middleware.RequirePermission("platform:dictionary:update"), adaptHandler(dictionaryHandler.UpdateDictionary))
			apiRouter.GET("/dictionaries/:dictionary_id/items", middleware.RequirePermission("platform:dictionary-item:read"), adaptHandler(dictionaryHandler.ListItems))
			apiRouter.POST("/dictionaries/:dictionary_id/items", middleware.RequirePermission("platform:dictionary-item:create"), adaptHandler(dictionaryHandler.CreateItem))
			apiRouter.PATCH("/dictionaries/:dictionary_id/items/:item_id", middleware.RequirePermission("platform:dictionary-item:update"), adaptHandler(dictionaryHandler.UpdateItem))
			apiRouter.DELETE("/dictionaries/:dictionary_id", middleware.RequirePermission("platform:dictionary:delete"), adaptHandler(dictionaryHandler.DeleteDictionary))
			apiRouter.DELETE("/dictionaries/:dictionary_id/items/:item_id", middleware.RequirePermission("platform:dictionary-item:delete"), adaptHandler(dictionaryHandler.DeleteItem))
		}

		if authorizationHandler != nil {
			apiRouter.GET("/resources", middleware.RequirePermission("platform:resource:read"), adaptHandler(authorizationHandler.ListResources))
			apiRouter.POST("/resources", middleware.RequirePermission("platform:resource:create"), adaptHandler(authorizationHandler.CreateResource))
			apiRouter.GET("/permissions", middleware.RequirePermission("platform:permission:read"), adaptHandler(authorizationHandler.ListPermissions))
			apiRouter.POST("/permissions", middleware.RequirePermission("platform:permission:create"), adaptHandler(authorizationHandler.CreatePermission))
			apiRouter.GET("/roles", middleware.RequirePermission("platform:role:read"), adaptHandler(authorizationHandler.ListRoles))
			apiRouter.POST("/roles", middleware.RequirePermission("platform:role:create"), adaptHandler(authorizationHandler.CreateRole))
			apiRouter.GET("/roles/:role_id", middleware.RequirePermission("platform:role:read"), adaptHandler(authorizationHandler.GetRole))
			apiRouter.PATCH("/roles/:role_id", middleware.RequirePermission("platform:role:update"), adaptHandler(authorizationHandler.UpdateRole))
			apiRouter.GET("/role-bindings", middleware.RequirePermission("platform:role-binding:read"), adaptHandler(authorizationHandler.ListRoleBindings))
			apiRouter.POST("/role-bindings", middleware.RequirePermission("platform:role-binding:create"), adaptHandler(authorizationHandler.CreateRoleBinding))
			apiRouter.PATCH("/role-bindings/:binding_id", middleware.RequirePermission("platform:role-binding:update"), adaptHandler(authorizationHandler.UpdateRoleBinding))
			apiRouter.GET("/authorization/effective-access", middleware.RequirePermission("platform:authorization:check"), adaptHandler(authorizationHandler.PreviewEffectiveAccess))
			apiRouter.POST("/authorization/role-binding-impact", middleware.RequirePermission("platform:role-binding:create"), adaptHandler(authorizationHandler.PreviewRoleBindingImpact))
			apiRouter.POST("/authorization/check", middleware.RequirePermission("platform:authorization:check"), adaptHandler(authorizationHandler.Check))
			apiRouter.POST("/authorization/batch-check", middleware.RequirePermission("platform:authorization:check"), adaptHandler(authorizationHandler.BatchCheck))
		}
	}

	// 应用授权目录同时支持平台控制台会话与应用自有的 OAuth client 凭据两种主体；
	// handler 负责按主体校验对应的控制台权限/归属，或目标应用的目录级 scope。
	if applicationAccessHandler != nil && authHandler != nil {
		catalogRouter := router.Group("/api/v1")
		if applicationAuthenticator != nil {
			catalogRouter.Use(middleware.RequireAllowedOriginForUnsafeMethodsOrBearer(allowedBrowserOrigins...), middleware.RequireSafeWriteContentType(), middleware.ConsoleOrApplicationAuthentication(authHandler, authHandler.CookieName(), applicationAuthenticator))
		} else {
			// 未装配应用 bearer 认证器的精简/测试部署仍保留控制台目录管理能力；
			// 完整部署额外开放上文的应用自有凭据路径。
			catalogRouter.Use(middleware.RequireAllowedOriginForUnsafeMethods(allowedBrowserOrigins...), middleware.RequireSafeWriteContentType(), middleware.Authentication(authHandler, authHandler.CookieName()))
		}
		if auditRecorder != nil {
			catalogRouter.Use(middleware.AuditTrail(auditRecorder, logger, middleware.AuditSource{ApplicationCode: cfg.Audit.ApplicationCode, EnvironmentCode: cfg.Audit.EnvironmentCode}))
		}
		catalogRouter.GET("/applications/:application_id/authorization-catalog", adaptHandler(applicationAccessHandler.GetCatalog))
		catalogRouter.PUT("/applications/:application_id/authorization-catalog", adaptHandler(applicationAccessHandler.SyncCatalog))
	}

	if applicationAuthenticator != nil && operational.RuntimeLicenses != nil {
		runtimeRouter := router.Group("/api/v1/internal/licenses/runtime")
		runtimeRouter.Use(middleware.ApplicationAuthentication(applicationAuthenticator), middleware.RequireApplicationScope("license.runtime"), middleware.RequireSafeWriteContentType())
		runtimeRouter.POST("/:service_id/ready", adaptHandler(operational.RuntimeLicenses.Ready))
		runtimeRouter.GET("/:service_id/snapshot", adaptHandler(operational.RuntimeLicenses.Snapshot))
		runtimeRouter.POST("/:service_id/ack", adaptHandler(operational.RuntimeLicenses.Ack))
	}
	// 集成侧审计上报使用独立的 bearer-token 边界：控制台用户角色不授予外部业务系统
	// 提交审计事件的权限。
	if applicationAuthenticator != nil && (auditHandler != nil || operational.Notifications != nil || operational.ExternalIdentity != nil || operational.OwnerDirectory != nil) {
		integrationRouter := router.Group("/api/v1")
		integrationRouter.Use(middleware.ApplicationAuthentication(applicationAuthenticator))
		if auditHandler != nil {
			// 子系统审计发布方的免凭据预检：与事件上报共用完全相同的 bearer-token
			// 与 audit.ingest 边界，但不触发任何 Keycloak 或审计数据变更。
			integrationRouter.GET("/audit/ingest/validate", middleware.RequireApplicationScope("audit.ingest"), adaptHandler(auditHandler.ValidateIngestion))
			integrationRouter.POST("/audit/events", middleware.RequireApplicationScope("audit.ingest"), middleware.AuditIngestionCorrelation(), adaptHandler(auditHandler.Ingest))
			integrationRouter.POST("/audit/events/batch", middleware.RequireApplicationScope("audit.ingest"), middleware.AuditIngestionCorrelation(), adaptHandler(auditHandler.IngestBatch))
		}
		if operational.Notifications != nil {
			// 外部系统只提交持久化事件信封；投递展开由 worker 异步完成，
			// 该 scope 与审计上报相互独立。
			integrationRouter.POST("/notifications/events", middleware.RequireApplicationScope("notification.ingest"), adaptHandler(operational.Notifications.Ingest))
			integrationRouter.POST("/notifications/events/batch", middleware.RequireApplicationScope("notification.ingest"), adaptHandler(operational.Notifications.IngestBatch))
			integrationRouter.GET("/notifications/events/receipts/:receipt_id", middleware.RequireApplicationScope("notification.ingest"), adaptHandler(operational.Notifications.GetIngestionReceipt))
		}
		if operational.ExternalIdentity != nil {
			integrationRouter.POST("/internal/external-users", middleware.RequireApplicationScope("external_user.provision"), adaptHandler(operational.ExternalIdentity.Provision))
			// 外部客户绑定：CRM 机器客户端专用。scope 与子系统接入的按用途凭据一致，
			// 浏览器会话或任何其他机器客户端都不能读写客户绑定。
			integrationRouter.PUT("/internal/external-users/:platform_user_id/customer-binding", middleware.RequireApplicationScope("portal_mapping_provision"), adaptHandler(operational.ExternalIdentity.BindCustomer))
			integrationRouter.POST("/internal/external-users/:platform_user_id/customer-binding/disable", middleware.RequireApplicationScope("portal_mapping_disable"), adaptHandler(operational.ExternalIdentity.DisableCustomerBinding))
			integrationRouter.GET("/internal/external-users/:platform_user_id/customer-binding", middleware.RequireApplicationScope("portal_mapping_provision"), adaptHandler(operational.ExternalIdentity.GetCustomerBinding))
			integrationRouter.POST("/internal/application-roles", middleware.RequireApplicationScope("application_role.assign"), adaptHandler(operational.ExternalIdentity.AssignRole))
			integrationRouter.POST("/internal/application-roles/revoke", middleware.RequireApplicationScope("application_role.revoke"), adaptHandler(operational.ExternalIdentity.RevokeRole))
		}
		if operational.OwnerDirectory != nil {
			integrationRouter.GET("/internal/owner-directory", middleware.RequireApplicationScope("owner_directory.read"), adaptHandler(operational.OwnerDirectory.List))
		}
	}

	router.NoRoute(func(context *gin.Context) {
		httpresponse.WriteError(context.Writer, context.Request, http.StatusNotFound, httperror.NotFound)
	})
	router.NoMethod(func(context *gin.Context) {
		httpresponse.WriteError(context.Writer, context.Request, http.StatusMethodNotAllowed, httperror.MethodNotAllowed)
	})

	return router
}

// adaptHandler 让既有 net/http 适配器保留响应封装和输入校验，同时由 Gin 执行路由与中间件。
// 路径参数先复制到 Request.PathValue，模块处理器无需依赖 Gin context。
func adaptHandler(handler http.HandlerFunc) gin.HandlerFunc {
	return func(context *gin.Context) {
		for _, parameter := range context.Params {
			context.Request.SetPathValue(parameter.Key, parameter.Value)
		}
		handler(context.Writer, context.Request)
	}
}
