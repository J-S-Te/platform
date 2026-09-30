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

// customerPortalBrokerReconciler provisions the optional portal Broker when
// its application/environment is registered, including after worker startup.
type customerPortalBrokerReconciler struct {
	registrar customerPortalBrokerClientRegistrar
	control   customerPortalBrokerControl
	tenantID  string
}

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

	// OAuth client secrets are stored hashed; rotate only for classified Keycloak
	// configuration drift, never for transient network or service failures.
	clientID, clientSecret, err = reconciler.registrar.RecoverCustomerPortalBroker(ctx, reconciler.tenantID, "system-keycloak")
	if err != nil {
		return true, fmt.Errorf("recover customer portal Broker OAuth credential: %w", err)
	}
	if err := reconciler.control.EnsureCustomerPortalBroker(ctx, clientID, clientSecret); err != nil {
		return true, fmt.Errorf("reconcile recovered customer portal Broker IdP: %w", err)
	}
	return true, nil
}
