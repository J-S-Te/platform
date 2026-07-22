// Package bootstrap wires infrastructure dependencies into runnable processes.
package bootstrap

import (
	"context"
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
	authorizationinfrastructure "github.com/J-S-Te/Basic-Platform/backend/internal/platform/authorization/infrastructure"
	authorizationhttp "github.com/J-S-Te/Basic-Platform/backend/internal/platform/authorization/interfaces/http"
	configurationapplication "github.com/J-S-Te/Basic-Platform/backend/internal/platform/configuration/application"
	configurationinfrastructure "github.com/J-S-Te/Basic-Platform/backend/internal/platform/configuration/infrastructure"
	configurationhttp "github.com/J-S-Te/Basic-Platform/backend/internal/platform/configuration/interfaces/http"
	dictionaryapplication "github.com/J-S-Te/Basic-Platform/backend/internal/platform/dictionary/application"
	dictionaryinfrastructure "github.com/J-S-Te/Basic-Platform/backend/internal/platform/dictionary/infrastructure"
	dictionaryhttp "github.com/J-S-Te/Basic-Platform/backend/internal/platform/dictionary/interfaces/http"
	identityapplication "github.com/J-S-Te/Basic-Platform/backend/internal/platform/identity/application"
	federationapplication "github.com/J-S-Te/Basic-Platform/backend/internal/platform/identity/federation/application"
	federationinfrastructure "github.com/J-S-Te/Basic-Platform/backend/internal/platform/identity/federation/infrastructure"
	federationhttp "github.com/J-S-Te/Basic-Platform/backend/internal/platform/identity/federation/interfaces/http"
	"github.com/J-S-Te/Basic-Platform/backend/internal/platform/identity/infrastructure"
	identityhttp "github.com/J-S-Te/Basic-Platform/backend/internal/platform/identity/interfaces/http"
	mfaapplication "github.com/J-S-Te/Basic-Platform/backend/internal/platform/identity/mfa/application"
	mfainfrastructure "github.com/J-S-Te/Basic-Platform/backend/internal/platform/identity/mfa/infrastructure"
	mfahttp "github.com/J-S-Te/Basic-Platform/backend/internal/platform/identity/mfa/interfaces/http"
	oidcapplication "github.com/J-S-Te/Basic-Platform/backend/internal/platform/oidc/application"
	oidcinfrastructure "github.com/J-S-Te/Basic-Platform/backend/internal/platform/oidc/infrastructure"
	oidcaccesssubject "github.com/J-S-Te/Basic-Platform/backend/internal/platform/oidc/interfaces/accesssubject"
	oidchttp "github.com/J-S-Te/Basic-Platform/backend/internal/platform/oidc/interfaces/http"
	oidctokenissuer "github.com/J-S-Te/Basic-Platform/backend/internal/platform/oidc/interfaces/tokenissuer"
	securityapplication "github.com/J-S-Te/Basic-Platform/backend/internal/platform/security/application"
	securityinfrastructure "github.com/J-S-Te/Basic-Platform/backend/internal/platform/security/infrastructure"
	securityhttp "github.com/J-S-Te/Basic-Platform/backend/internal/platform/security/interfaces/http"
	mfastepupapplication "github.com/J-S-Te/Basic-Platform/backend/internal/platform/security/mfastepup/application"
	mfastepupinfrastructure "github.com/J-S-Te/Basic-Platform/backend/internal/platform/security/mfastepup/infrastructure"
	mfastepuphttp "github.com/J-S-Te/Basic-Platform/backend/internal/platform/security/mfastepup/interfaces/http"
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

	database      *gorm.DB
	logFile       io.Closer
	runtimeCancel context.CancelFunc
	runtimeDone   chan struct{}
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
		loginSecurityRepository, ulid.Generator{}, securityapplication.SystemClock{},
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
		repository, mobileProtector, ulid.Generator{}, identityapplication.SystemClock{},
	)
	if err != nil {
		_ = database.Close(db)
		_ = logFile.Close()
		return nil, err
	}
	managementHandler, err := identityhttp.NewManagementHandler(managementService, logger)
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

	federationRepository, err := federationinfrastructure.NewRepository(db)
	if err != nil {
		_ = database.Close(db)
		_ = logFile.Close()
		return nil, err
	}
	var federatedProviderSecretProtector *security.EnvelopeProtector
	if strings.TrimSpace(cfg.Identity.FederatedProviderSecretEncryptionKey) != "" {
		federatedProviderSecretProtector, err = security.NewEnvelopeProtector(
			cfg.Identity.FederatedProviderSecretEncryptionKey,
			"IAM_FEDERATED_PROVIDER_SECRET_ENCRYPTION_KEY",
		)
		if err != nil {
			_ = database.Close(db)
			_ = logFile.Close()
			return nil, err
		}
	}
	var federationService *federationapplication.Service
	if federatedProviderSecretProtector == nil {
		federationService, err = federationapplication.NewService(federationRepository, ulid.Generator{}, federationapplication.SystemClock{})
	} else {
		federationService, err = federationapplication.NewService(
			federationRepository, ulid.Generator{}, federationapplication.SystemClock{}, federatedProviderSecretProtector,
		)
	}
	if err != nil {
		_ = database.Close(db)
		_ = logFile.Close()
		return nil, err
	}
	federationHandler, err := federationhttp.NewHandler(federationService, logger)
	if err != nil {
		_ = database.Close(db)
		_ = logFile.Close()
		return nil, err
	}

	mfaRepository, err := mfainfrastructure.NewRepository(db)
	if err != nil {
		_ = database.Close(db)
		_ = logFile.Close()
		return nil, err
	}
	mfaProtector, err := security.NewEnvelopeProtector(cfg.Identity.MFAEncryptionKey, "IAM_MFA_ENCRYPTION_KEY")
	if err != nil {
		_ = database.Close(db)
		_ = logFile.Close()
		return nil, err
	}
	mfaService, err := mfaapplication.NewService(mfaRepository, mfaProtector, ulid.Generator{}, mfaapplication.SystemClock{}, mfaapplication.Config{
		Issuer: cfg.Auth.OIDCIssuer, EnrollmentTTL: 5 * time.Minute, FactorTTL: 365 * 24 * time.Hour,
		ChallengeTTL: 5 * time.Minute, MaxChallengeAttempts: 5, RecoveryCodeCount: 10,
	})
	if err != nil {
		_ = database.Close(db)
		_ = logFile.Close()
		return nil, err
	}
	mfaHandler, err := mfahttp.NewHandler(mfaService, logger)
	if err != nil {
		_ = database.Close(db)
		_ = logFile.Close()
		return nil, err
	}

	// Password authentication depends on the MFA service so an enrolled account cannot receive a
	// browser session before it completes the second factor.
	authService, err := identityapplication.NewService(
		repository, security.Argon2idPasswordVerifier{}, tokenManager, ulid.Generator{}, identityapplication.SystemClock{}, loginSecurityService, cfg.Auth.SessionTTL, mfaService,
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
	externalLoginHandler, err := buildExternalLoginHandler(
		cfg, db, authService, federatedProviderSecretProtector, logger, auditService,
	)
	if err != nil {
		_ = database.Close(db)
		_ = logFile.Close()
		return nil, err
	}
	dingTalkLoginHandler, err := buildDingTalkLoginHandler(
		cfg, db, authService, federatedProviderSecretProtector, logger, auditService,
	)
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

	mfaStepUpRepository, err := mfastepupinfrastructure.NewRepository(db)
	if err != nil {
		_ = database.Close(db)
		_ = logFile.Close()
		return nil, err
	}
	mfaStepUpService, err := mfastepupapplication.NewService(
		mfaStepUpRepository, mfaService, ulid.Generator{}, mfastepupapplication.SystemClock{},
		mfastepupapplication.Config{GrantTTL: 2 * time.Minute},
	)
	if err != nil {
		_ = database.Close(db)
		_ = logFile.Close()
		return nil, err
	}
	mfaStepUpHandler, err := mfastepuphttp.NewHandler(mfaStepUpService, logger)
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
	oidcIssuer, err := oidctokenissuer.New(oidcTokenManager, ulid.Generator{})
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

	operational, err := buildOperationalModules(cfg, db, logger, auditService, auditRepository)
	if err != nil {
		_ = database.Close(db)
		_ = logFile.Close()
		return nil, err
	}
	runtimeContext, runtimeCancel := context.WithCancel(context.Background())
	runtimeDone := make(chan struct{})
	go func() {
		defer close(runtimeDone)
		operational.alertRunner.Run(runtimeContext)
	}()

	return &API{
		Handler: httptransport.NewRouter(
			cfg, logger, db, authHandler, externalLoginHandler, dingTalkLoginHandler, bootstrapHandler, managementHandler, accountLifecycleHandler, authorizationHandler, auditHandler, configurationHandler, settingsHandler, dictionaryHandler, loginSecurityHandler, applicationTokenHandler, applicationManagementHandler, oauthClientManagementHandler, applicationRegistryService, auditService, oidcHandler, federationHandler, mfaHandler, mfaStepUpHandler, mfaStepUpService, operational.modules,
		),
		Logger:        logger,
		database:      db,
		logFile:       logFile,
		runtimeCancel: runtimeCancel,
		runtimeDone:   runtimeDone,
	}, nil
}

// Close releases process-owned resources. It is safe to defer after a successful NewAPI call.
func (api *API) Close() {
	if api.runtimeCancel != nil {
		api.runtimeCancel()
	}
	if api.runtimeDone != nil {
		<-api.runtimeDone
	}
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
