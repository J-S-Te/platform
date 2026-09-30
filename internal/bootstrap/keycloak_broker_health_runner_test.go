package bootstrap

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"
)

// fakeBrokerHealthVerifier 记录 verify 调用并返回可配置错误。
type fakeBrokerHealthVerifier struct {
	basicCalls    int32
	customerCalls int32
	err           error
}

func (f *fakeBrokerHealthVerifier) VerifyBrokerExists(context.Context) error {
	atomic.AddInt32(&f.basicCalls, 1)
	return f.err
}

func (f *fakeBrokerHealthVerifier) VerifyCustomerPortalBrokerExists(context.Context) error {
	atomic.AddInt32(&f.customerCalls, 1)
	return f.err
}

// fakeBrokerCredentialChecker 返回可配置的 credential 状态。
type fakeBrokerCredentialChecker struct {
	calls int32
	err   error
}

type fakeCustomerPortalBrokerReconciler struct {
	registered bool
	err        error
}

func (f fakeCustomerPortalBrokerReconciler) Reconcile(context.Context) (bool, error) {
	return f.registered, f.err
}

func (f *fakeBrokerCredentialChecker) HasActiveCredential(context.Context, string) error {
	atomic.AddInt32(&f.calls, 1)
	return f.err
}

func TestKeycloakBrokerHealthRunnerValidation(t *testing.T) {
	if _, err := newKeycloakBrokerHealthRunner(nil, nil, testLogger(), time.Minute); err == nil {
		t.Fatal("expected nil verifier rejection")
	}
	if _, err := newKeycloakBrokerHealthRunner(&fakeBrokerHealthVerifier{}, nil, testLogger(), time.Minute); err == nil {
		t.Fatal("expected nil db rejection")
	}
}

func TestKeycloakBrokerHealthRunnerVerifiesBothBrokers(t *testing.T) {
	verifier := &fakeBrokerHealthVerifier{}
	checker := &fakeBrokerCredentialChecker{}
	runner := &keycloakBrokerHealthRunner{verifier: verifier, checker: checker, logger: testLogger(), poll: time.Minute}
	runner.check(context.Background())
	if atomic.LoadInt32(&verifier.basicCalls) != 1 {
		t.Fatalf("expected 1 basic verify, got %d", verifier.basicCalls)
	}
	if atomic.LoadInt32(&verifier.customerCalls) != 1 {
		t.Fatalf("expected 1 customer verify, got %d", verifier.customerCalls)
	}
	if atomic.LoadInt32(&checker.calls) != 2 {
		t.Fatalf("expected 2 credential checks, got %d", checker.calls)
	}
}

func TestKeycloakBrokerHealthRunnerChecksNoPanicOnErrors(t *testing.T) {
	verifier := &fakeBrokerHealthVerifier{err: errors.New("idp missing")}
	checker := &fakeBrokerCredentialChecker{err: errors.New("no credential")}
	runner := &keycloakBrokerHealthRunner{verifier: verifier, checker: checker, logger: testLogger(), poll: time.Minute}
	for i := 0; i < 3; i++ {
		runner.check(context.Background()) // 不应 panic，且限流日志
	}
}

func TestKeycloakBrokerHealthRunnerSkipsOptionalPortalBeforeOnboarding(t *testing.T) {
	verifier := &fakeBrokerHealthVerifier{}
	checker := &fakeBrokerCredentialChecker{}
	runner := &keycloakBrokerHealthRunner{
		verifier: verifier, checker: checker, portalReconciler: fakeCustomerPortalBrokerReconciler{},
		logger: testLogger(), poll: time.Minute,
	}
	runner.check(context.Background())
	if got := atomic.LoadInt32(&checker.calls); got != 1 {
		t.Fatalf("credential checks = %d, want only the required platform Broker", got)
	}
	if got := atomic.LoadInt32(&verifier.customerCalls); got != 0 {
		t.Fatalf("customer portal verification calls = %d, want 0 before onboarding", got)
	}
}

func TestKeycloakBrokerHealthRunnerChecksPortalAfterReconciliation(t *testing.T) {
	verifier := &fakeBrokerHealthVerifier{}
	checker := &fakeBrokerCredentialChecker{}
	runner := &keycloakBrokerHealthRunner{
		verifier: verifier, checker: checker, portalReconciler: fakeCustomerPortalBrokerReconciler{registered: true},
		logger: testLogger(), poll: time.Minute,
	}
	runner.check(context.Background())
	if got := atomic.LoadInt32(&checker.calls); got != 2 {
		t.Fatalf("credential checks = %d, want both Brokers", got)
	}
	if got := atomic.LoadInt32(&verifier.customerCalls); got != 1 {
		t.Fatalf("customer portal verification calls = %d, want 1", got)
	}
}
