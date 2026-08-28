package bootstrap

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"gorm.io/gorm"
)

const (
	brokerClientIDKeycloak       = "keycloak-broker"
	brokerClientIDCustomerPortal = "keycloak-customer-portal-broker"
)

// brokerHealthVerifier verifies that a Keycloak Broker IdP exists with a complete config.
// The Keycloak control plane implements it.
type brokerHealthVerifier interface {
	VerifyBrokerExists(ctx context.Context) error
	VerifyCustomerPortalBrokerExists(ctx context.Context) error
}

// brokerCredentialChecker reports whether a platform broker OAuth client has an active
// credential. The GORM implementation queries the platform database.
type brokerCredentialChecker interface {
	HasActiveCredential(ctx context.Context, clientID string) error
}

// gormBrokerCredentialChecker is the database-backed credential checker.
type gormBrokerCredentialChecker struct{ db *gorm.DB }

func (c gormBrokerCredentialChecker) HasActiveCredential(ctx context.Context, clientID string) error {
	var count int64
	err := c.db.WithContext(ctx).
		Table("platform_oauth_client_credential AS c").
		Joins("JOIN platform_oauth_client AS o ON o.id = c.oauth_client_id").
		Where("o.client_id = ? AND o.status = ? AND c.status = ?", clientID, "ACTIVE", "ACTIVE").
		Count(&count).Error
	if err != nil {
		return err
	}
	if count == 0 {
		return errors.New("no active credential")
	}
	return nil
}

// keycloakBrokerHealthRunner periodically verifies the two platform Keycloak Broker IdPs
// and their platform-side OAuth clients. It exists because Broker config is reconciled
// only once at startup; a later drift (empty IdP config, missing client credential) would
// otherwise surface only when a user logs in. Detected problems are logged and recorded
// without a hot error loop.
type keycloakBrokerHealthRunner struct {
	verifier brokerHealthVerifier
	checker  brokerCredentialChecker
	logger   *slog.Logger
	poll     time.Duration
	lastWarn time.Time
}

func newKeycloakBrokerHealthRunner(verifier brokerHealthVerifier, db *gorm.DB, logger *slog.Logger, poll time.Duration) (*keycloakBrokerHealthRunner, error) {
	if verifier == nil || db == nil || logger == nil || poll <= 0 {
		return nil, errors.New("Keycloak broker health runner configuration is invalid")
	}
	return &keycloakBrokerHealthRunner{
		verifier: verifier, checker: gormBrokerCredentialChecker{db: db}, logger: logger, poll: poll,
	}, nil
}

// Run verifies both brokers on an interval. Failures are rate-limited to one log line per
// minute so a persistent misconfiguration cannot flood the worker log.
func (runner *keycloakBrokerHealthRunner) Run(ctx context.Context) {
	ticker := time.NewTicker(runner.poll)
	defer ticker.Stop()
	for {
		runner.check(ctx)
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func (runner *keycloakBrokerHealthRunner) check(ctx context.Context) {
	now := time.Now().UTC()
	warn := func(level slog.Level, msg, detail string) {
		if !runner.lastWarn.IsZero() && now.Sub(runner.lastWarn) < time.Minute {
			return
		}
		runner.lastWarn = now
		runner.logger.Log(ctx, level, msg, "detail", detail)
	}
	// 平台侧：broker client 必须有活跃凭据，否则 Keycloak 无法完成令牌交换。
	for _, clientID := range []string{brokerClientIDKeycloak, brokerClientIDCustomerPortal} {
		if err := runner.checker.HasActiveCredential(ctx, clientID); err != nil {
			warn(slog.LevelError, "Keycloak broker client has no active credential",
				"client_id="+clientID+" error="+err.Error())
		}
	}
	// Keycloak 侧：两个 IdP 必须存在且 config 完整。
	if err := runner.verifier.VerifyBrokerExists(ctx); err != nil {
		warn(slog.LevelError, "Keycloak broker IdP is incomplete", "detail="+err.Error())
	}
	if err := runner.verifier.VerifyCustomerPortalBrokerExists(ctx); err != nil {
		warn(slog.LevelError, "Keycloak customer broker IdP is incomplete", "detail="+err.Error())
	}
}
