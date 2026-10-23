// Package bootstrap 把基础设施依赖装配为可运行的进程。
package bootstrap

import (
	"context"
	"crypto/ed25519"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"time"

	applicationregistryapplication "github.com/J-S-Te/Basic-Platform/internal/platform/applicationregistry/application"
	applicationregistryinfrastructure "github.com/J-S-Te/Basic-Platform/internal/platform/applicationregistry/infrastructure"
	applicationregistryhttp "github.com/J-S-Te/Basic-Platform/internal/platform/applicationregistry/interfaces/http"
	auditapplication "github.com/J-S-Te/Basic-Platform/internal/platform/audit/application"
	auditinfrastructure "github.com/J-S-Te/Basic-Platform/internal/platform/audit/infrastructure"
	audithttp "github.com/J-S-Te/Basic-Platform/internal/platform/audit/interfaces/http"
	authorizationapplication "github.com/J-S-Te/Basic-Platform/internal/platform/authorization/application"
	applicationaccess "github.com/J-S-Te/Basic-Platform/internal/platform/authorization/applicationaccess"
	authorizationinfrastructure "github.com/J-S-Te/Basic-Platform/internal/platform/authorization/infrastructure"
	authorizationhttp "github.com/J-S-Te/Basic-Platform/internal/platform/authorization/interfaces/http"
	managementscope "github.com/J-S-Te/Basic-Platform/internal/platform/authorization/managementscope"
	positiongrant "github.com/J-S-Te/Basic-Platform/internal/platform/authorization/positiongrant"
	configurationapplication "github.com/J-S-Te/Basic-Platform/internal/platform/configuration/application"
	configurationinfrastructure "github.com/J-S-Te/Basic-Platform/internal/platform/configuration/infrastructure"
	configurationhttp "github.com/J-S-Te/Basic-Platform/internal/platform/configuration/interfaces/http"
	dictionaryapplication "github.com/J-S-Te/Basic-Platform/internal/platform/dictionary/application"
	dictionaryinfrastructure "github.com/J-S-Te/Basic-Platform/internal/platform/dictionary/infrastructure"
	dictionaryhttp "github.com/J-S-Te/Basic-Platform/internal/platform/dictionary/interfaces/http"
	externalidentityapplication "github.com/J-S-Te/Basic-Platform/internal/platform/externalidentity/application"
	externalidentityinfrastructure "github.com/J-S-Te/Basic-Platform/internal/platform/externalidentity/infrastructure"
	externalidentityhttp "github.com/J-S-Te/Basic-Platform/internal/platform/externalidentity/interfaces/http"
	identityapplication "github.com/J-S-Te/Basic-Platform/internal/platform/identity/application"
	"github.com/J-S-Te/Basic-Platform/internal/platform/identity/infrastructure"
	identityhttp "github.com/J-S-Te/Basic-Platform/internal/platform/identity/interfaces/http"
	keycloakauthorizationinfrastructure "github.com/J-S-Te/Basic-Platform/internal/platform/keycloakauthorization/infrastructure"
	licenseapplication "github.com/J-S-Te/Basic-Platform/internal/platform/license/application"
	licensecoordination "github.com/J-S-Te/Basic-Platform/internal/platform/license/coordination"
	licensedomain "github.com/J-S-Te/Basic-Platform/internal/platform/license/domain"
	licenseinfrastructure "github.com/J-S-Te/Basic-Platform/internal/platform/license/infrastructure"
	licensehttp "github.com/J-S-Te/Basic-Platform/internal/platform/license/interfaces/http"
	licensetrust "github.com/J-S-Te/Basic-Platform/internal/platform/license/trust"
	oidcapplication "github.com/J-S-Te/Basic-Platform/internal/platform/oidc/application"
	backchannel "github.com/J-S-Te/Basic-Platform/internal/platform/oidc/backchannel"
	oidcinfrastructure "github.com/J-S-Te/Basic-Platform/internal/platform/oidc/infrastructure"
	oidcaccesssubject "github.com/J-S-Te/Basic-Platform/internal/platform/oidc/interfaces/accesssubject"
	oidchttp "github.com/J-S-Te/Basic-Platform/internal/platform/oidc/interfaces/http"
	oidcpersonneldirectory "github.com/J-S-Te/Basic-Platform/internal/platform/oidc/interfaces/personneldirectory"
	oidctokenissuer "github.com/J-S-Te/Basic-Platform/internal/platform/oidc/interfaces/tokenissuer"
	ownerdirectoryapplication "github.com/J-S-Te/Basic-Platform/internal/platform/ownerdirectory/application"
	ownerdirectoryinfrastructure "github.com/J-S-Te/Basic-Platform/internal/platform/ownerdirectory/infrastructure"
	ownerdirectoryhttp "github.com/J-S-Te/Basic-Platform/internal/platform/ownerdirectory/interfaces/http"
	securityapplication "github.com/J-S-Te/Basic-Platform/internal/platform/security/application"
	securityinfrastructure "github.com/J-S-Te/Basic-Platform/internal/platform/security/infrastructure"
	securityhttp "github.com/J-S-Te/Basic-Platform/internal/platform/security/interfaces/http"
	settingsapplication "github.com/J-S-Te/Basic-Platform/internal/platform/settings/application"
	settingsinfrastructure "github.com/J-S-Te/Basic-Platform/internal/platform/settings/infrastructure"
	settingshttp "github.com/J-S-Te/Basic-Platform/internal/platform/settings/interfaces/http"
	"github.com/J-S-Te/Basic-Platform/internal/shared/config"
	"github.com/J-S-Te/Basic-Platform/internal/shared/database"
	"github.com/J-S-Te/Basic-Platform/internal/shared/observability"
	"github.com/J-S-Te/Basic-Platform/internal/shared/security"
	"github.com/J-S-Te/Basic-Platform/internal/shared/ulid"
	httptransport "github.com/J-S-Te/Basic-Platform/internal/transport/http"
	"github.com/J-S-Te/Basic-Platform/internal/transport/http/middleware"
	licenseruntime "github.com/J-S-Te/license-core/runtime"
	"gorm.io/gorm"
)

// API 是 HTTP 进程的依赖容器，保存服务路由、日志器与资源句柄。
type API struct {
	Handler         http.Handler
	Logger          *slog.Logger
	OfflineDelivery *OfflineDeliveryRunner

	database           *gorm.DB
	oidcDatabase       *gorm.DB
	logFile            io.Closer
	logoutWorkerCancel context.CancelFunc
	logoutWorkerDone   chan struct{}
}

// NewAPI 按启动配置创建本地存储目录、结构化日志、数据库连接池与 HTTP 路由。
// 参数 cfg 为完整应用配置；返回值是装配完成的 API 运行时对象；若任一依赖初始化失败则返回错误并释放已分配资源。
// 该初始化阶段不做数据库 ping：/readyz 负责反馈依赖状态，/healthz 始终对进程活性负责。
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

	// 三类 JWT 共用签名密钥但隔离 issuer/audience 与用途：平台会话令牌不能充当应用
	// 机器令牌，应用机器令牌也不能冒充 OIDC ID/Access Token。各验证器必须只接受
	// 自己的受众，不能因为密钥相同而跨协议复用。
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
	customerRefProtector, err := security.NewCustomerRefProtector(cfg.Identity.CustomerRefEncryptionKey, cfg.Identity.CustomerRefDigestKey)
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
	if strings.TrimSpace(cfg.IAMImportFileGateway.BaseURL) != "" {
		gateway, gatewayErr := infrastructure.NewEmployeeImportFileGateway(
			cfg.IAMImportFileGateway.BaseURL, cfg.IAMImportFileGateway.TokenURL,
			cfg.IAMImportFileGateway.ClientID, cfg.IAMImportFileGateway.ClientSecret,
			cfg.IAMImportFileGateway.Scope, &http.Client{Timeout: 30 * time.Second},
		)
		if gatewayErr != nil {
			_ = database.Close(db)
			_ = logFile.Close()
			return nil, gatewayErr
		}
		managementHandler.ConfigureEmployeeImportGateway(gateway)
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
	externalIdentityRepository, err := externalidentityinfrastructure.NewGORMRepository(db, auditService)
	if err != nil {
		_ = database.Close(db)
		_ = logFile.Close()
		return nil, err
	}
	externalIdentityService, err := externalidentityapplication.NewService(externalIdentityRepository, mobileProtector, ulid.Generator{}, externalidentityapplication.SystemClock{},
		externalidentityapplication.WithPortalApplicationCode(cfg.PortalApplicationCode),
		externalidentityapplication.WithCustomerRefProtection(customerRefProtector),
		externalidentityapplication.WithCustomerPortalInitialPassword(cfg.Identity.CustomerPortalInitialPassword),
	)
	if err != nil {
		_ = database.Close(db)
		_ = logFile.Close()
		return nil, err
	}
	externalIdentityHandler, err := externalidentityhttp.NewHandler(externalIdentityService, logger)
	if err != nil {
		_ = database.Close(db)
		_ = logFile.Close()
		return nil, err
	}
	ownerDirectoryRepository, err := ownerdirectoryinfrastructure.NewRepository(db)
	if err != nil {
		_ = database.Close(db)
		_ = logFile.Close()
		return nil, err
	}
	ownerDirectoryService, err := ownerdirectoryapplication.NewService(ownerDirectoryRepository)
	if err != nil {
		_ = database.Close(db)
		_ = logFile.Close()
		return nil, err
	}
	ownerDirectoryHandler, err := ownerdirectoryhttp.NewHandler(ownerDirectoryService, logger)
	if err != nil {
		_ = database.Close(db)
		_ = logFile.Close()
		return nil, err
	}

	applicationAccessAudit := &applicationAccessAuditAdapter{service: auditService, config: cfg.Audit, ids: ulid.Generator{}}
	// 子系统通过各自的目录发布机器凭据维护授权目录，平台只保存镜像并据此分配应用角色，
	// 因此这里不能替合同等子系统擅自初始化目录。平台自身没有目录发布客户端，必须在启动
	// 时同步迁移预置的角色和权限，否则前端无法把平台内置角色用于授权。
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

	var keycloakAdmin *keycloakauthorizationinfrastructure.KeycloakAdmin
	if cfg.Keycloak.Enabled {
		keycloakAdmin, err = keycloakauthorizationinfrastructure.NewKeycloakAdminWithCredentials(cfg.Keycloak.AdminURL, cfg.Keycloak.Realm, keycloakauthorizationinfrastructure.KeycloakAdminCredentials{
			ServiceAccountClientID:     cfg.Keycloak.AdminClientID,
			ServiceAccountClientSecret: cfg.Keycloak.AdminClientSecret,
			Username:                   cfg.Keycloak.AdminUsername,
			Password:                   cfg.Keycloak.AdminPassword,
		})
		if err != nil {
			_ = database.Close(db)
			_ = logFile.Close()
			return nil, fmt.Errorf("create Keycloak session terminator: %w", err)
		}
		managementService.SetExternalAccountStatusReader(keycloakAdmin)
		managementService.SetExternalAccountStatusSynchronizer(repository)
	}
	authServiceDependencies := []identityapplication.ExternalSessionTerminator{}
	if keycloakAdmin != nil {
		authServiceDependencies = append(authServiceDependencies, keycloakAdmin)
	}
	authService, err := identityapplication.NewService(
		repository, security.Argon2idPasswordVerifier{}, tokenManager, ulid.Generator{}, identityapplication.SystemClock{}, loginSecurityService, cfg.Auth.SessionTTL, authServiceDependencies...,
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
	var authHandlerOptions []identityhttp.IDTokenVerifierOption
	if cfg.Auth.KeycloakOIDCEnabled && cfg.Keycloak.Enabled {
		// P1-2：平台自身 OIDC 登录校验 ID Token（签名/issuer/aud/exp/nonce/azp）。
		// 复用与 broker 验证同源的 JWKS 验证器，验证面不扩大。
		idTokenVerifier, verifierErr := middleware.NewKeycloakBrokerJWTVerifier(cfg.Auth.KeycloakOIDCIssuer, nil)
		if verifierErr != nil {
			_ = database.Close(db)
			_ = logFile.Close()
			return nil, fmt.Errorf("configure platform OIDC ID token verifier: %w", verifierErr)
		}
		authHandlerOptions = append(authHandlerOptions, identityhttp.WithIDTokenVerifier(oidcIDTokenVerifierAdapter{verifier: idTokenVerifier}))
	}
	authHandler, err := identityhttp.NewHandler(authService, logger, cfg.Auth, auditService, cfg.Audit, loginTargetResolver, authHandlerOptions...)
	if err != nil {
		_ = database.Close(db)
		_ = logFile.Close()
		return nil, err
	}
	var externalResetter identityapplication.ExternalPasswordResetter
	if keycloakAdmin != nil {
		externalResetter = keycloakAdmin
	}
	accountLifecycleService, err := identityapplication.NewAccountLifecycleService(
		repository, security.Argon2idPasswordHasher{}, security.Argon2idPasswordVerifier{}, identityapplication.CryptoPasswordGenerator{}, ulid.Generator{}, identityapplication.SystemClock{}, externalResetter,
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

	// 延迟敏感的 broker token 交换与共享的管理/审计连接池隔离。普通平台流量的
	// 突发或卡住的事务，绝不能拖累 Keycloak 等待其五秒回调 deadline。
	oidcDB, err := database.OpenMySQLWithPool(cfg.MySQL, 10, 5, 15*time.Minute, 2*time.Minute)
	if err != nil {
		_ = database.Close(db)
		_ = logFile.Close()
		return nil, fmt.Errorf("open OIDC database pool: %w", err)
	}
	oidcRepository, err := oidcinfrastructure.NewRepository(oidcDB)
	if err != nil {
		_ = database.Close(oidcDB)
		_ = database.Close(db)
		_ = logFile.Close()
		return nil, err
	}
	oidcAuthorizationResolver, err := applicationaccess.NewApplicationAuthorizationResolver(applicationAccessService)
	if err != nil {
		_ = database.Close(oidcDB)
		_ = database.Close(db)
		_ = logFile.Close()
		return nil, err
	}
	oidcIssuer, err := oidctokenissuer.New(
		oidcTokenManager, ulid.Generator{}, oidcAuthorizationResolver,
	)
	if err != nil {
		_ = database.Close(oidcDB)
		_ = database.Close(db)
		_ = logFile.Close()
		return nil, err
	}
	oidcService, err := oidcapplication.NewService(
		oidcRepository, oidcIssuer, ulid.Generator{}, oidcapplication.CryptographicSecretGenerator{}, oidcapplication.SystemClock{}, 5*time.Minute,
	)
	if err != nil {
		_ = database.Close(oidcDB)
		_ = database.Close(db)
		_ = logFile.Close()
		return nil, err
	}
	oidcAccessTokenSubjects, err := oidcaccesssubject.New(oidcRepository, oidcapplication.SystemClock{})
	if err != nil {
		_ = database.Close(oidcDB)
		_ = database.Close(db)
		_ = logFile.Close()
		return nil, err
	}
	oidcPersonnelDirectory, err := oidcpersonneldirectory.New(db)
	if err != nil {
		_ = database.Close(oidcDB)
		_ = database.Close(db)
		_ = logFile.Close()
		return nil, err
	}
	var externalAuthorizationVerifier oidchttp.ExternalAuthorizationTokenVerifier
	if cfg.Keycloak.Enabled {
		externalAuthorizationVerifier, err = newKeycloakAuthorizationVerifier(cfg.Keycloak.PublicURL, cfg.Keycloak.AdminURL, cfg.Keycloak.Realm)
		if err != nil {
			_ = database.Close(db)
			_ = logFile.Close()
			return nil, fmt.Errorf("configure Keycloak authorization-context verifier: %w", err)
		}
	}
	oidcCookieSameSite, err := parseSameSite(cfg.Auth.SessionCookieSameSite)
	if err != nil {
		_ = database.Close(oidcDB)
		_ = database.Close(db)
		_ = logFile.Close()
		return nil, err
	}
	oidcHandler, err := oidchttp.NewHandler(oidchttp.Config{
		Service:                        oidcService,
		JWTManager:                     oidcTokenManager,
		LegacyClientCredentialsIssuer:  applicationRegistryService,
		SessionAuthenticator:           authService,
		SessionLogout:                  authService,
		PostLogoutRedirectValidator:    oidcService,
		AccessTokenSubjectResolver:     oidcAccessTokenSubjects,
		ExternalAuthorizationVerifier:  externalAuthorizationVerifier,
		AuthorizationResolver:          oidcAuthorizationResolver,
		AuthorizationContextResolver:   oidcAuthorizationResolver,
		CustomerBindingResolver:        customerBindingResolverAdapter{service: externalIdentityService},
		EmitCustomerRef:                cfg.Auth.AuthzContextCustomerRefEnabled,
		PersonnelDirectoryResolver:     oidcPersonnelDirectory,
		AllowLegacyPlatformAccessToken: cfg.Auth.AllowLegacyPlatformAccessToken,
		SessionCookieName:              cfg.Auth.SessionCookieName,
		SessionCookieSecure:            cfg.Auth.SessionCookieSecure,
		SessionCookieSameSite:          oidcCookieSameSite,
		Logger:                         logger,
	})
	if err != nil {
		_ = database.Close(oidcDB)
		_ = database.Close(db)
		_ = logFile.Close()
		return nil, err
	}

	applicationManagementRepository, err := applicationregistryinfrastructure.NewManagementRepository(db)
	if err != nil {
		_ = database.Close(oidcDB)
		_ = database.Close(db)
		_ = logFile.Close()
		return nil, err
	}
	applicationManagementService, err := applicationregistryapplication.NewManagementService(
		applicationManagementRepository, ulid.Generator{}, applicationregistryapplication.SystemClock{},
	)
	if err != nil {
		_ = database.Close(oidcDB)
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
	backchannelURIRepository, err := applicationregistryinfrastructure.NewBackchannelLogoutURIRepository(db)
	if err != nil {
		_ = database.Close(db)
		_ = logFile.Close()
		return nil, err
	}
	backchannelURIHandler := applicationregistryhttp.BackchannelLogoutURIHandler{Repository: backchannelURIRepository}
	backchannelDispatcher := &backchannel.Dispatcher{
		Queue: &backchannel.GORMRepository{DB: db, Lease: 30 * time.Second, MaxAttempts: 8},
		Sender: backchannel.HTTPFormSender{
			Signer: oidcLogoutSignerAdapter{manager: oidcTokenManager},
			Issuer: cfg.Auth.OIDCIssuer,
			TTL:    time.Minute,
		},
		BatchSize:  50,
		RetryDelay: time.Second,
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

	// Broker 自愈：Keycloak 持有的 broker client secret 与平台活跃凭据漂移时（token 交换
	// 返回 invalid_client），自动轮换平台 secret 并同步回 Keycloak IdP。仅在 Keycloak 启用
	// 且具备管理凭据时装配；nil 时 token 端点对漂移保持失败关闭。
	if cfg.Keycloak.Enabled {
		tenantID, tenantErr := resolveDefaultTenantID(db)
		if tenantErr != nil {
			_ = database.Close(db)
			_ = logFile.Close()
			return nil, fmt.Errorf("resolve tenant for broker self-heal: %w", tenantErr)
		}
		brokerControl := applicationregistryhttp.NewKeycloakControlPlaneWithCredentials(
			cfg.Keycloak.AdminURL, cfg.Keycloak.Realm,
			applicationregistryhttp.KeycloakControlPlaneCredentials{
				ServiceAccountClientID:     cfg.Keycloak.AdminClientID,
				ServiceAccountClientSecret: cfg.Keycloak.AdminClientSecret,
				Username:                   cfg.Keycloak.AdminUsername,
				Password:                   cfg.Keycloak.AdminPassword,
			},
			cfg.Keycloak.BrokerClientID, cfg.Keycloak.BrokerClientSecret,
			cfg.Auth.OIDCIssuer, cfg.Keycloak.PlatformBackchannelURL,
		)
		oidcHandler.SetBrokerDriftSelfHealer(applicationregistryhttp.NewBrokerDriftSelfHealer(oauthClientManagementService, brokerControl, tenantID, logger))
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
	// 设置服务先以空部署器完成纯配置装配；部署 Agent 属于后面构造的运行模块，待其就绪
	// 后再重建处理器并注入 AccessApplier，避免设置领域反向依赖部署基础设施。
	settingsHandler, err := settingshttp.NewHandler(settingsService, nil, logger)
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

	personnelChangeAudit := &personnelChangeAuditAdapter{service: auditService, config: cfg.Audit, ids: ulid.Generator{}}
	// 授权协调服务在下方才构造；交接 inventory 通过延迟解析读取端，未就绪时按不可达阻断。
	var handoverLicenseReader handoverLicenseStatusReader
	personnelHandover, err := buildHandoverCoordinator(cfg, db, applicationManagementService, lazyHandoverLicenseReader{resolve: func() handoverLicenseStatusReader { return handoverLicenseReader }})
	if err != nil {
		_ = database.Close(db)
		_ = logFile.Close()
		return nil, err
	}
	operational, err := buildOperationalModules(cfg, db, logger, applicationAccessService, applicationManagementService, oauthClientManagementService, personnelChangeAudit, personnelHandover)
	if err != nil {
		_ = database.Close(db)
		_ = logFile.Close()
		return nil, err
	}
	// 部署 Agent 可选：未启用时设置仍可保存，但“应用配置”会失败关闭；启用时只把受限的
	// 对外访问操作接口注入处理器，不向普通设置逻辑暴露 Docker 或宿主机权限。
	settingsHandler, err = settingshttp.NewHandler(settingsService, operational.AccessApplier, logger)
	if err != nil {
		_ = database.Close(db)
		_ = logFile.Close()
		return nil, err
	}
	operational.ExternalIdentity = externalIdentityHandler
	operational.OwnerDirectory = ownerDirectoryHandler
	operational.BackchannelLogoutURI = &backchannelURIHandler
	licenseRepository, err := licenseinfrastructure.NewRepository(db)
	if err != nil {
		_ = database.Close(db)
		_ = logFile.Close()
		return nil, err
	}
	licenseService, err := licenseapplication.NewService(licenseRepository, licensetrust.Keys(), time.Now)
	if err != nil {
		_ = database.Close(db)
		_ = logFile.Close()
		return nil, err
	}
	operational.Licenses, err = licensehttp.NewHandler(licenseService, cfg.Environment)
	if err != nil {
		_ = database.Close(db)
		_ = logFile.Close()
		return nil, err
	}
	licenseCoordinatorOptions := []licensecoordination.Option{
		licensecoordination.WithApplicationEnvironment(cfg.Audit.EnvironmentCode),
		licensecoordination.WithBuiltinComponentSource(applicationregistryinfrastructure.BuiltinRuntimeComponentCatalog{}),
	}
	if source, ok := operational.AccessApplier.(applicationregistryapplication.RuntimeLicenseLifecycleApprovalSource); ok {
		verifier, verifierErr := applicationregistryapplication.NewRuntimeLicenseLifecycleVerifier(source)
		if verifierErr != nil {
			_ = database.Close(db)
			_ = logFile.Close()
			return nil, verifierErr
		}
		licenseCoordinatorOptions = append(licenseCoordinatorOptions, licensecoordination.WithLifecycleApprovalVerifier(verifier))
	}
	licenseCoordinator, err := licensecoordination.NewService(db, licenseService, licensetrust.Keys(), licenseSnapshotSigner{applicationTokenManager, licensetrust.Keys()}, time.Now, licenseCoordinatorOptions...)
	// 授权协调服务就绪后交接 inventory 即可读取真实授权状态。
	handoverLicenseReader = licenseCoordinator
	if err != nil {
		_ = database.Close(db)
		_ = logFile.Close()
		return nil, err
	}
	operational.RuntimeLicenses, err = licensehttp.NewRuntimeHandler(licenseCoordinator)
	if err != nil {
		_ = database.Close(db)
		_ = logFile.Close()
		return nil, err
	}
	if source, ok := operational.AccessApplier.(licensehttp.EvidenceSource); ok {
		operational.RuntimeLicenses.ConfigureEvidence(source, cfg.Audit.EnvironmentCode)
	}
	if operational.SubsystemOnboarding != nil {
		var approvalSource applicationregistryapplication.RuntimeLicenseApprovalSource
		if source, ok := operational.AccessApplier.(applicationregistryapplication.RuntimeLicenseApprovalSource); ok {
			approvalSource = source
		}
		enrollment, enrollErr := applicationregistryapplication.NewRuntimeLicenseEnrollmentServiceWithCatalog(
			approvalSource,
			applicationregistryinfrastructure.BuiltinRuntimeComponentCatalog{},
			oauthClientManagementService,
			licenseCoordinator,
		)
		if enrollErr != nil {
			_ = database.Close(db)
			_ = logFile.Close()
			return nil, enrollErr
		}
		operational.SubsystemOnboarding.ConfigureRuntimeLicenseEnrollment(enrollment)
		operational.SubsystemOnboarding.ConfigureRuntimeLicenseSettings(func(ctx context.Context) (applicationregistryapplication.RuntimeLicenseSettings, error) {
			state, readErr := licenseService.Read(ctx)
			if errors.Is(readErr, licensedomain.ErrNotInitialized) {
				return applicationregistryapplication.RuntimeLicenseSettings{}, applicationregistryapplication.ErrRuntimeLicenseNotInitialized
			}
			if readErr != nil {
				return applicationregistryapplication.RuntimeLicenseSettings{}, readErr
			}
			return applicationregistryapplication.RuntimeLicenseSettings{InstanceID: state.InstanceID, Environment: state.Environment, PlatformBaseURL: cfg.CommercialRuntime.PlatformBaseURL, PlatformPublicKeyPath: cfg.CommercialRuntime.PlatformPublicKeyPath, StateDirectory: cfg.CommercialRuntime.StateDirectory, AllowHTTP: strings.HasPrefix(cfg.CommercialRuntime.PlatformBaseURL, "http://")}, nil
		})
	}

	var offlineDelivery *OfflineDeliveryRunner
	if cfg.Environment == "production" && cfg.Audit.EnvironmentCode == "prod" {
		offlineDelivery, err = NewOfflineDeliveryRunner(repository, licenseService, licenseCoordinator, operational.SubsystemOnboarding, cfg)
		if err == nil {
			if source, ok := operational.AccessApplier.(offlineInstallationSource); ok {
				err = offlineDelivery.ConfigureInstallation(source, licenseCoordinator, licenseService)
			} else {
				err = errors.New("offline installation evidence source is unavailable")
			}
		}
		if err != nil {
			_ = database.Close(db)
			_ = logFile.Close()
			return nil, err
		}
	}
	api := &API{
		OfflineDelivery: offlineDelivery,
		Handler: httptransport.NewRouter(
			cfg, logger, db, authHandler, bootstrapHandler, managementHandler, accountLifecycleHandler, authorizationHandler, applicationAccessHandler, positionGrantHandler, auditHandler, configurationHandler, settingsHandler, dictionaryHandler, loginSecurityHandler, applicationTokenHandler, applicationManagementHandler, oauthClientManagementHandler, applicationRegistryService, auditService, oidcHandler, operational,
		),
		Logger:       logger,
		database:     db,
		oidcDatabase: oidcDB,
		logFile:      logFile,
	}
	workerContext, cancelWorker := context.WithCancel(context.Background())
	api.logoutWorkerCancel = cancelWorker
	api.logoutWorkerDone = make(chan struct{})
	go func() {
		defer close(api.logoutWorkerDone)
		if runErr := backchannelDispatcher.Run(workerContext, time.Second); runErr != nil && !errors.Is(runErr, context.Canceled) {
			logger.Error("OIDC back-channel logout worker stopped", "error", runErr)
		}
	}()
	return api, nil
}

// bootstrapPlatformCatalog 确保平台应用自身的授权目录行反映迁移种子的内置角色与
// 权限。子系统自有目录（如 contract_management）由其 catalog-publisher OAuth client
// 发布，刻意不在此处触碰。平台自身没有这样的 client；若没有这条 bootstrap，即使
// 内置数据已在数据库中，UI 也会以"目录同步: 未同步"阻止角色分配。
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

// resolveDefaultTenantID 查找迁移种子的 "default" 租户。bootstrap 路径绝不
// 硬编码租户 ULID，使未来多租户安装能复用同一代码。
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

// resolvePlatformCatalogOperatorID 优先取首位超级管理员的用户 ID，使审计历史与
// 最初搭建平台的人对齐。若 bootstrap 尚未执行，则使用稳定的系统身份，让目录行
// 在首位管理员接管前仍有合法的 26 位 last_synced_by 引用。
func resolvePlatformCatalogOperatorID(db *gorm.DB, tenantID string) (string, error) {
	// application code "platform" 与 role code "platform-super-admin" 由
	// 000011_seed_platform_defaults.sql 迁移种子写入；此处动态查找，
	// 使 bootstrap 能容忍未来的 schema 重命名。
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
	// Pre-bootstrap 阶段：在首位管理员接管前使用稳定的系统身份。26 位占位符是
	// 合法的 Crockford Base32 ULID，且带不重叠的 "PLATFSY" 后缀，
	// 绝不会与真实用户 ID 冲突。
	return applicationaccess.PlatformCatalogBootstrapOperatorID, nil
}

// Close 释放 API 持有的数据库连接与日志文件句柄。
// 仅在 NewAPI 初始化完成后调用；若依赖未就绪则按空值保护直接返回。
func (api *API) Close() {
	if api.logoutWorkerCancel != nil {
		api.logoutWorkerCancel()
	}
	if api.logoutWorkerDone != nil {
		select {
		case <-api.logoutWorkerDone:
		case <-time.After(5 * time.Second):
			api.Logger.Warn("OIDC back-channel logout worker did not stop before deadline")
		}
	}
	if api.oidcDatabase != nil {
		if err := database.Close(api.oidcDatabase); err != nil {
			api.Logger.Error("close OIDC mysql database handle", "error", err)
		}
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

type oidcLogoutSignerAdapter struct{ manager *security.OIDCJWTManager }

type licenseSnapshotSigner struct {
	manager    *security.ApplicationJWTManager
	vendorKeys map[string]ed25519.PublicKey
}

// Sign 用平台受管签名身份对许可快照签名。
func (s licenseSnapshotSigner) Sign(snapshot licenseruntime.PlatformSnapshot) (string, error) {
	return s.manager.Sign(snapshot)
}

// Verify 用厂商公钥验签并解析许可快照，绑定校验交由底层 JWT 管理器完成。
func (s licenseSnapshotSigner) Verify(raw string, binding licenseruntime.Binding) (licenseruntime.PlatformSnapshot, error) {
	return s.manager.VerifyLicenseSnapshot(raw, binding, s.vendorKeys)
}

// IssueLogoutToken 将投递参数映射为专用 logout+jwt 声明，并严格使用调用方给定的有效期。
func (adapter oidcLogoutSignerAdapter) IssueLogoutToken(issuer, audience, subject, session, jti string, now time.Time, ttl time.Duration) (string, error) {
	if adapter.manager == nil || ttl <= 0 {
		return "", errors.New("OIDC logout token signer is not configured")
	}
	return adapter.manager.IssueLogoutToken(security.OIDCLogoutTokenClaims{
		Issuer: issuer, Subject: subject, Audience: []string{audience}, IssuedAt: now.UTC(),
		ExpiresAt: now.UTC().Add(ttl), JWTID: jti, SessionID: session,
		Events: map[string]any{security.OIDCBackchannelLogoutEvent: map[string]any{}},
	}, ttl)
}

// oidcIDTokenVerifierAdapter 把 Keycloak broker 验证器的 ID Token 校验适配为
// 身份 HTTP 层接口：验签通过后要求 identity_id 与 sub 都存在，并以平台身份 identity_id
// 作为登录主体。
//
// 不能要求两个声明相等：Keycloak 的 sub 是 Keycloak 内部用户 ID，平台身份保存在
// identity_id 用户属性里，投影按 identity_id 对齐用户，Keycloak 用户 ID 只作为外部
// 资源句柄（见 keycloakauthorization 的 keycloak_admin.go）。常规部署下二者本来就不同。
type oidcIDTokenVerifierAdapter struct {
	verifier *middleware.KeycloakBrokerJWTVerifier
}

// VerifyIDToken 验签上游 ID Token 并返回平台身份 identity_id 作为登录主体。
func (adapter oidcIDTokenVerifierAdapter) VerifyIDToken(ctx context.Context, raw, nonce, clientID string) (string, error) {
	claims, err := adapter.verifier.VerifyIDToken(ctx, raw, nonce, clientID)
	if err != nil {
		return "", err
	}
	identityID := strings.TrimSpace(claims.IdentityID)
	if identityID == "" || strings.TrimSpace(claims.Subject) == "" {
		return "", errors.New("Keycloak ID token identity claims are invalid")
	}
	return identityID, nil
}

// customerBindingResolverAdapter 把外部身份模块的绑定解析适配为 OIDC 传输层接口。
// 任何解析错误都按"无绑定"处理，authorization-context 省略 customer_ref 声明。
type customerBindingResolverAdapter struct {
	service *externalidentityapplication.Service
}

// ResolveCustomerBinding 解析平台身份在指定应用内绑定的 CRM 客户引用。
func (adapter customerBindingResolverAdapter) ResolveCustomerBinding(ctx context.Context, tenantID, platformUserID, applicationCode string) (string, error) {
	resolved, err := adapter.service.ResolveCustomerBinding(ctx, tenantID, platformUserID, applicationCode)
	if err != nil {
		return "", err
	}
	return resolved.CustomerRef, nil
}

// parseSameSite 把已校验的配置值转换为 Go 的 cookie 枚举，供 OIDC logout 使用。
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
