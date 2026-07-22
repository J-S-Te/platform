// Package httptransport assembles the HTTP transport without embedding business logic.
package httptransport

import (
	"log/slog"
	"net/http"
	"strings"
	"time"

	applicationregistryapplication "github.com/J-S-Te/Basic-Platform/backend/internal/platform/applicationregistry/application"
	applicationregistryhttp "github.com/J-S-Te/Basic-Platform/backend/internal/platform/applicationregistry/interfaces/http"
	audithttp "github.com/J-S-Te/Basic-Platform/backend/internal/platform/audit/interfaces/http"
	authorizationhttp "github.com/J-S-Te/Basic-Platform/backend/internal/platform/authorization/interfaces/http"
	configurationhttp "github.com/J-S-Te/Basic-Platform/backend/internal/platform/configuration/interfaces/http"
	dictionaryhttp "github.com/J-S-Te/Basic-Platform/backend/internal/platform/dictionary/interfaces/http"
	filetaskhttp "github.com/J-S-Te/Basic-Platform/backend/internal/platform/filetask/interfaces/http"
	dingtalkhttp "github.com/J-S-Te/Basic-Platform/backend/internal/platform/identity/federation/dingtalk/interfaces/http"
	federationhttp "github.com/J-S-Te/Basic-Platform/backend/internal/platform/identity/federation/interfaces/http"
	federatedloginhttp "github.com/J-S-Te/Basic-Platform/backend/internal/platform/identity/federation/login/interfaces/http"
	identityhttp "github.com/J-S-Te/Basic-Platform/backend/internal/platform/identity/interfaces/http"
	mfahttp "github.com/J-S-Te/Basic-Platform/backend/internal/platform/identity/mfa/interfaces/http"
	notificationhttp "github.com/J-S-Te/Basic-Platform/backend/internal/platform/notification/interfaces/http"
	observabilityhttp "github.com/J-S-Te/Basic-Platform/backend/internal/platform/observability/interfaces/http"
	oidchttp "github.com/J-S-Te/Basic-Platform/backend/internal/platform/oidc/interfaces/http"
	securityhttp "github.com/J-S-Te/Basic-Platform/backend/internal/platform/security/interfaces/http"
	mfastepuphttp "github.com/J-S-Te/Basic-Platform/backend/internal/platform/security/mfastepup/interfaces/http"
	settingshttp "github.com/J-S-Te/Basic-Platform/backend/internal/platform/settings/interfaces/http"
	"github.com/J-S-Te/Basic-Platform/backend/internal/shared/config"
	"github.com/J-S-Te/Basic-Platform/backend/internal/shared/httperror"
	"github.com/J-S-Te/Basic-Platform/backend/internal/shared/httpresponse"
	"github.com/J-S-Te/Basic-Platform/backend/internal/transport/http/middleware"
	"github.com/gin-gonic/gin"
	"gorm.io/gorm"
)

// OperationalModules groups optional platform capabilities added after the original P0 router.
// Keeping them in one value prevents the composition root from growing another long positional
// argument list and lets router tests omit the whole group safely.
type OperationalModules struct {
	LoginTargets    *applicationregistryhttp.LoginTargetManagementHandler
	Notifications   *notificationhttp.Handler
	Observability   *observabilityhttp.Handler
	Telemetry       gin.HandlerFunc
	AuditOperations *audithttp.OperationsHandler
	FilesAndJobs    *filetaskhttp.Handler
}

// NewRouter creates the shared middleware chain and registers infrastructure endpoints. Domain
// modules register their own routes here only through their public HTTP adapters.
func NewRouter(
	cfg config.Config,
	logger *slog.Logger,
	database *gorm.DB,
	authHandler *identityhttp.Handler,
	externalLoginHandler *federatedloginhttp.Handler,
	dingTalkLoginHandler *dingtalkhttp.Handler,
	bootstrapHandler *identityhttp.BootstrapHandler,
	managementHandler *identityhttp.ManagementHandler,
	accountLifecycleHandler *identityhttp.AccountLifecycleHandler,
	authorizationHandler *authorizationhttp.Handler,
	auditHandler *audithttp.Handler,
	configurationHandler *configurationhttp.Handler,
	settingsHandler *settingshttp.Handler,
	dictionaryHandler *dictionaryhttp.Handler,
	securityHandler *securityhttp.Handler,
	applicationTokenHandler *applicationregistryhttp.Handler,
	applicationManagementHandler *applicationregistryhttp.ManagementHandler,
	oauthClientManagementHandler *applicationregistryhttp.OAuthClientManagementHandler,
	applicationAuthenticator *applicationregistryapplication.Service,
	auditRecorder middleware.AuditRecorder,
	oidcHandler *oidchttp.Handler,
	federationHandler *federationhttp.Handler,
	mfaHandler *mfahttp.Handler,
	mfaStepUpHandler *mfastepuphttp.Handler,
	mfaStepUpGrantConsumer middleware.MFAStepUpGrantConsumer,
	operational OperationalModules,
) *gin.Engine {
	router := gin.New()
	router.HandleMethodNotAllowed = true
	router.Use(
		middleware.RequestID(),
		middleware.Recover(logger),
		middleware.AccessLog(logger),
		middleware.SecurityHeaders(),
		middleware.CORS(cfg.CORSOrigins),
	)
	if operational.Telemetry != nil {
		router.Use(operational.Telemetry)
	}

	healthHandler := NewHealthHandler(database, cfg.AppName)
	router.GET("/healthz", healthHandler.Liveness)
	router.GET("/readyz", healthHandler.Readiness)

	if oidcHandler != nil {
		router.GET("/authorize", adaptHandler(oidcHandler.Authorize))
		router.POST("/oauth2/token", adaptHandler(oidcHandler.Token))
		router.POST("/oauth2/par", adaptHandler(oidcHandler.PAR))
		router.POST("/oauth2/revoke", adaptHandler(oidcHandler.Revoke))
		router.GET("/oauth2/consent", adaptHandler(oidcHandler.Consent))
		consentRouter := router.Group("/oauth2/consent")
		consentRouter.Use(middleware.RequireSameOrigin(cfg.Auth.OIDCIssuer))
		consentRouter.POST("/grant", adaptHandler(oidcHandler.GrantConsent))
		consentRouter.DELETE("", adaptHandler(oidcHandler.RevokeConsent))
		router.GET("/.well-known/openid-configuration", adaptHandler(oidcHandler.Discovery))
		router.GET("/oauth2/jwks", adaptHandler(oidcHandler.JWKS))
		router.GET("/oauth2/userinfo", adaptHandler(oidcHandler.UserInfo))
		router.POST("/oauth2/userinfo", adaptHandler(oidcHandler.UserInfo))
		router.GET("/oauth2/logout", adaptHandler(oidcHandler.Logout))
		router.POST("/oauth2/logout", adaptHandler(oidcHandler.Logout))
	} else if applicationTokenHandler != nil {
		// Retain the existing machine-to-machine endpoint for deployments that have not yet
		// configured the complete OIDC dependency graph.
		router.POST("/oauth2/token", adaptHandler(applicationTokenHandler.IssueToken))
	}

	if bootstrapHandler != nil {
		bootstrapRouter := router.Group("/api/v1/iam/bootstrap")
		bootstrapRouteHandlers := []gin.HandlerFunc{adaptHandler(bootstrapHandler.InitializeFirstSuperAdmin)}
		if strings.TrimSpace(cfg.Identity.BootstrapToken) != "" {
			bootstrapRouteHandlers = append([]gin.HandlerFunc{middleware.FixedWindowRateLimit(5, time.Minute)}, bootstrapRouteHandlers...)
		}
		bootstrapRouter.POST("/first-super-admin", bootstrapRouteHandlers...)
	}

	if authHandler != nil || externalLoginHandler != nil || dingTalkLoginHandler != nil {
		authRouter := router.Group("/api/v1/auth")
		if authHandler != nil {
			authRouter.POST("/login", middleware.FixedWindowRateLimit(30, time.Minute), adaptHandler(authHandler.Login))
			authRouter.POST("/login/mfa:verify", middleware.FixedWindowRateLimit(30, time.Minute), adaptHandler(authHandler.VerifyMFALogin))

			protected := authRouter.Group("")
			protected.Use(middleware.Authentication(authHandler, authHandler.CookieName()))
			protected.POST("/token/refresh", adaptHandler(authHandler.Refresh))
			protected.POST("/logout", adaptHandler(authHandler.Logout))
			protected.GET("/me", adaptHandler(authHandler.Me))
		}
		if externalLoginHandler != nil {
			externalLoginRateLimit := middleware.FixedWindowRateLimit(30, time.Minute)
			authRouter.GET("/external/login", externalLoginRateLimit, adaptHandler(externalLoginHandler.Start))
			authRouter.GET("/external/callback", externalLoginRateLimit, adaptHandler(externalLoginHandler.Callback))
		}
		if dingTalkLoginHandler != nil {
			dingTalkRateLimit := middleware.FixedWindowRateLimit(30, time.Minute)
			authRouter.POST("/dingtalk/qr-sessions", dingTalkRateLimit, adaptHandler(dingTalkLoginHandler.CreateQRSession))
			authRouter.GET("/dingtalk/callback", dingTalkRateLimit, adaptHandler(dingTalkLoginHandler.Callback))
		}
	}

	if authHandler != nil {
		apiRouter := router.Group("/api/v1")
		apiRouter.Use(middleware.Authentication(authHandler, authHandler.CookieName()))
		if auditRecorder != nil {
			apiRouter.Use(middleware.AuditTrail(auditRecorder, logger))
		}

		if federationHandler != nil {
			apiRouter.GET("/identity-providers", middleware.RequirePermission("platform:identity-provider:read"), adaptHandler(federationHandler.ListProviders))
			apiRouter.POST("/identity-providers", middleware.RequirePermission("platform:identity-provider:create"), adaptHandler(federationHandler.CreateProvider))
			apiRouter.GET("/identity-providers/:provider_id", middleware.RequirePermission("platform:identity-provider:read"), adaptHandler(federationHandler.GetProvider))
			apiRouter.PATCH("/identity-providers/:provider_id", middleware.RequirePermission("platform:identity-provider:update"), adaptHandler(federationHandler.UpdateProvider))
			apiRouter.GET("/users/:user_id/external-identities", middleware.RequirePermission("platform:identity-binding:read"), adaptHandler(federationHandler.ListUserBindings))
			apiRouter.POST("/users/:user_id/external-identities", middleware.RequirePermission("platform:identity-binding:create"), adaptHandler(federationHandler.BindUser))
			apiRouter.DELETE("/users/:user_id/external-identities/:binding_id", middleware.RequirePermission("platform:identity-binding:delete"), adaptHandler(federationHandler.UnbindUser))
		}

		if mfaHandler != nil {
			// MFA operations are strictly self-bound: the handler reads account and tenant only from
			// the authenticated session, so no administrator delegation permission is exposed here.
			apiRouter.POST("/mfa/totp:prepare", adaptHandler(mfaHandler.PrepareTOTP))
			apiRouter.POST("/mfa/totp:confirm", adaptHandler(mfaHandler.ConfirmTOTP))
			apiRouter.POST("/mfa/totp:disable", adaptHandler(mfaHandler.DisableTOTP))
			apiRouter.POST("/mfa/challenges", adaptHandler(mfaHandler.CreateChallenge))
			apiRouter.POST("/mfa/challenges:verify", adaptHandler(mfaHandler.VerifyChallenge))
		}

		if mfaStepUpHandler != nil {
			// Step-up challenges are also self-bound. The returned one-time grant is consumed only
			// after the target route has passed its authorization middleware.
			apiRouter.POST("/mfa/step-up/challenges", adaptHandler(mfaStepUpHandler.CreateChallenge))
			apiRouter.POST("/mfa/step-up/challenges:verify", adaptHandler(mfaStepUpHandler.VerifyChallenge))
		}

		if managementHandler != nil {
			apiRouter.GET("/users", middleware.RequirePermission("platform:user:read"), adaptHandler(managementHandler.ListUsers))
			apiRouter.POST("/users", middleware.RequirePermission("platform:user:create"), adaptHandler(managementHandler.CreateUser))
			apiRouter.GET("/users/:user_id", middleware.RequirePermission("platform:user:read"), adaptHandler(managementHandler.GetUser))
			apiRouter.PATCH("/users/:user_id", middleware.RequirePermission("platform:user:update"), adaptHandler(managementHandler.UpdateUser))
			apiRouter.GET("/accounts", middleware.RequirePermission("platform:account:read"), adaptHandler(managementHandler.ListAccounts))
			apiRouter.PATCH("/accounts/:account_id", middleware.RequirePermission("platform:account:update"), adaptHandler(managementHandler.UpdateAccount))
			apiRouter.GET("/org-units", middleware.RequirePermission("platform:organization:read"), adaptHandler(managementHandler.ListOrgUnits))
			apiRouter.POST("/org-units", middleware.RequirePermission("platform:organization:create"), adaptHandler(managementHandler.CreateOrgUnit))
			apiRouter.GET("/positions", middleware.RequirePermission("platform:position:read"), adaptHandler(managementHandler.ListPositions))
			apiRouter.POST("/positions", middleware.RequirePermission("platform:position:create"), adaptHandler(managementHandler.CreatePosition))
			apiRouter.GET("/memberships", middleware.RequirePermission("platform:membership:read"), adaptHandler(managementHandler.ListMemberships))
			apiRouter.POST("/memberships", middleware.RequirePermission("platform:membership:create"), adaptHandler(managementHandler.CreateMembership))
			apiRouter.PATCH("/memberships/:membership_id", middleware.RequirePermission("platform:membership:update"), adaptHandler(managementHandler.UpdateMembership))
		}

		if applicationManagementHandler != nil {
			apiRouter.GET("/applications", middleware.RequirePermission("platform:application:read"), adaptHandler(applicationManagementHandler.ListApplications))
			apiRouter.POST("/applications", middleware.RequirePermission("platform:application:create"), adaptHandler(applicationManagementHandler.CreateApplication))
			apiRouter.GET("/applications/:application_id", middleware.RequirePermission("platform:application:read"), adaptHandler(applicationManagementHandler.GetApplication))
			apiRouter.PATCH("/applications/:application_id", middleware.RequirePermission("platform:application:update"), adaptHandler(applicationManagementHandler.UpdateApplication))
			apiRouter.GET("/applications/:application_id/environments", middleware.RequirePermission("platform:application-environment:read"), adaptHandler(applicationManagementHandler.ListEnvironments))
			apiRouter.POST("/applications/:application_id/environments", middleware.RequirePermission("platform:application-environment:create"), adaptHandler(applicationManagementHandler.CreateEnvironment))
			apiRouter.PATCH("/applications/:application_id/environments/:environment_id", middleware.RequirePermission("platform:application-environment:update"), adaptHandler(applicationManagementHandler.UpdateEnvironment))
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
			apiRouter.GET("/oauth-clients/:oauth_client_id/post-logout-redirect-uris", middleware.RequirePermission("platform:oauth-client:read"), adaptHandler(oauthClientManagementHandler.GetOAuthClientPostLogoutRedirectURIs))
			apiRouter.PUT("/oauth-clients/:oauth_client_id/post-logout-redirect-uris", middleware.RequirePermission("platform:oauth-client:post-logout-redirect-uri-update"), adaptHandler(oauthClientManagementHandler.UpdateOAuthClientPostLogoutRedirectURIs))
			apiRouter.GET("/oauth-clients/:oauth_client_id/jwks", middleware.RequirePermission("platform:oauth-client:read"), adaptHandler(oauthClientManagementHandler.GetOAuthClientJWKs))
			apiRouter.PUT("/oauth-clients/:oauth_client_id/jwks", middleware.RequirePermission("platform:oauth-client:jwk-update"), middleware.RequireMFAStepUp(mfaStepUpGrantConsumer), adaptHandler(oauthClientManagementHandler.UpdateOAuthClientJWKs))
			apiRouter.POST("/oauth-clients/:oauth_client_id:disable", middleware.RequirePermission("platform:oauth-client:disable"), middleware.RequireMFAStepUp(mfaStepUpGrantConsumer), adaptHandler(oauthClientManagementHandler.DisableOAuthClient))
			apiRouter.POST("/oauth-clients/:oauth_client_id/credentials", middleware.RequirePermission("platform:oauth-client-credential:create"), middleware.RequireMFAStepUp(mfaStepUpGrantConsumer), adaptHandler(oauthClientManagementHandler.CreateCredential))
			apiRouter.POST("/oauth-clients/:oauth_client_id/credentials:rotate", middleware.RequirePermission("platform:oauth-client-credential:rotate"), middleware.RequireMFAStepUp(mfaStepUpGrantConsumer), adaptHandler(oauthClientManagementHandler.RotateCredential))
			apiRouter.POST("/oauth-clients/:oauth_client_id/credentials/:credential_id:disable", middleware.RequirePermission("platform:oauth-client-credential:disable"), middleware.RequireMFAStepUp(mfaStepUpGrantConsumer), adaptHandler(oauthClientManagementHandler.DisableCredential))
		}

		if accountLifecycleHandler != nil {
			apiRouter.POST("/accounts", middleware.RequirePermission("platform:account:create"), adaptHandler(accountLifecycleHandler.CreateLocalAccount))
			apiRouter.POST("/accounts/:account_id/password:initialize", middleware.RequirePermission("platform:account:password-initialize"), adaptHandler(accountLifecycleHandler.InitializePassword))
			apiRouter.POST("/accounts/:account_id/password:reset", middleware.RequirePermission("platform:account:password-reset"), middleware.RequireMFAStepUp(mfaStepUpGrantConsumer), adaptHandler(accountLifecycleHandler.ResetPassword))
			apiRouter.POST("/auth/password:change", adaptHandler(accountLifecycleHandler.ChangeOwnPassword))
		}

		if auditHandler != nil {
			auditQueryRateLimit := middleware.FixedWindowRateLimit(60, time.Minute)
			apiRouter.GET("/audit/events", middleware.RequirePermission("platform:audit:view"), auditQueryRateLimit, adaptHandler(auditHandler.ListEvents))
			apiRouter.GET("/audit/events/:event_id", middleware.RequirePermission("platform:audit:view"), auditQueryRateLimit, adaptHandler(auditHandler.GetEvent))
			apiRouter.POST("/audit/export-jobs", middleware.RequirePermission("platform:audit:export"), middleware.RequireMFAStepUp(mfaStepUpGrantConsumer), adaptHandler(auditHandler.CreateExportJob))
			apiRouter.GET("/audit/export-jobs/:job_id", middleware.RequirePermission("platform:audit:export"), adaptHandler(auditHandler.GetExportJob))
			apiRouter.GET("/audit/export-jobs/:job_id/download", middleware.RequirePermission("platform:audit:export"), adaptHandler(auditHandler.DownloadExport))
		}

		if operational.AuditOperations != nil {
			apiRouter.GET("/audit/ingestion-receipts", middleware.RequirePermission("platform:audit:ingestion-receipt:view"), adaptHandler(operational.AuditOperations.ListIngestionReceipts))
			apiRouter.GET("/audit/dead-letters", middleware.RequirePermission("platform:audit:dead-letter:view"), adaptHandler(operational.AuditOperations.ListDeadLetters))
			apiRouter.GET("/audit/dead-letters/status", middleware.RequirePermission("platform:audit:dead-letter:view"), adaptHandler(operational.AuditOperations.GetDeadLetterStatus))
			apiRouter.GET("/audit/dead-letters/:dead_letter_id", middleware.RequirePermission("platform:audit:dead-letter:view"), adaptHandler(operational.AuditOperations.GetDeadLetter))
			apiRouter.POST("/audit/dead-letters/:dead_letter_id:replay", middleware.RequirePermission("platform:audit:dead-letter:replay"), middleware.RequireMFAStepUp(mfaStepUpGrantConsumer), adaptHandler(operational.AuditOperations.ReplayDeadLetter))
			apiRouter.POST("/audit/dead-letters:replay", middleware.RequirePermission("platform:audit:dead-letter:replay"), middleware.RequireMFAStepUp(mfaStepUpGrantConsumer), adaptHandler(operational.AuditOperations.ReplayDeadLetters))
			apiRouter.GET("/audit/retention-tasks", middleware.RequirePermission("platform:audit:retention:manage"), adaptHandler(operational.AuditOperations.ListRetentionTasks))
			apiRouter.POST("/audit/retention-tasks", middleware.RequirePermission("platform:audit:retention:manage"), middleware.RequireMFAStepUp(mfaStepUpGrantConsumer), adaptHandler(operational.AuditOperations.CreateRetentionTask))
		}

		if operational.Notifications != nil {
			apiRouter.GET("/notifications/templates", middleware.RequirePermission("platform:notification:template:read"), adaptHandler(operational.Notifications.ListTemplates))
			apiRouter.POST("/notifications/templates", middleware.RequirePermission("platform:notification:template:create"), adaptHandler(operational.Notifications.CreateTemplate))
			apiRouter.POST("/notifications/templates/:template_id/versions", middleware.RequirePermission("platform:notification:template:update"), adaptHandler(operational.Notifications.CreateTemplateVersion))
			apiRouter.PATCH("/notifications/templates/:template_id/status", middleware.RequirePermission("platform:notification:template:update"), adaptHandler(operational.Notifications.ChangeTemplateStatus))
			apiRouter.POST("/notifications/messages", middleware.RequirePermission("platform:notification:operate"), adaptHandler(operational.Notifications.CreateMessage))
			apiRouter.GET("/notifications/deliveries", middleware.RequirePermission("platform:notification:operate"), adaptHandler(operational.Notifications.ListDeliveries))
			apiRouter.POST("/notifications/deliveries:retry", middleware.RequirePermission("platform:notification:operate"), adaptHandler(operational.Notifications.RetryFailed))
			apiRouter.GET("/notifications/inbox", adaptHandler(operational.Notifications.ListInbox))
			apiRouter.GET("/notifications/inbox/unread-count", adaptHandler(operational.Notifications.UnreadCount))
			apiRouter.GET("/notifications/inbox/:delivery_id", adaptHandler(operational.Notifications.GetInboxItem))
			apiRouter.POST("/notifications/inbox/:delivery_id:read", adaptHandler(operational.Notifications.MarkRead))
			apiRouter.POST("/notifications/inbox:read-all", adaptHandler(operational.Notifications.MarkAllRead))
		}

		if operational.Observability != nil {
			apiRouter.GET("/observability/logs", middleware.RequirePermission("platform:observability:log:view"), operational.Observability.ListLogs)
			apiRouter.GET("/observability/traces", middleware.RequirePermission("platform:observability:trace:view"), operational.Observability.ListTraces)
			apiRouter.GET("/observability/metrics", middleware.RequirePermission("platform:observability:metric:view"), operational.Observability.ListMetrics)
			apiRouter.GET("/observability/alert-rules", middleware.RequirePermission("platform:observability:alert:manage"), operational.Observability.ListAlertRules)
			apiRouter.POST("/observability/alert-rules", middleware.RequirePermission("platform:observability:alert:manage"), operational.Observability.CreateAlertRule)
			apiRouter.PATCH("/observability/alert-rules/:rule_id", middleware.RequirePermission("platform:observability:alert:manage"), operational.Observability.UpdateAlertRule)
			apiRouter.POST("/observability/alert-rules/:rule_id:execute", middleware.RequirePermission("platform:observability:alert:execute"), operational.Observability.ExecuteAlertRule)
		}

		if operational.FilesAndJobs != nil {
			apiRouter.POST("/files", middleware.RequirePermission("platform:file:upload"), adaptHandler(operational.FilesAndJobs.Upload))
			apiRouter.GET("/files/:file_id/content", middleware.RequirePermission("platform:file:download"), adaptHandler(operational.FilesAndJobs.Download))
			apiRouter.POST("/files:cleanup", middleware.RequirePermission("platform:file:cleanup"), middleware.RequireMFAStepUp(mfaStepUpGrantConsumer), adaptHandler(operational.FilesAndJobs.CleanupFiles))
			apiRouter.POST("/async-jobs", middleware.RequirePermission("platform:async-job:create"), adaptHandler(operational.FilesAndJobs.CreateJob))
			apiRouter.GET("/async-jobs", middleware.RequirePermission("platform:async-job:read"), adaptHandler(operational.FilesAndJobs.ListJobs))
			apiRouter.POST("/async-jobs/:job_id:cancel", middleware.RequirePermission("platform:async-job:cancel"), adaptHandler(operational.FilesAndJobs.CancelJob))
			apiRouter.POST("/async-jobs/:job_id:retry", middleware.RequirePermission("platform:async-job:retry"), middleware.RequireMFAStepUp(mfaStepUpGrantConsumer), adaptHandler(operational.FilesAndJobs.RetryJob))
			apiRouter.POST("/async-jobs/:job_id:rerun", middleware.RequirePermission("platform:async-job:rerun"), middleware.RequireMFAStepUp(mfaStepUpGrantConsumer), adaptHandler(operational.FilesAndJobs.RerunJob))
		}

		if securityHandler != nil {
			apiRouter.GET("/security/login-policy", middleware.RequirePermission("platform:security-policy:read"), adaptHandler(securityHandler.GetLoginPolicy))
			apiRouter.PUT("/security/login-policy", middleware.RequirePermission("platform:security-policy:update"), middleware.RequireMFAStepUp(mfaStepUpGrantConsumer), adaptHandler(securityHandler.UpdateLoginPolicy))
			apiRouter.GET("/security/locked-accounts", middleware.RequirePermission("platform:locked-account:read"), adaptHandler(securityHandler.ListLockedAccounts))
			apiRouter.POST("/security/locked-accounts/:account_id/unlock", middleware.RequirePermission("platform:locked-account:unlock"), middleware.RequireMFAStepUp(mfaStepUpGrantConsumer), adaptHandler(securityHandler.UnlockAccount))
			apiRouter.GET("/security/risk-events", middleware.RequirePermission("platform:risk-event:read"), adaptHandler(securityHandler.ListRiskEvents))
			apiRouter.POST("/security/risk-events/:risk_event_id/resolve", middleware.RequirePermission("platform:risk-event:resolve"), adaptHandler(securityHandler.ResolveRiskEvent))
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
		}

		if dictionaryHandler != nil {
			apiRouter.GET("/dictionaries", middleware.RequirePermission("platform:dictionary:read"), adaptHandler(dictionaryHandler.ListDictionaries))
			apiRouter.POST("/dictionaries", middleware.RequirePermission("platform:dictionary:create"), adaptHandler(dictionaryHandler.CreateDictionary))
			apiRouter.GET("/dictionaries/code/:dictionary_code/items", middleware.RequirePermission("platform:dictionary:read"), adaptHandler(dictionaryHandler.ListActiveItemsByCode))
			apiRouter.GET("/dictionaries/:dictionary_id", middleware.RequirePermission("platform:dictionary:read"), adaptHandler(dictionaryHandler.GetDictionary))
			apiRouter.PATCH("/dictionaries/:dictionary_id", middleware.RequirePermission("platform:dictionary:update"), adaptHandler(dictionaryHandler.UpdateDictionary))
			apiRouter.GET("/dictionaries/:dictionary_id/items", middleware.RequirePermission("platform:dictionary-item:read"), adaptHandler(dictionaryHandler.ListItems))
			apiRouter.POST("/dictionaries/:dictionary_id/items", middleware.RequirePermission("platform:dictionary-item:create"), adaptHandler(dictionaryHandler.CreateItem))
			apiRouter.PATCH("/dictionaries/:dictionary_id/items/:item_id", middleware.RequirePermission("platform:dictionary-item:update"), adaptHandler(dictionaryHandler.UpdateItem))
		}

		if authorizationHandler != nil {
			apiRouter.GET("/resources", middleware.RequirePermission("platform:resource:read"), adaptHandler(authorizationHandler.ListResources))
			apiRouter.POST("/resources", middleware.RequirePermission("platform:resource:create"), middleware.RequireMFAStepUp(mfaStepUpGrantConsumer), adaptHandler(authorizationHandler.CreateResource))
			apiRouter.GET("/permissions", middleware.RequirePermission("platform:permission:read"), adaptHandler(authorizationHandler.ListPermissions))
			apiRouter.POST("/permissions", middleware.RequirePermission("platform:permission:create"), middleware.RequireMFAStepUp(mfaStepUpGrantConsumer), adaptHandler(authorizationHandler.CreatePermission))
			apiRouter.GET("/roles", middleware.RequirePermission("platform:role:read"), adaptHandler(authorizationHandler.ListRoles))
			apiRouter.POST("/roles", middleware.RequirePermission("platform:role:create"), middleware.RequireMFAStepUp(mfaStepUpGrantConsumer), adaptHandler(authorizationHandler.CreateRole))
			apiRouter.GET("/roles/:role_id", middleware.RequirePermission("platform:role:read"), adaptHandler(authorizationHandler.GetRole))
			apiRouter.PATCH("/roles/:role_id", middleware.RequirePermission("platform:role:update"), middleware.RequireMFAStepUp(mfaStepUpGrantConsumer), adaptHandler(authorizationHandler.UpdateRole))
			apiRouter.GET("/role-bindings", middleware.RequirePermission("platform:role-binding:read"), adaptHandler(authorizationHandler.ListRoleBindings))
			apiRouter.POST("/role-bindings", middleware.RequirePermission("platform:role-binding:create"), middleware.RequireMFAStepUp(mfaStepUpGrantConsumer), adaptHandler(authorizationHandler.CreateRoleBinding))
			apiRouter.PATCH("/role-bindings/:binding_id", middleware.RequirePermission("platform:role-binding:update"), middleware.RequireMFAStepUp(mfaStepUpGrantConsumer), adaptHandler(authorizationHandler.UpdateRoleBinding))
			apiRouter.POST("/authorization/check", middleware.RequirePermission("platform:authorization:check"), adaptHandler(authorizationHandler.Check))
			apiRouter.POST("/authorization/batch-check", middleware.RequirePermission("platform:authorization:check"), adaptHandler(authorizationHandler.BatchCheck))
		}
	}

	// Integration audit ingestion has a separate bearer-token boundary. Console user roles do
	// not grant an external business system permission to submit audit events.
	if auditHandler != nil && applicationAuthenticator != nil {
		integrationRouter := router.Group("/api/v1")
		integrationRouter.Use(middleware.ApplicationAuthentication(applicationAuthenticator))
		integrationRouter.POST("/audit/events", middleware.RequireApplicationScope("audit.ingest"), middleware.AuditIngestionCorrelation(), adaptHandler(auditHandler.Ingest))
		integrationRouter.POST("/audit/events:batch", middleware.RequireApplicationScope("audit.ingest"), middleware.AuditIngestionCorrelation(), adaptHandler(auditHandler.IngestBatch))
	}

	router.NoRoute(func(context *gin.Context) {
		httpresponse.WriteError(context.Writer, context.Request, http.StatusNotFound, httperror.NotFound)
	})
	router.NoMethod(func(context *gin.Context) {
		httpresponse.WriteError(context.Writer, context.Request, http.StatusMethodNotAllowed, httperror.MethodNotAllowed)
	})

	return router
}

// adaptHandler allows existing module HTTP adapters to retain their response validation and
// envelope logic while Gin owns routing and middleware execution. Gin path parameters are copied
// into net/http request path values before the module handler runs.
func adaptHandler(handler http.HandlerFunc) gin.HandlerFunc {
	return func(context *gin.Context) {
		for _, parameter := range context.Params {
			context.Request.SetPathValue(parameter.Key, parameter.Value)
		}
		handler(context.Writer, context.Request)
	}
}
