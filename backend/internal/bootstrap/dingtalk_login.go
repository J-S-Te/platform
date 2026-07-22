package bootstrap

import (
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"time"

	auditapplication "github.com/J-S-Te/Basic-Platform/backend/internal/platform/audit/application"
	identityapplication "github.com/J-S-Te/Basic-Platform/backend/internal/platform/identity/application"
	dingtalkapplication "github.com/J-S-Te/Basic-Platform/backend/internal/platform/identity/federation/dingtalk/application"
	dingtalkinfrastructure "github.com/J-S-Te/Basic-Platform/backend/internal/platform/identity/federation/dingtalk/infrastructure"
	dingtalkhttp "github.com/J-S-Te/Basic-Platform/backend/internal/platform/identity/federation/dingtalk/interfaces/http"
	federatedlogininfrastructure "github.com/J-S-Te/Basic-Platform/backend/internal/platform/identity/federation/login/infrastructure"
	"github.com/J-S-Te/Basic-Platform/backend/internal/shared/config"
	"github.com/J-S-Te/Basic-Platform/backend/internal/shared/security"
	"gorm.io/gorm"
)

const dingTalkQRStateTTL = 5 * time.Minute

// buildDingTalkLoginHandler wires the dedicated DingTalk protocol adapter. It intentionally does
// not reuse the OIDC provider allow-list or discovery client because DingTalk is a fixed protocol.
func buildDingTalkLoginHandler(
	cfg config.Config,
	database *gorm.DB,
	authentication *identityapplication.Service,
	providerSecretProtector *security.EnvelopeProtector,
	logger *slog.Logger,
	auditRecorder *auditapplication.Service,
) (*dingtalkhttp.Handler, error) {
	if !dingTalkRuntimeEnabled(cfg.Identity) {
		return nil, nil
	}
	if providerSecretProtector == nil {
		return nil, errors.New("IAM_FEDERATED_PROVIDER_SECRET_ENCRYPTION_KEY must be configured when DingTalk QR login is enabled")
	}
	stateProtector, err := security.NewEnvelopeProtector(cfg.Identity.ExternalLoginStateEncryptionKey, "IAM_EXTERNAL_LOGIN_STATE_ENCRYPTION_KEY")
	if err != nil {
		return nil, err
	}
	resolver, err := dingtalkinfrastructure.NewGORMRuntimeResolver(database, providerSecretProtector)
	if err != nil {
		return nil, err
	}
	stateStore, err := dingtalkinfrastructure.NewDefaultGORMStateStore(database, stateProtector)
	if err != nil {
		return nil, err
	}
	issuer, err := federatedlogininfrastructure.NewFederatedSessionIssuer(authentication)
	if err != nil {
		return nil, err
	}
	remote, err := dingtalkinfrastructure.NewHTTPClient(&http.Client{Timeout: cfg.Identity.DingTalkHTTPTimeout})
	if err != nil {
		return nil, err
	}
	service, err := dingtalkapplication.NewService(resolver, resolver, issuer, stateStore, dingtalkinfrastructure.SecretGenerator{}, remote, dingtalkapplication.SystemClock{}, dingtalkapplication.Config{StateTTL: dingTalkQRStateTTL})
	if err != nil {
		return nil, err
	}
	sameSite, err := parseSameSite(cfg.Auth.SessionCookieSameSite)
	if err != nil {
		return nil, err
	}
	return dingtalkhttp.NewHandler(service, stateProtector, dingtalkhttp.CookieConfig{Name: cfg.Auth.SessionCookieName, Path: "/", Secure: cfg.Auth.SessionCookieSecure, SameSite: sameSite}, logger, auditRecorder, cfg.Audit)
}

// DingTalk is enabled only when both existing envelope keys are present. This preserves local and
// OIDC login in deployments that have not opted in to the DingTalk adapter.
func dingTalkRuntimeEnabled(identity config.IdentityConfig) bool {
	return strings.TrimSpace(identity.FederatedProviderSecretEncryptionKey) != "" && strings.TrimSpace(identity.ExternalLoginStateEncryptionKey) != ""
}
