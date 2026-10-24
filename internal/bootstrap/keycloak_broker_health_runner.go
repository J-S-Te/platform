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

// brokerHealthVerifier 验证 Keycloak Broker IdP 存在且配置完整，
// 由 Keycloak 控制面实现。
type brokerHealthVerifier interface {
	VerifyBrokerExists(ctx context.Context) error
	VerifyCustomerPortalBrokerExists(ctx context.Context) error
}

// brokerCredentialChecker 报告平台 broker OAuth client 是否存在活动凭据，
// 由 GORM 实现查询平台数据库。
type brokerCredentialChecker interface {
	HasActiveCredential(ctx context.Context, clientID string) error
}

type customerPortalBrokerHealthReconciler interface {
	Reconcile(context.Context) (registered bool, err error)
}

// gormBrokerCredentialChecker 是基于数据库的凭据检查器。
type gormBrokerCredentialChecker struct{ db *gorm.DB }

// HasActiveCredential 校验指定 broker client 存在 ACTIVE 状态的凭据记录；
// 无凭据时返回错误供健康检查升级告警。
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

// keycloakBrokerHealthRunner 周期性验证两个平台 Keycloak Broker IdP 及其平台侧
// OAuth client。它存在的原因是 Broker 配置只在启动时对账一次；之后的漂移
// （IdP 配置为空、client 凭据丢失）否则只会在用户登录时才暴露。检测到的问题
// 被记录下来，但不进入热错误循环。
type keycloakBrokerHealthRunner struct {
	verifier         brokerHealthVerifier
	checker          brokerCredentialChecker
	portalReconciler customerPortalBrokerHealthReconciler
	logger           *slog.Logger
	poll             time.Duration
	lastWarn         time.Time
}

func newKeycloakBrokerHealthRunner(verifier brokerHealthVerifier, db *gorm.DB, logger *slog.Logger, poll time.Duration, portalReconcilers ...customerPortalBrokerHealthReconciler) (*keycloakBrokerHealthRunner, error) {
	if verifier == nil || db == nil || logger == nil || poll <= 0 || len(portalReconcilers) > 1 {
		return nil, errors.New("Keycloak broker health runner configuration is invalid")
	}
	runner := &keycloakBrokerHealthRunner{
		verifier: verifier, checker: gormBrokerCredentialChecker{db: db}, logger: logger, poll: poll,
	}
	if len(portalReconcilers) == 1 {
		runner.portalReconciler = portalReconcilers[0]
	}
	return runner, nil
}

// Run 按时间间隔验证两个 broker。失败按每分钟最多一条日志限流，
// 使持续性的配置错误无法刷爆 worker 日志。
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
	// 客户门户是可选子系统；Worker 启动后才接入时由 reconciler 创建 Broker。
	portalReady := true
	if runner.portalReconciler != nil {
		registered, err := runner.portalReconciler.Reconcile(ctx)
		if err != nil {
			warn(slog.LevelError, "Keycloak customer portal Broker reconciliation failed", err.Error())
			portalReady = false
		} else {
			portalReady = registered
		}
	}
	clientIDs := []string{brokerClientIDKeycloak}
	if portalReady {
		clientIDs = append(clientIDs, brokerClientIDCustomerPortal)
	}
	// 平台侧：broker client 必须有活跃凭据，否则 Keycloak 无法完成令牌交换。
	for _, clientID := range clientIDs {
		if err := runner.checker.HasActiveCredential(ctx, clientID); err != nil {
			warn(slog.LevelError, "Keycloak broker client has no active credential",
				"client_id="+clientID+" error="+err.Error())
		}
	}
	// Keycloak 侧：两个 IdP 必须存在且 config 完整。
	if err := runner.verifier.VerifyBrokerExists(ctx); err != nil {
		warn(slog.LevelError, "Keycloak broker IdP is incomplete", "detail="+err.Error())
	}
	if portalReady {
		if err := runner.verifier.VerifyCustomerPortalBrokerExists(ctx); err != nil {
			warn(slog.LevelError, "Keycloak customer broker IdP is incomplete", "detail="+err.Error())
		}
	}
}
