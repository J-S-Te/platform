package bootstrap

import (
	"context"
	"errors"
	"fmt"

	applicationregistryhttp "github.com/J-S-Te/Basic-Platform/internal/platform/applicationregistry/interfaces/http"
)

type customerPortalBrokerClientRegistrar interface {
	EnsureCustomerPortalBroker(context.Context, string) (string, string, error)
	RecoverCustomerPortalBroker(context.Context, string, string) (string, string, error)
}

type customerPortalBrokerControl interface {
	EnsureCustomerPortalBroker(context.Context, string, string) error
	VerifyCustomerPortalBrokerExists(context.Context) error
}

// customerPortalBrokerReconciler 在门户应用/环境完成注册后（包括 worker 启动后）
// 开通可选的门户 Broker。
type customerPortalBrokerReconciler struct {
	registrar customerPortalBrokerClientRegistrar
	control   customerPortalBrokerControl
	tenantID  string
}

// Reconcile 确保门户 Broker OAuth client 与 IdP 配置就绪。目标应用/环境尚未注册时
// 返回 false（下次再试）；其余错误包装后上抛，由调度方决定重试。
func (reconciler customerPortalBrokerReconciler) Reconcile(ctx context.Context) (bool, error) {
	clientID, clientSecret, err := reconciler.registrar.EnsureCustomerPortalBroker(ctx, reconciler.tenantID)
	if errors.Is(err, errBrokerTargetNotRegistered) {
		return false, nil
	}
	if err != nil {
		return true, fmt.Errorf("ensure customer portal Broker OAuth client: %w", err)
	}
	if clientID == "" {
		return true, errors.New("customer portal Broker OAuth client ID is empty")
	}
	if clientSecret != "" {
		if err := reconciler.control.EnsureCustomerPortalBroker(ctx, clientID, clientSecret); err != nil {
			return true, fmt.Errorf("reconcile customer portal Broker IdP: %w", err)
		}
		return true, nil
	}
	if err := reconciler.control.VerifyCustomerPortalBrokerExists(ctx); err == nil {
		return true, nil
	} else if !applicationregistryhttp.IsRecoverableKeycloakBrokerConfigurationError(err) {
		return true, fmt.Errorf("verify customer portal Broker IdP: %w", err)
	}

	// OAuth client secret 以哈希存储；只对已定性的 Keycloak 配置漂移轮换，
	// 绝不因瞬时网络或服务故障轮换。
	clientID, clientSecret, err = reconciler.registrar.RecoverCustomerPortalBroker(ctx, reconciler.tenantID, "system-keycloak")
	if err != nil {
		return true, fmt.Errorf("recover customer portal Broker OAuth credential: %w", err)
	}
	if err := reconciler.control.EnsureCustomerPortalBroker(ctx, clientID, clientSecret); err != nil {
		return true, fmt.Errorf("reconcile recovered customer portal Broker IdP: %w", err)
	}
	return true, nil
}
