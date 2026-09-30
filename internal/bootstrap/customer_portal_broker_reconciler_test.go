package bootstrap

import (
	"context"
	"errors"
	"testing"
)

type fakePortalBrokerRegistrar struct {
	clientID, secret string
	ensureErr        error
	recoverCalls     int
}

func (fake *fakePortalBrokerRegistrar) EnsureCustomerPortalBroker(context.Context, string) (string, string, error) {
	return fake.clientID, fake.secret, fake.ensureErr
}

func (fake *fakePortalBrokerRegistrar) RecoverCustomerPortalBroker(context.Context, string, string) (string, string, error) {
	fake.recoverCalls++
	return fake.clientID, "recovered-secret", nil
}

type fakePortalBrokerControl struct {
	verifyErr   error
	ensureCalls int
}

func (fake *fakePortalBrokerControl) EnsureCustomerPortalBroker(context.Context, string, string) error {
	fake.ensureCalls++
	return nil
}

func (fake *fakePortalBrokerControl) VerifyCustomerPortalBrokerExists(context.Context) error {
	return fake.verifyErr
}

func TestCustomerPortalBrokerReconcilerSkipsUntilApplicationRegistered(t *testing.T) {
	registrar := &fakePortalBrokerRegistrar{ensureErr: errBrokerTargetNotRegistered}
	control := &fakePortalBrokerControl{}
	reconciler := customerPortalBrokerReconciler{registrar: registrar, control: control, tenantID: "tenant-1"}
	registered, err := reconciler.Reconcile(context.Background())
	if err != nil || registered {
		t.Fatalf("Reconcile() = (%t, %v), want (false, nil)", registered, err)
	}
	if control.ensureCalls != 0 || registrar.recoverCalls != 0 {
		t.Fatalf("unexpected provisioning for an unregistered application: ensure=%d recover=%d", control.ensureCalls, registrar.recoverCalls)
	}
}

func TestCustomerPortalBrokerReconcilerUsesNewCredentialWhenAvailable(t *testing.T) {
	registrar := &fakePortalBrokerRegistrar{clientID: "portal-broker", secret: "new-secret"}
	control := &fakePortalBrokerControl{}
	reconciler := customerPortalBrokerReconciler{registrar: registrar, control: control, tenantID: "tenant-1"}
	registered, err := reconciler.Reconcile(context.Background())
	if err != nil || !registered {
		t.Fatalf("Reconcile() = (%t, %v), want (true, nil)", registered, err)
	}
	if control.ensureCalls != 1 || registrar.recoverCalls != 0 {
		t.Fatalf("unexpected reconciliation calls: ensure=%d recover=%d", control.ensureCalls, registrar.recoverCalls)
	}
}

func TestCustomerPortalBrokerReconcilerDoesNotRotateForUnclassifiedFailure(t *testing.T) {
	registrar := &fakePortalBrokerRegistrar{clientID: "portal-broker"}
	control := &fakePortalBrokerControl{verifyErr: errors.New("Keycloak unavailable")}
	reconciler := customerPortalBrokerReconciler{registrar: registrar, control: control, tenantID: "tenant-1"}
	registered, err := reconciler.Reconcile(context.Background())
	if err == nil || !registered {
		t.Fatalf("Reconcile() = (%t, %v), want registered with an error", registered, err)
	}
	if registrar.recoverCalls != 0 {
		t.Fatalf("credential rotated after a non-drift failure: recover=%d", registrar.recoverCalls)
	}
}
