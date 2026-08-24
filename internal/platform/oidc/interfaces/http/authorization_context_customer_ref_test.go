package oidchttp

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
)

type customerBindingResolverStub struct {
	tenantID, platformUserID, applicationCode string
	ref                                       string
	err                                       error
}

func (resolver *customerBindingResolverStub) ResolveCustomerBinding(_ context.Context, tenantID, platformUserID, applicationCode string) (string, error) {
	resolver.tenantID, resolver.platformUserID, resolver.applicationCode = tenantID, platformUserID, applicationCode
	if resolver.err != nil {
		return "", resolver.err
	}
	return resolver.ref, nil
}

func newCustomerRefHandler(binding *customerBindingResolverStub, emit bool) *Handler {
	return &Handler{
		jwtManager:          rejectingExternalJWTManager{},
		accessTokenSubjects: accessTokenSubjectResolverStub{},
		externalAuthorizationVerifier: externalAuthorizationVerifierStub{claims: ExternalAuthorizationTokenClaims{
			Subject: "identity-1", IdentityID: "identity-1", TenantID: "tenant-1", SessionID: "session-1",
			AuthorizedParty: "contract-prod-web", Audience: []string{"contract-prod-web"}, TokenUse: "access_token",
		}},
		authorizationContextResolver: &authorizationContextResolverStub{},
		customerBindingResolver:      binding,
		emitCustomerRef:              emit,
		clock:                        authorizationContextClock{},
		logger:                       slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
}

func TestAuthorizationContextEmitsCustomerRefWhenEnabled(t *testing.T) {
	binding := &customerBindingResolverStub{ref: "CRM-CUST-1"}
	handler := newCustomerRefHandler(binding, true)
	request := httptest.NewRequest(http.MethodGet, "/oauth2/authorization-context", nil)
	request.Header.Set("Authorization", "Bearer external-token")
	response := httptest.NewRecorder()

	handler.AuthorizationContext(response, request)

	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", response.Code, response.Body.String())
	}
	var body struct {
		CustomerRef string `json:"customer_ref"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if body.CustomerRef != "CRM-CUST-1" {
		t.Fatalf("customer_ref = %q, want CRM-CUST-1", body.CustomerRef)
	}
	if binding.tenantID != "tenant-1" || binding.platformUserID != "identity-1" || binding.applicationCode != "contract_management" {
		t.Fatalf("binding resolver args = tenant:%q platform:%q app:%q", binding.tenantID, binding.platformUserID, binding.applicationCode)
	}
}

func TestAuthorizationContextOmitsCustomerRef(t *testing.T) {
	tests := []struct {
		name    string
		emit    bool
		binding *customerBindingResolverStub
		calls   int
	}{
		{name: "switch disabled", emit: false, binding: &customerBindingResolverStub{ref: "CRM-CUST-1"}, calls: 0},
		{name: "resolver error", emit: true, binding: &customerBindingResolverStub{err: errors.New("no key")}, calls: 1},
		{name: "blank reference", emit: true, binding: &customerBindingResolverStub{ref: "  "}, calls: 1},
		{name: "no resolver", emit: false, binding: nil, calls: 0},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			handler := newCustomerRefHandler(test.binding, test.emit)
			request := httptest.NewRequest(http.MethodGet, "/oauth2/authorization-context", nil)
			request.Header.Set("Authorization", "Bearer external-token")
			response := httptest.NewRecorder()

			handler.AuthorizationContext(response, request)

			if response.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200; body=%s", response.Code, response.Body.String())
			}
			var body map[string]any
			if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
				t.Fatalf("decode response: %v", err)
			}
			if _, present := body["customer_ref"]; present {
				t.Fatalf("customer_ref present when it must be omitted: %v", body["customer_ref"])
			}
			if test.binding != nil && test.binding.tenantID != "" && test.calls == 0 {
				t.Fatal("resolver was called while the switch is disabled")
			}
		})
	}
}
