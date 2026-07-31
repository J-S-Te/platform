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
	applicationaccess "github.com/J-S-Te/Basic-Platform/backend/internal/platform/authorization/applicationaccess"
	authorizationhttp "github.com/J-S-Te/Basic-Platform/backend/internal/platform/authorization/interfaces/http"
	positiongrant "github.com/J-S-Te/Basic-Platform/backend/internal/platform/authorization/positiongrant"
	configurationhttp "github.com/J-S-Te/Basic-Platform/backend/internal/platform/configuration/interfaces/http"
	dictionaryhttp "github.com/J-S-Te/Basic-Platform/backend/internal/platform/dictionary/interfaces/http"
	filetaskhttp "github.com/J-S-Te/Basic-Platform/backend/internal/platform/filetask/interfaces/http"
	identityhttp "github.com/J-S-Te/Basic-Platform/backend/internal/platform/identity/interfaces/http"
	notificationhttp "github.com/J-S-Te/Basic-Platform/backend/internal/platform/notification/interfaces/http"
	oidchttp "github.com/J-S-Te/Basic-Platform/backend/internal/platform/oidc/interfaces/http"
	securityhttp "github.com/J-S-Te/Basic-Platform/backend/internal/platform/security/interfaces/http"
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
	LoginTargets        *applicationregistryhttp.LoginTargetManagementHandler
	SubsystemOnboarding *applicationregistryhttp.SubsystemOnboardingHandler
	Notifications       *notificationhttp.Handler
	FilesAndJobs        *filetaskhttp.Handler
}

// NewRouter creates the shared middleware chain and registers infrastructure endpoints. Domain
// modules register their own routes here only through their public HTTP adapters.
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
	oauthClientManagementHandler *applicationregistryhttp.OAuthClientManagementHandler,
	applicationAuthenticator *applicationregistryapplication.Service,
	auditRecorder middleware.AuditRecorder,
	oidcHandler *oidchttp.Handler,
	operational OperationalModules,
) *gin.Engine {
	router := gin.New()
	router.HandleMethodNotAllowed = true
	if err := router.SetTrustedProxies(cfg.HTTP.TrustedProxies); err != nil {
		panic("invalid trusted proxy configuration: " + err.Error())
	}
	allowedBrowserOrigins := append([]string(nil), cfg.CORSOrigins...)
	allowedBrowserOrigins = append(allowedBrowserOrigins, cfg.Auth.OIDCIssuer)

	router.Use(
		middleware.RequestID(),
		middleware.ClientIP(),
		middleware.Recover(logger),
		middleware.AccessLog(logger),
		middleware.SecurityHeaders(),
		middleware.CORS(cfg.CORSOrigins),
	)
	healthHandler := NewHealthHandler(database, cfg.AppName)
	router.GET("/healthz", healthHandler.Liveness)
	router.GET("/readyz", healthHandler.Readiness)

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
		router.GET("/oauth2/logout", adaptHandler(oidcHandler.Logout))
		router.POST("/oauth2/logout", middleware.RequireSameOrigin(cfg.Auth.OIDCIssuer), adaptHandler(oidcHandler.Logout))
	} else if applicationTokenHandler != nil {
		// Retain the existing machine-to-machine endpoint for deployments that have not yet
		// configured the complete OIDC dependency graph.
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
		authRouter := router.Group("/api/v1/auth")
		authRouter.Use(middleware.RequireAllowedOriginForUnsafeMethods(allowedBrowserOrigins...), middleware.RequireSafeWriteContentType())
		authRouter.POST("/login", middleware.FixedWindowRateLimit(30, time.Minute), adaptHandler(authHandler.Login))

		protected := authRouter.Group("")
		protected.Use(middleware.Authentication(authHandler, authHandler.CookieName()))
		protected.POST("/token/refresh", adaptHandler(authHandler.Refresh))
		protected.POST("/activity", adaptHandler(authHandler.Activity))
		protected.POST("/logout", adaptHandler(authHandler.Logout))
		protected.GET("/me", adaptHandler(authHandler.Me))
	}

	if authHandler != nil {
		apiRouter := router.Group("/api/v1")
		apiRouter.Use(middleware.RequireAllowedOriginForUnsafeMethods(allowedBrowserOrigins...), middleware.RequireSafeWriteContentType())
		apiRouter.Use(middleware.Authentication(authHandler, authHandler.CookieName()))
		if auditRecorder != nil {
			apiRouter.Use(middleware.AuditTrail(auditRecorder, logger))
		}

		if managementHandler != nil {
			apiRouter.GET("/users", middleware.RequirePermission("platform:user:read"), adaptHandler(managementHandler.ListUsers))
			apiRouter.POST("/users", middleware.RequirePermission("platform:user:create"), adaptHandler(managementHandler.CreateUser))
			apiRouter.POST("/employees", middleware.RequirePermission("platform:user:create"), adaptHandler(managementHandler.CreateEmployee))
			apiRouter.POST("/users/batch", middleware.RequirePermission("platform:user:create"), adaptHandler(managementHandler.CreateUsersBatch))
			apiRouter.GET("/users/:user_id", middleware.RequirePermission("platform:user:read"), adaptHandler(managementHandler.GetUser))
			apiRouter.PATCH("/users/:user_id", middleware.RequirePermission("platform:user:update"), adaptHandler(managementHandler.UpdateUser))
			apiRouter.DELETE("/users/:user_id", middleware.RequirePermission("platform:user:delete"), adaptHandler(managementHandler.DeleteUser))
			apiRouter.GET("/accounts", middleware.RequirePermission("platform:account:read"), adaptHandler(managementHandler.ListAccounts))
			apiRouter.PATCH("/accounts/:account_id", middleware.RequirePermission("platform:account:update"), adaptHandler(managementHandler.UpdateAccount))
			apiRouter.GET("/org-units", adaptHandler(managementHandler.ListOrgUnits))
			apiRouter.POST("/org-units", adaptHandler(managementHandler.CreateOrgUnit))
			apiRouter.PATCH("/org-units/:org_unit_id", adaptHandler(managementHandler.UpdateOrgUnit))
			apiRouter.DELETE("/org-units/:org_unit_id", adaptHandler(managementHandler.DeleteOrgUnit))
			apiRouter.GET("/positions", adaptHandler(managementHandler.ListPositions))
			apiRouter.POST("/positions", adaptHandler(managementHandler.CreatePosition))
			apiRouter.DELETE("/positions/:position_id", adaptHandler(managementHandler.DeletePosition))
			apiRouter.GET("/memberships", adaptHandler(managementHandler.ListMemberships))
			apiRouter.POST("/memberships", adaptHandler(managementHandler.CreateMembership))
			apiRouter.PATCH("/memberships/:membership_id", adaptHandler(managementHandler.UpdateMembership))
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
		}

		if operational.SubsystemOnboarding != nil {
			apiRouter.GET("/portal/applications", adaptHandler(operational.SubsystemOnboarding.ListPortalApplications))
			apiRouter.POST("/subsystem-onboarding",
				middleware.RequirePermission("platform:application:create"),
				middleware.RequirePermission("platform:application-environment:create"),
				middleware.RequirePermission("platform:application-login-target:create"),
				middleware.RequirePermission("platform:oauth-client:create"),
				adaptHandler(operational.SubsystemOnboarding.OnboardSubsystem),
			)
			// Reapply a previously-onboarded subsystem: rewrite .env.local, rebuild containers,
			// refresh the portal gateway include. Caller is expected to have already updated
			// /environments and /oauth-clients via PATCH if BaseURL/UpstreamURL/PathPrefix changed.
			// Use oauth-client:disable (not :update) because the platform's permission catalog
			// only has granular update permissions (scope/redirect-uri/jwk), no generic :update.
			// Re-provisioning effectively re-issues the running OAuth client binding, so :disable
			// is the closest existing permission.
			apiRouter.POST("/subsystem-update",
				middleware.RequirePermission("platform:application:update"),
				middleware.RequirePermission("platform:application-environment:update"),
				middleware.RequirePermission("platform:application-login-target:update"),
				middleware.RequirePermission("platform:oauth-client:disable"),
				adaptHandler(operational.SubsystemOnboarding.UpdateSubsystem),
			)
			// Tear down an onboarded subsystem: stop containers, remove .env.local, remove
			// the portal gateway include, reload nginx. The HTTP layer does NOT delete the DB
			// rows here; the script is expected to follow up with DELETE /environments and
			// (optionally) DELETE /applications so the audit trail is preserved per step.
			// Use lifecycle-update permissions, not :delete, because this endpoint mutates
			// infrastructure state (containers/files/gateway), not DB rows. The DB delete is
			// gated by the regular DELETE /environments endpoint's own :delete permission.
			apiRouter.POST("/subsystem-teardown",
				middleware.RequirePermission("platform:application:update"),
				middleware.RequirePermission("platform:application-environment:update"),
				middleware.RequirePermission("platform:application-login-target:update"),
				middleware.RequirePermission("platform:oauth-client:disable"),
				adaptHandler(operational.SubsystemOnboarding.TeardownSubsystem),
			)
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
			apiRouter.PUT("/oauth-clients/:oauth_client_id/jwks", middleware.RequirePermission("platform:oauth-client:jwk-update"), adaptHandler(oauthClientManagementHandler.UpdateOAuthClientJWKs))
			apiRouter.POST("/oauth-clients/:oauth_client_id/disable", middleware.RequirePermission("platform:oauth-client:disable"), adaptHandler(oauthClientManagementHandler.DisableOAuthClient))
			apiRouter.POST("/oauth-clients/:oauth_client_id/credentials", middleware.RequirePermission("platform:oauth-client-credential:create"), adaptHandler(oauthClientManagementHandler.CreateCredential))
			apiRouter.POST("/oauth-clients/:oauth_client_id/credentials/rotate", middleware.RequirePermission("platform:oauth-client-credential:rotate"), adaptHandler(oauthClientManagementHandler.RotateCredential))
			apiRouter.POST("/oauth-clients/:oauth_client_id/credentials/:credential_id/disable", middleware.RequirePermission("platform:oauth-client-credential:disable"), adaptHandler(oauthClientManagementHandler.DisableCredential))
		}

		if accountLifecycleHandler != nil {
			apiRouter.POST("/accounts", middleware.RequirePermission("platform:account:create"), adaptHandler(accountLifecycleHandler.CreateLocalAccount))
			apiRouter.POST("/accounts/:account_id/password/initialize", middleware.RequirePermission("platform:account:password-initialize"), adaptHandler(accountLifecycleHandler.InitializePassword))
			apiRouter.POST("/accounts/:account_id/password/reset", middleware.RequirePermission("platform:account:password-reset"), adaptHandler(accountLifecycleHandler.ResetPassword))
			apiRouter.POST("/auth/password/change", adaptHandler(accountLifecycleHandler.ChangeOwnPassword))
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

		if operational.FilesAndJobs != nil {
			apiRouter.POST("/files", middleware.RequirePermission("platform:file:upload"), adaptHandler(operational.FilesAndJobs.Upload))
			apiRouter.GET("/files/:file_id/content", middleware.RequirePermission("platform:file:download"), adaptHandler(operational.FilesAndJobs.Download))
			apiRouter.POST("/files/cleanup", middleware.RequirePermission("platform:file:cleanup"), adaptHandler(operational.FilesAndJobs.CleanupFiles))
			apiRouter.POST("/async-jobs", middleware.RequirePermission("platform:async-job:create"), adaptHandler(operational.FilesAndJobs.CreateJob))
			apiRouter.GET("/async-jobs", middleware.RequirePermission("platform:async-job:read"), adaptHandler(operational.FilesAndJobs.ListJobs))
			apiRouter.POST("/async-jobs/:job_id/cancel", middleware.RequirePermission("platform:async-job:cancel"), adaptHandler(operational.FilesAndJobs.CancelJob))
			apiRouter.POST("/async-jobs/:job_id/retry", middleware.RequirePermission("platform:async-job:retry"), adaptHandler(operational.FilesAndJobs.RetryJob))
			apiRouter.POST("/async-jobs/:job_id/rerun", middleware.RequirePermission("platform:async-job:rerun"), adaptHandler(operational.FilesAndJobs.RerunJob))
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
			apiRouter.POST("/authorization/check", middleware.RequirePermission("platform:authorization:check"), adaptHandler(authorizationHandler.Check))
			apiRouter.POST("/authorization/batch-check", middleware.RequirePermission("platform:authorization:check"), adaptHandler(authorizationHandler.BatchCheck))
		}
	}

	// Application authorization catalogs support both the platform console session and an
	// application-owned OAuth client credential. The handler enforces the matching console
	// permission/ownership or the target application's catalog scope.
	if applicationAccessHandler != nil && authHandler != nil {
		catalogRouter := router.Group("/api/v1")
		if applicationAuthenticator != nil {
			catalogRouter.Use(middleware.RequireAllowedOriginForUnsafeMethodsOrBearer(allowedBrowserOrigins...), middleware.RequireSafeWriteContentType(), middleware.ConsoleOrApplicationAuthentication(authHandler, authHandler.CookieName(), applicationAuthenticator))
		} else {
			// Keep console catalog management available in reduced/test deployments that do not
			// construct the application bearer authenticator. Full deployments additionally expose
			// the application-owned credential path above.
			catalogRouter.Use(middleware.RequireAllowedOriginForUnsafeMethods(allowedBrowserOrigins...), middleware.RequireSafeWriteContentType(), middleware.Authentication(authHandler, authHandler.CookieName()))
		}
		catalogRouter.GET("/applications/:application_id/authorization-catalog", adaptHandler(applicationAccessHandler.GetCatalog))
		catalogRouter.PUT("/applications/:application_id/authorization-catalog", adaptHandler(applicationAccessHandler.SyncCatalog))
	}

	// Integration audit ingestion has a separate bearer-token boundary. Console user roles do
	// not grant an external business system permission to submit audit events.
	if auditHandler != nil && applicationAuthenticator != nil {
		integrationRouter := router.Group("/api/v1")
		integrationRouter.Use(middleware.ApplicationAuthentication(applicationAuthenticator))
		integrationRouter.POST("/audit/events", middleware.RequireApplicationScope("audit.ingest"), middleware.AuditIngestionCorrelation(), adaptHandler(auditHandler.Ingest))
		integrationRouter.POST("/audit/events/batch", middleware.RequireApplicationScope("audit.ingest"), middleware.AuditIngestionCorrelation(), adaptHandler(auditHandler.IngestBatch))
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
