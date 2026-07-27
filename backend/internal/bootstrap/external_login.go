package bootstrap

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"time"

	auditapplication "github.com/J-S-Te/Basic-Platform/backend/internal/platform/audit/application"
	identityapplication "github.com/J-S-Te/Basic-Platform/backend/internal/platform/identity/application"
	federatedloginapplication "github.com/J-S-Te/Basic-Platform/backend/internal/platform/identity/federation/login/application"
	federatedlogininfrastructure "github.com/J-S-Te/Basic-Platform/backend/internal/platform/identity/federation/login/infrastructure"
	federatedloginhttp "github.com/J-S-Te/Basic-Platform/backend/internal/platform/identity/federation/login/interfaces/http"
	"github.com/J-S-Te/Basic-Platform/backend/internal/shared/config"
	"github.com/J-S-Te/Basic-Platform/backend/internal/shared/security"
	"gorm.io/gorm"
)

const (
	externalLoginStateTTL  = 5 * time.Minute
	externalLoginClockSkew = time.Minute
)

// buildExternalLoginHandler wires the external OIDC browser flow only when an outbound host
// allow-list is configured. Leaving the allow-list empty keeps external login disabled without
// weakening or interrupting local account authentication.
func buildExternalLoginHandler(
	cfg config.Config,
	database *gorm.DB,
	authentication *identityapplication.Service,
	providerSecretProtector *security.EnvelopeProtector,
	logger *slog.Logger,
	auditRecorder *auditapplication.Service,
) (*federatedloginhttp.Handler, error) {
	if !externalLoginRuntimeEnabled(cfg.Identity) {
		return nil, nil
	}
	if err := validateExternalLoginRuntime(cfg.Identity, providerSecretProtector); err != nil {
		return nil, err
	}

	stateProtector, err := security.NewEnvelopeProtector(
		cfg.Identity.ExternalLoginStateEncryptionKey,
		"IAM_EXTERNAL_LOGIN_STATE_ENCRYPTION_KEY",
	)
	if err != nil {
		return nil, err
	}
	runtimeResolver, err := federatedlogininfrastructure.NewGORMRuntimeResolver(database, providerSecretProtector)
	if err != nil {
		return nil, err
	}
	stateStore, err := federatedlogininfrastructure.NewDefaultGORMStateStore(database, stateProtector)
	if err != nil {
		return nil, err
	}
	sessionIssuer, err := federatedlogininfrastructure.NewFederatedSessionIssuer(authentication)
	if err != nil {
		return nil, err
	}
	remote, err := federatedlogininfrastructure.NewHTTPClient(federatedlogininfrastructure.HTTPClientConfig{
		Timeout:           cfg.Identity.ExternalOIDCHTTPTimeout,
		AllowInsecureHTTP: cfg.Identity.ExternalOIDCAllowInsecureHTTP,
		AllowedHosts:      cfg.Identity.ExternalOIDCAllowedHosts,
	})
	if err != nil {
		return nil, err
	}
	service, err := federatedloginapplication.NewService(
		runtimeResolver,
		runtimeResolver,
		sessionIssuer,
		stateStore,
		federatedlogininfrastructure.SecretGenerator{},
		remote,
		federatedloginapplication.SystemClock{},
		federatedloginapplication.Config{
			StateTTL:          externalLoginStateTTL,
			ClockSkew:         externalLoginClockSkew,
			AllowInsecureHTTP: cfg.Identity.ExternalOIDCAllowInsecureHTTP,
		},
	)
	if err != nil {
		return nil, err
	}
	sameSite, err := parseSameSite(cfg.Auth.SessionCookieSameSite)
	if err != nil {
		return nil, err
	}
	return federatedloginhttp.NewHandler(
		externalLoginHTTPService{service: service},
		federatedloginhttp.CookieConfig{
			Name:     cfg.Auth.SessionCookieName,
			Path:     "/",
			Secure:   cfg.Auth.SessionCookieSecure,
			SameSite: sameSite,
		},
		logger,
		auditRecorder,
		cfg.Audit,
	)
}

func externalLoginRuntimeEnabled(identity config.IdentityConfig) bool {
	return len(identity.ExternalOIDCAllowedHosts) > 0
}

func validateExternalLoginRuntime(identity config.IdentityConfig, providerSecretProtector *security.EnvelopeProtector) error {
	if !externalLoginRuntimeEnabled(identity) {
		return nil
	}
	if providerSecretProtector == nil || strings.TrimSpace(identity.FederatedProviderSecretEncryptionKey) == "" {
		return errors.New("IAM_FEDERATED_PROVIDER_SECRET_ENCRYPTION_KEY must be configured when external OIDC login is enabled")
	}
	if strings.TrimSpace(identity.ExternalLoginStateEncryptionKey) == "" {
		return errors.New("IAM_EXTERNAL_LOGIN_STATE_ENCRYPTION_KEY must be configured when external OIDC login is enabled")
	}
	return nil
}

// externalLoginHTTPService maps the application lifecycle type to the HTTP adapter's audit-only
// lifecycle type.
type externalLoginHTTPService struct {
	service *federatedloginapplication.Service
}

func (adapter externalLoginHTTPService) Begin(ctx context.Context, input federatedloginapplication.BeginInput) (federatedloginapplication.BeginResult, error) {
	return adapter.service.Begin(ctx, input)
}

func (adapter externalLoginHTTPService) CompleteCallbackWithLifecycle(ctx context.Context, input federatedloginapplication.CallbackInput) (federatedloginapplication.CallbackResult, federatedloginhttp.CallbackLifecycle, error) {
	result, lifecycle, err := adapter.service.CompleteCallbackWithLifecycle(ctx, input)
	return result, externalLoginHTTPLifecycle(lifecycle), err
}

func externalLoginHTTPLifecycle(lifecycle federatedloginapplication.CallbackLifecycle) federatedloginhttp.CallbackLifecycle {
	return federatedloginhttp.CallbackLifecycle{
		TenantID:     lifecycle.TenantID,
		ProviderCode: lifecycle.ProviderCode,
		ProviderID:   lifecycle.ProviderID,
		BindingID:    lifecycle.BindingID,
		UserID:       lifecycle.UserID,
		AccountID:    lifecycle.AccountID,
	}
}

var _ federatedloginhttp.ApplicationService = externalLoginHTTPService{}
