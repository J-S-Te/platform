// Package bootstrap wires infrastructure dependencies into runnable processes.
package bootstrap

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"time"

	applicationregistryapplication "github.com/J-S-Te/Basic-Platform/backend/internal/platform/applicationregistry/application"
	applicationregistryinfrastructure "github.com/J-S-Te/Basic-Platform/backend/internal/platform/applicationregistry/infrastructure"
	applicationregistryhttp "github.com/J-S-Te/Basic-Platform/backend/internal/platform/applicationregistry/interfaces/http"
	auditapplication "github.com/J-S-Te/Basic-Platform/backend/internal/platform/audit/application"
	auditinfrastructure "github.com/J-S-Te/Basic-Platform/backend/internal/platform/audit/infrastructure"
	audithttp "github.com/J-S-Te/Basic-Platform/backend/internal/platform/audit/interfaces/http"
	authorizationapplication "github.com/J-S-Te/Basic-Platform/backend/internal/platform/authorization/application"
	applicationaccess "github.com/J-S-Te/Basic-Platform/backend/internal/platform/authorization/applicationaccess"
	authorizationinfrastructure "github.com/J-S-Te/Basic-Platform/backend/internal/platform/authorization/infrastructure"
	authorizationhttp "github.com/J-S-Te/Basic-Platform/backend/internal/platform/authorization/interfaces/http"
	managementscope "github.com/J-S-Te/Basic-Platform/backend/internal/platform/authorization/managementscope"
	positiongrant "github.com/J-S-Te/Basic-Platform/backend/internal/platform/authorization/positiongrant"
	configurationapplication "github.com/J-S-Te/Basic-Platform/backend/internal/platform/configuration/application"
	configurationinfrastructure "github.com/J-S-Te/Basic-Platform/backend/internal/platform/configuration/infrastructure"
	configurationhttp "github.com/J-S-Te/Basic-Platform/backend/internal/platform/configuration/interfaces/http"
	dictionaryapplication "github.com/J-S-Te/Basic-Platform/backend/internal/platform/dictionary/application"
	dictionaryinfrastructure "github.com/J-S-Te/Basic-Platform/backend/internal/platform/dictionary/infrastructure"
	dictionaryhttp "github.com/J-S-Te/Basic-Platform/backend/internal/platform/dictionary/interfaces/http"
	identityapplication "github.com/J-S-Te/Basic-Platform/backend/internal/platform/identity/application"
	"github.com/J-S-Te/Basic-Platform/backend/internal/platform/identity/infrastructure"
	identityhttp "github.com/J-S-Te/Basic-Platform/backend/internal/platform/identity/interfaces/http"
	oidcapplication "github.com/J-S-Te/Basic-Platform/backend/internal/platform/oidc/application"
	oidcinfrastructure "github.com/J-S-Te/Basic-Platform/backend/internal/platform/oidc/infrastructure"
	oidcaccesssubject "github.com/J-S-Te/Basic-Platform/backend/internal/platform/oidc/interfaces/accesssubject"
	oidchttp "github.com/J-S-Te/Basic-Platform/backend/internal/platform/oidc/interfaces/http"
	oidctokenissuer "github.com/J-S-Te/Basic-Platform/backend/internal/platform/oidc/interfaces/tokenissuer"
	securityapplication "github.com/J-S-Te/Basic-Platform/backend/internal/platform/security/application"
	securityinfrastructure "github.com/J-S-Te/Basic-Platform/backend/internal/platform/security/infrastructure"
	securityhttp "github.com/J-S-Te/Basic-Platform/backend/internal/platform/security/interfaces/http"
	settingsapplication "github.com/J-S-Te/Basic-Platform/backend/internal/platform/settings/application"
	settingsinfrastructure "github.com/J-S-Te/Basic-Platform/backend/internal/platform/settings/infrastructure"
	settingshttp "github.com/J-S-Te/Basic-Platform/backend/internal/platform/settings/interfaces/http"
	"github.com/J-S-Te/Basic-Platform/backend/internal/shared/config"
	"github.com/J-S-Te/Basic-Platform/backend/internal/shared/database"
	"github.com/J-S-Te/Basic-Platform/backend/internal/shared/observability"
	"github.com/J-S-Te/Basic-Platform/backend/internal/shared/security"
	"github.com/J-S-Te/Basic-Platform/backend/internal/shared/ulid"
	httptransport "github.com/J-S-Te/Basic-Platform/backend/internal/transport/http"
	"gorm.io/gorm"
)

// API is the dependency container for the HTTP process.
type API struct {
	Handler http.Handler
	Logger  *slog.Logger

	database *gorm.DB
	logFile  io.Closer
}

// NewAPI creates the local storage directories, structured logger, database pool and HTTP router.
// It deliberately does not ping MySQL during startup; /readyz reports dependency state while
// /healthz remains available for process liveness.
func NewAPI(cfg config.Config) (*API, error) {
	if err := os.MkdirAll(cfg.FileStorageRoot, 0o750); err != nil {
		return nil, fmt.Errorf("create file storage root: %w", err)
	}

	logger, logFile, err := observability.NewLogger(cfg.Logging, cfg.AppName, cfg.Environment)
	if err != nil {
		return nil, err
	}

	db, err := database.OpenMySQL(cfg.MySQL)
	if err != nil {
		_ = logFile.Close()
		return nil, err
	}

	tokenManager, err := security.LoadJWTManager(
		cfg.Auth.JWTIssuer, cfg.Auth.JWTAudience, cfg.Auth.JWTPrivateKeyPath, cfg.Auth.JWTPublicKeyPath,
	)
	if err != nil {
		_ = database.Close(db)
		_ = logFile.Close()
		return nil, fmt.Errorf("load JWT signing keys: %w", err)
	}
	applicationTokenManager, err := security.LoadApplicationJWTManager(
		cfg.Auth.JWTIssuer, cfg.Auth.ApplicationJWTAudience, cfg.Auth.JWTPrivateKeyPath, cfg.Auth.JWTPublicKeyPath,
	)
	if err != nil {
		_ = database.Close(db)
		_ = logFile.Close()
		return nil, fmt.Errorf("load application JWT signing keys: %w", err)
	}
	oidcTokenManager, err := security.LoadOIDCJWTManager(
		cfg.Auth.OIDCIssuer, cfg.Auth.JWTPrivateKeyPath, cfg.Auth.JWTPublicKeyPath,
	)
	if err != nil {
		_ = database.Close(db)
		_ = logFile.Close()
		return nil, fmt.Errorf("load OIDC JWT signing keys: %w", err)
	}
	repository, err := infrastructure.NewGORMRepository(db)
	if err != nil {
		_ = database.Close(db)
		_ = logFile.Close()
		return nil, err
	}
	loginSecurityRepository, err := securityinfrastructure.NewGORMRepository(db)
	if err != nil {
		_ = database.Close(db)
		_ = logFile.Close()
		return nil, err
	}
	loginSecurityService, err := securityapplication.NewService(
		loginSecurityRepository, securityapplication.SystemClock{},
	)
	if err != nil {
		_ = database.Close(db)
		_ = logFile.Close()
		return nil, err
	}
	mobileProtector, err := security.NewMobileProtector(cfg.Identity.MobileEncryptionKey)
	if err != nil {
		_ = database.Close(db)
		_ = logFile.Close()
		return nil, err
	}
	managementService, err := identityapplication.NewManagementService(
		repository, mobileProtector, ulid.Generator{}, identityapplication.SystemClock{}, security.Argon2idPasswordHasher{},
	)
	if err != nil {
		_ = database.Close(db)
		_ = logFile.Close()
		return nil, err
	}
	managementScopeAuthorizer, err := managementscope.New(db)
	if err != nil {
		_ = database.Close(db)
		_ = logFile.Close()
		return nil, err
	}
	managementHandler, err := identityhttp.NewManagementHandler(managementService, logger, managementScopeAuthorizer)
	if err != nil {
		_ = database.Close(db)
		_ = logFile.Close()
		return nil, err
	}

	authorizationRepository, err := authorizationinfrastructure.NewGORMRepository(db)
	if err != nil {
		_ = database.Close(db)
		_ = logFile.Close()
		return nil, err
	}
	authorizationService, err := authorizationapplication.NewService(
		authorizationRepository, ulid.Generator{}, authorizationapplication.SystemClock{},
	)
	if err != nil {
		_ = database.Close(db)
		_ = logFile.Close()
		return nil, err
	}
	authorizationHandler, err := authorizationhttp.NewHandler(authorizationService, logger)
	if err != nil {
		_ = database.Close(db)
		_ = logFile.Close()
		return nil, err
	}

	auditRepository, err := auditinfrastructure.NewRepository(db)
	if err != nil {
		_ = database.Close(db)
		_ = logFile.Close()
		return nil, err
	}
	auditService, err := auditapplication.NewService(
		auditRepository, ulid.Generator{}, auditapplication.SystemClock{},
	)
	if err != nil {
		_ = database.Close(db)
		_ = logFile.Close()
		return nil, err
	}

	applicationAccessAudit := &applicationAccessAuditAdapter{service: auditService, config: cfg.Audit, ids: ulid.Generator{}}
	// Application authorization catalogs are published by the owning subsystem through
	// its OAuth machine credential. Do not bootstrap a contract_management catalog here:
	// the platform only mirrors the directory and assigns published roles. The platform's
	// own catalog is bootstrapped below because the platform has no catalog-publisher
	// client for itself; without that mirror the UI blocks role assignment for built-in
	// platform roles.
	applicationAccessService, err := applicationaccess.NewService(db, ulid.Generator{}, applicationaccess.SystemClock{}, applicationAccessAudit)
	if err != nil {
		_ = database.Close(db)
		_ = logFile.Close()
		return nil, err
	}
	if err := bootstrapPlatformCatalog(applicationAccessService, db, logger); err != nil {
		_ = database.Close(db)
		_ = logFile.Close()
		return nil, err
	}
	applicationAccessHandler, err := applicationaccess.NewHandler(applicationAccessService)
	if err != nil {
		_ = database.Close(db)
		_ = logFile.Close()
		return nil, err
	}
	positionGrantService, err := positiongrant.NewService(db, ulid.Generator{}, positiongrant.SystemClock{})
	if err != nil {
		_ = database.Close(db)
		_ = logFile.Close()
		return nil, err
	}
	positionGrantHandler, err := positiongrant.NewHandler(positionGrantService)
	if err != nil {
		_ = database.Close(db)
		_ = logFile.Close()
		return nil, err
	}
	bootstrapService, err := identityapplication.NewBootstrapService(
		repository, security.Argon2idPasswordHasher{}, ulid.Generator{}, identityapplication.SystemClock{},
	)
	if err != nil {
		_ = database.Close(db)
		_ = logFile.Close()
		return nil, err
	}
	bootstrapHandler, err := identityhttp.NewBootstrapHandler(bootstrapService, logger, cfg.Identity.BootstrapToken, auditService, cfg.Audit)
	if err != nil {
		_ = database.Close(db)
		_ = logFile.Close()
		return nil, err
	}

	authService, err := identityapplication.NewService(
		repository, security.Argon2idPasswordVerifier{}, tokenManager, ulid.Generator{}, identityapplication.SystemClock{}, loginSecurityService, cfg.Auth.SessionTTL,
	)
	if err != nil {
		_ = database.Close(db)
		_ = logFile.Close()
		return nil, err
	}
	loginTargetRepository, err := applicationregistryinfrastructure.NewLoginTargetGORMRepository(db)
	if err != nil {
		_ = database.Close(db)
		_ = logFile.Close()
		return nil, err
	}
	loginTargetResolver, err := applicationregistryapplication.NewLoginTargetService(loginTargetRepository)
	if err != nil {
		_ = database.Close(db)
		_ = logFile.Close()
		return nil, err
	}
	authHandler, err := identityhttp.NewHandler(authService, logger, cfg.Auth, auditService, cfg.Audit, loginTargetResolver)
	if err != nil {
		_ = database.Close(db)
		_ = logFile.Close()
		return nil, err
	}
	accountLifecycleService, err := identityapplication.NewAccountLifecycleService(
		repository, security.Argon2idPasswordHasher{}, security.Argon2idPasswordVerifier{}, identityapplication.CryptoPasswordGenerator{}, ulid.Generator{}, identityapplication.SystemClock{},
	)
	if err != nil {
		_ = database.Close(db)
		_ = logFile.Close()
		return nil, err
	}
	accountLifecycleHandler, err := identityhttp.NewAccountLifecycleHandler(accountLifecycleService, authHandler, logger)
	if err != nil {
		_ = database.Close(db)
		_ = logFile.Close()
		return nil, err
	}

	auditHandler, err := audithttp.NewHandler(auditService, logger, cfg.FileStorageRoot)
	if err != nil {
		_ = database.Close(db)
		_ = logFile.Close()
		return nil, err
	}

	applicationRegistryRepository, err := applicationregistryinfrastructure.NewRepository(db)
	if err != nil {
		_ = database.Close(db)
		_ = logFile.Close()
		return nil, err
	}
	applicationRegistryService, err := applicationregistryapplication.NewService(
		applicationRegistryRepository, applicationTokenManager, applicationregistryapplication.SystemClock{},
	)
	if err != nil {
		_ = database.Close(db)
		_ = logFile.Close()
		return nil, err
	}
	applicationTokenHandler, err := applicationregistryhttp.NewHandler(applicationRegistryService, logger)
	if err != nil {
		_ = database.Close(db)
		_ = logFile.Close()
		return nil, err
	}

	oidcRepository, err := oidcinfrastructure.NewRepository(db)
	if err != nil {
		_ = database.Close(db)
		_ = logFile.Close()
		return nil, err
	}
	oidcAuthorizationResolver, err := applicationaccess.NewApplicationAuthorizationResolver(applicationAccessService)
	if err != nil {
		_ = database.Close(db)
		_ = logFile.Close()
		return nil, err
	}
	oidcIssuer, err := oidctokenissuer.New(
		oidcTokenManager, ulid.Generator{}, oidcAuthorizationResolver,
	)
	if err != nil {
		_ = database.Close(db)
		_ = logFile.Close()
		return nil, err
	}
	oidcService, err := oidcapplication.NewService(
		oidcRepository, oidcIssuer, ulid.Generator{}, oidcapplication.CryptographicSecretGenerator{}, oidcapplication.SystemClock{}, 5*time.Minute,
	)
	if err != nil {
		_ = database.Close(db)
		_ = logFile.Close()
		return nil, err
	}
	oidcAccessTokenSubjects, err := oidcaccesssubject.New(oidcRepository, oidcapplication.SystemClock{})
	if err != nil {
		_ = database.Close(db)
		_ = logFile.Close()
		return nil, err
	}
	oidcCookieSameSite, err := parseSameSite(cfg.Auth.SessionCookieSameSite)
	if err != nil {
		_ = database.Close(db)
		_ = logFile.Close()
		return nil, err
	}
	oidcHandler, err := oidchttp.NewHandler(oidchttp.Config{
		Service:                       oidcService,
		JWTManager:                    oidcTokenManager,
		LegacyClientCredentialsIssuer: applicationRegistryService,
		SessionAuthenticator:          authService,
		SessionLogout:                 authService,
		PostLogoutRedirectValidator:   oidcService,
		AccessTokenSubjectResolver:    oidcAccessTokenSubjects,
		AuthorizationResolver:         oidcAuthorizationResolver,
		SessionCookieName:             cfg.Auth.SessionCookieName,
		SessionCookieSecure:           cfg.Auth.SessionCookieSecure,
		SessionCookieSameSite:         oidcCookieSameSite,
		Logger:                        logger,
	})
	if err != nil {
		_ = database.Close(db)
		_ = logFile.Close()
		return nil, err
	}

	applicationManagementRepository, err := applicationregistryinfrastructure.NewManagementRepository(db)
	if err != nil {
		_ = database.Close(db)
		_ = logFile.Close()
		return nil, err
	}
	applicationManagementService, err := applicationregistryapplication.NewManagementService(
		applicationManagementRepository, ulid.Generator{}, applicationregistryapplication.SystemClock{},
	)
	if err != nil {
		_ = database.Close(db)
		_ = logFile.Close()
		return nil, err
	}
	applicationManagementHandler, err := applicationregistryhttp.NewManagementHandler(applicationManagementService, logger)
	if err != nil {
		_ = database.Close(db)
		_ = logFile.Close()
		return nil, err
	}

	oauthClientManagementRepository, err := applicationregistryinfrastructure.NewOAuthClientManagementRepository(db)
	if err != nil {
		_ = database.Close(db)
		_ = logFile.Close()
		return nil, err
	}
	oauthClientManagementService, err := applicationregistryapplication.NewOAuthClientManagementService(
		oauthClientManagementRepository, ulid.Generator{}, applicationregistryapplication.SystemClock{},
		applicationregistryapplication.RedirectURIValidationPolicy{
			AllowInsecureHTTP: cfg.Auth.OAuthClientAllowInsecureHTTPRedirectURIs,
		},
	)
	if err != nil {
		_ = database.Close(db)
		_ = logFile.Close()
		return nil, err
	}
	oauthClientManagementHandler, err := applicationregistryhttp.NewOAuthClientManagementHandler(oauthClientManagementService, logger)
	if err != nil {
		_ = database.Close(db)
		_ = logFile.Close()
		return nil, err
	}

	configurationRepository, err := configurationinfrastructure.NewRepository(db)
	if err != nil {
		_ = database.Close(db)
		_ = logFile.Close()
		return nil, err
	}
	configurationService, err := configurationapplication.NewService(
		configurationRepository, ulid.Generator{}, configurationapplication.SystemClock{},
	)
	if err != nil {
		_ = database.Close(db)
		_ = logFile.Close()
		return nil, err
	}
	configurationHandler, err := configurationhttp.NewHandler(configurationService, logger)
	if err != nil {
		_ = database.Close(db)
		_ = logFile.Close()
		return nil, err
	}

	settingsRepository, err := settingsinfrastructure.NewRepository(db)
	if err != nil {
		_ = database.Close(db)
		_ = logFile.Close()
		return nil, err
	}
	settingsService, err := settingsapplication.NewService(
		settingsRepository, ulid.Generator{}, settingsapplication.SystemClock{},
	)
	if err != nil {
		_ = database.Close(db)
		_ = logFile.Close()
		return nil, err
	}
	settingsHandler, err := settingshttp.NewHandler(settingsService, logger)
	if err != nil {
		_ = database.Close(db)
		_ = logFile.Close()
		return nil, err
	}

	dictionaryRepository, err := dictionaryinfrastructure.NewRepository(db)
	if err != nil {
		_ = database.Close(db)
		_ = logFile.Close()
		return nil, err
	}
	dictionaryService, err := dictionaryapplication.NewService(
		dictionaryRepository, ulid.Generator{}, dictionaryapplication.SystemClock{},
	)
	if err != nil {
		_ = database.Close(db)
		_ = logFile.Close()
		return nil, err
	}
	dictionaryHandler, err := dictionaryhttp.NewHandler(dictionaryService, logger)
	if err != nil {
		_ = database.Close(db)
		_ = logFile.Close()
		return nil, err
	}

	loginSecurityHandler, err := securityhttp.NewHandler(loginSecurityService, logger)
	if err != nil {
		_ = database.Close(db)
		_ = logFile.Close()
		return nil, err
	}

	operational, err := buildOperationalModules(cfg, db, logger, applicationAccessService)
	if err != nil {
		_ = database.Close(db)
		_ = logFile.Close()
		return nil, err
	}

	return &API{
		Handler: httptransport.NewRouter(
			cfg, logger, db, authHandler, bootstrapHandler, managementHandler, accountLifecycleHandler, authorizationHandler, applicationAccessHandler, positionGrantHandler, auditHandler, configurationHandler, settingsHandler, dictionaryHandler, loginSecurityHandler, applicationTokenHandler, applicationManagementHandler, oauthClientManagementHandler, applicationRegistryService, auditService, oidcHandler, operational,
		),
		Logger:   logger,
		database: db,
		logFile:  logFile,
	}, nil
}

// bootstrapPlatformCatalog ensures the platform application's own authorization catalog row
// reflects the migration-seeded built-in roles and permissions. Subsystem-owned catalogs
// (e.g. contract_management) are published by their catalog-publisher OAuth client and are
// intentionally not touched here. The platform has no such client for itself; without this
// bootstrap the UI blocks role assignment with "目录同步: 未同步" even though the
// built-in data is already in the database.
func bootstrapPlatformCatalog(
	svc *applicationaccess.Service,
	db *gorm.DB,
	logger *slog.Logger,
) error {
	if svc == nil || db == nil || logger == nil {
		return errors.New("bootstrap platform catalog dependencies must not be nil")
	}
	tenantID, err := resolveDefaultTenantID(db)
	if err != nil {
		return fmt.Errorf("resolve default tenant for platform catalog bootstrap: %w", err)
	}
	operatorID, err := resolvePlatformCatalogOperatorID(db, tenantID)
	if err != nil {
		return fmt.Errorf("resolve platform catalog bootstrap operator: %w", err)
	}
	bootstrapCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	view, err := svc.EnsurePlatformCatalogSynced(bootstrapCtx, tenantID, operatorID)
	if err != nil {
		return fmt.Errorf("ensure platform catalog synced: %w", err)
	}
	logger.Info(
		"platform authorization catalog bootstrapped",
		"application_code", applicationaccess.PlatformApplicationCode,
		"catalog_version", view.CatalogVersion,
		"checksum", view.Checksum,
		"role_count", len(view.Roles),
		"permission_count", len(view.Permissions),
		"operator_id", operatorID,
	)
	return nil
}

// resolveDefaultTenantID looks up the migration-seeded "default" tenant. The bootstrap path
// must not hard-code the tenant ULID; future multi-tenant installs will share the same code.
func resolveDefaultTenantID(db *gorm.DB) (string, error) {
	var tenantID string
	err := db.WithContext(context.Background()).
		Table("iam_tenant").
		Where("code = ? AND status = ?", "default", "ACTIVE").
		Limit(1).
		Pluck("id", &tenantID).Error
	if err != nil {
		return "", err
	}
	if strings.TrimSpace(tenantID) == "" {
		return "", errors.New("default tenant not found")
	}
	return tenantID, nil
}

// resolvePlatformCatalogOperatorID prefers the first super administrator's user id so the
// audit history aligns with whoever initially set the platform up. If bootstrap has not yet
// happened, a stable system identity is used so the catalog row still has a valid 26-char
// last_synced_by reference until the first admin takes over.
func resolvePlatformCatalogOperatorID(db *gorm.DB, tenantID string) (string, error) {
	// application code "platform" + role code "platform-super-admin" are migration-seeded by
	// 000011_seed_platform_defaults.sql; look them up dynamically so this bootstrap tolerates
	// future schema renames.
	var platformAppID string
	if err := db.WithContext(context.Background()).
		Table("platform_application").
		Where("tenant_id = ? AND code = ? AND status = ?", tenantID, applicationaccess.PlatformApplicationCode, "ACTIVE").
		Limit(1).
		Pluck("id", &platformAppID).Error; err != nil {
		return "", err
	}
	if strings.TrimSpace(platformAppID) == "" {
		return "", errors.New("platform application not found")
	}
	var adminUserID string
	err := db.WithContext(context.Background()).
		Table("iam_user AS u").
		Where("u.tenant_id = ? AND u.status = ? AND u.employment_status = ?", tenantID, "ACTIVE", "ACTIVE").
		Where("EXISTS (SELECT 1 FROM authz_role_binding rb JOIN authz_role r ON r.id = rb.role_id WHERE rb.subject_type = 'USER' AND rb.subject_id = u.id AND rb.application_id = ? AND r.code = ? AND rb.status = 'ACTIVE')",
			platformAppID, applicationaccess.BootstrapSuperAdminRoleCode).
		Order("u.created_at ASC").
		Limit(1).
		Pluck("u.id", &adminUserID).Error
	if err != nil {
		return "", err
	}
	if strings.TrimSpace(adminUserID) != "" {
		return adminUserID, nil
	}
	// Pre-bootstrap: use a stable system identity until the first admin takes over. The
	// 26-char placeholder is a valid Crockford Base32 ULID with a non-overlapping "PLATFSY"
	// suffix so it can never collide with a real user id.
	return applicationaccess.PlatformCatalogBootstrapOperatorID, nil
}

// Close releases process-owned resources. It is safe to defer after a successful NewAPI call.
func (api *API) Close() {
	if api.database != nil {
		if err := database.Close(api.database); err != nil {
			api.Logger.Error("close mysql database handle", "error", err)
		}
	}
	if api.logFile != nil {
		if err := api.logFile.Close(); err != nil {
			api.Logger.Error("close application log file", "error", err)
		}
	}
}

// parseSameSite converts the validated configuration value into Go's cookie enum for OIDC logout.
func parseSameSite(value string) (http.SameSite, error) {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "lax":
		return http.SameSiteLaxMode, nil
	case "strict":
		return http.SameSiteStrictMode, nil
	case "none":
		return http.SameSiteNoneMode, nil
	default:
		return http.SameSiteDefaultMode, fmt.Errorf("unsupported session cookie SameSite value %q", value)
	}
}
