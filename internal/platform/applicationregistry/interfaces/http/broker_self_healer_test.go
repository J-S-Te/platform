package http

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"

	application "github.com/J-S-Te/Basic-Platform/internal/platform/applicationregistry/application"
)

// fakeBrokerHealOAuth 模拟 broker client 查询与轮换，用于验证自愈编排与限流。
type fakeBrokerHealOAuth struct {
	lookupCalls  int32
	rotateCalls  int32
	rotateSecret string
	lookupErr    error
	rotateErr    error
}

func (f *fakeBrokerHealOAuth) GetOAuthClientByClientID(context.Context, string, string) (application.OAuthClientView, error) {
	atomic.AddInt32(&f.lookupCalls, 1)
	if f.lookupErr != nil {
		return application.OAuthClientView{}, f.lookupErr
	}
	return application.OAuthClientView{ID: "client-internal-id", ClientID: "keycloak-broker"}, nil
}

func (f *fakeBrokerHealOAuth) RotateOAuthClientSecret(context.Context, application.OAuthClientSecretRotateInput) (application.OAuthClientSecretResult, error) {
	atomic.AddInt32(&f.rotateCalls, 1)
	if f.rotateErr != nil {
		return application.OAuthClientSecretResult{}, f.rotateErr
	}
	return application.OAuthClientSecretResult{PlaintextSecret: f.rotateSecret}, nil
}

// fakeBrokerHealControl 记录 IdP 同步调用。
type fakeBrokerHealControl struct {
	brokerCalls   int32
	customerCalls int32
	syncErr       error
}

func (f *fakeBrokerHealControl) EnsureBroker(context.Context, string, string) error {
	atomic.AddInt32(&f.brokerCalls, 1)
	return f.syncErr
}

func (f *fakeBrokerHealControl) EnsureCustomerPortalBroker(context.Context, string, string) error {
	atomic.AddInt32(&f.customerCalls, 1)
	return f.syncErr
}

func TestBrokerTargetForClient(t *testing.T) {
	cases := []struct {
		clientID    string
		wantAlias   string
		wantAppCode string
		wantManaged bool
	}{
		{"keycloak-broker", "basic-platform", "platform", true},
		{"keycloak-customer-portal-broker", "basic-platform-customer", "customer_portal", true},
		{"customer_and_opportunity-prod-web", "", "", false},
	}
	for _, tc := range cases {
		alias, code := brokerTargetForClient(tc.clientID)
		if tc.wantManaged && alias != tc.wantAlias {
			t.Fatalf("%s: alias got %q want %q", tc.clientID, alias, tc.wantAlias)
		}
		if tc.wantManaged && code != tc.wantAppCode {
			t.Fatalf("%s: app code got %q want %q", tc.clientID, code, tc.wantAppCode)
		}
		if !tc.wantManaged && alias != "" {
			t.Fatalf("%s: expected unmanaged, got alias %q", tc.clientID, alias)
		}
	}
}

func TestHealBrokerSecretDriftRotatesAndSyncs(t *testing.T) {
	oauth := &fakeBrokerHealOAuth{rotateSecret: "ocsec_newsecret"}
	control := &fakeBrokerHealControl{}
	healer := newBrokerDriftSelfHealerNoExport(oauth, control)

	if err := healer.HealBrokerSecretDrift(context.Background(), "keycloak-broker"); err != nil {
		t.Fatalf("HealBrokerSecretDrift: %v", err)
	}
	if atomic.LoadInt32(&oauth.rotateCalls) != 1 {
		t.Fatalf("expected 1 rotate, got %d", oauth.rotateCalls)
	}
	if atomic.LoadInt32(&control.brokerCalls) != 1 {
		t.Fatalf("expected 1 basic-platform sync, got %d", control.brokerCalls)
	}
	if atomic.LoadInt32(&control.customerCalls) != 0 {
		t.Fatalf("unexpected customer sync")
	}
}

func TestHealBrokerSecretDriftCustomerPortal(t *testing.T) {
	oauth := &fakeBrokerHealOAuth{rotateSecret: "ocsec_newsecret"}
	control := &fakeBrokerHealControl{}
	healer := newBrokerDriftSelfHealerNoExport(oauth, control)

	if err := healer.HealBrokerSecretDrift(context.Background(), "keycloak-customer-portal-broker"); err != nil {
		t.Fatalf("HealBrokerSecretDrift: %v", err)
	}
	if atomic.LoadInt32(&control.customerCalls) != 1 {
		t.Fatalf("expected customer sync, got %d", control.customerCalls)
	}
	if atomic.LoadInt32(&control.brokerCalls) != 0 {
		t.Fatalf("unexpected basic sync")
	}
}

func TestHealBrokerSecretDriftUnknownClientNoOp(t *testing.T) {
	oauth := &fakeBrokerHealOAuth{}
	control := &fakeBrokerHealControl{}
	healer := newBrokerDriftSelfHealerNoExport(oauth, control)

	if err := healer.HealBrokerSecretDrift(context.Background(), "some-business-web"); err != nil {
		t.Fatalf("unknown client should be no-op, got %v", err)
	}
	if atomic.LoadInt32(&oauth.rotateCalls) != 0 || atomic.LoadInt32(&control.brokerCalls) != 0 {
		t.Fatalf("unknown client should not trigger repair")
	}
}

func TestHealBrokerSecretDriftRateLimited(t *testing.T) {
	oauth := &fakeBrokerHealOAuth{rotateSecret: "ocsec_newsecret"}
	control := &fakeBrokerHealControl{}
	healer := newBrokerDriftSelfHealerNoExport(oauth, control)

	_ = healer.HealBrokerSecretDrift(context.Background(), "keycloak-broker")
	_ = healer.HealBrokerSecretDrift(context.Background(), "keycloak-broker") // within TTL, should be skipped
	if atomic.LoadInt32(&oauth.rotateCalls) != 1 {
		t.Fatalf("expected rate limit to allow only 1 rotate, got %d", oauth.rotateCalls)
	}
}

func TestHealBrokerSecretDriftPropagatesError(t *testing.T) {
	oauth := &fakeBrokerHealOAuth{rotateErr: errors.New("rotate failed")}
	control := &fakeBrokerHealControl{}
	healer := newBrokerDriftSelfHealerNoExport(oauth, control)

	if err := healer.HealBrokerSecretDrift(context.Background(), "keycloak-broker"); err == nil {
		t.Fatal("expected rotate error to propagate")
	}
}

// newBrokerDriftSelfHealerNoExport 用最短 TTL 构造自愈器，供限流测试。
func newBrokerDriftSelfHealerNoExport(oauth brokerOAuthService, control brokerIdpSyncer) *brokerDriftSelfHealer {
	return NewBrokerDriftSelfHealer(oauth, control, "tenant", nil)
}
