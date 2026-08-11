package application

import (
	"context"
	"errors"
	"testing"
)

type loginTargetRepositoryStub struct {
	target      LoginTarget
	environment Environment
	targetErr   error
	envErr      error
}

func (stub loginTargetRepositoryStub) FindActiveLoginTarget(context.Context, LoginTargetResolveInput) (LoginTarget, error) {
	return stub.target, stub.targetErr
}

func (stub loginTargetRepositoryStub) FindActiveEnvironment(context.Context, string, string, string) (Environment, error) {
	return stub.environment, stub.envErr
}

func TestValidLoginTargetURI(t *testing.T) {
	tests := []struct {
		name  string
		value string
		want  bool
	}{
		{name: "relative root path", value: "/dashboard", want: true},
		{name: "relative nested path", value: "/reports/monthly", want: true},
		{name: "absolute https", value: "https://contracts.example.com/dashboard", want: true},
		{name: "absolute http rejected", value: "http://contracts.example.com/dashboard", want: false},
		{name: "scheme relative rejected", value: "//evil.example/dashboard", want: false},
		{name: "hostname rejected", value: "evil.example/dashboard", want: false},
		{name: "dot traversal rejected", value: "/../admin", want: false},
		{name: "encoded traversal rejected", value: "/%2e%2e/admin", want: false},
		{name: "encoded slash rejected", value: "/reports%2fadmin", want: false},
		{name: "double encoded traversal rejected", value: "/%252e%252e/admin", want: false},
		{name: "backslash rejected", value: "/reports\\admin", want: false},
		{name: "encoded backslash rejected", value: "/reports%5cadmin", want: false},
		{name: "query rejected", value: "/dashboard?next=/admin", want: false},
		{name: "fragment rejected", value: "/dashboard#section", want: false},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := validLoginTargetURI(test.value); got != test.want {
				t.Fatalf("validLoginTargetURI(%q) = %v, want %v", test.value, got, test.want)
			}
		})
	}
}

func TestJoinEnvironmentBaseURLAndTargetURI(t *testing.T) {
	prefix := "/contract"
	tests := []struct {
		name       string
		baseURL    string
		pathPrefix *string
		targetURI  string
		want       string
		wantErr    error
	}{
		{
			name:       "portal host plus subsystem prefix",
			baseURL:    "http://portal.local:8081",
			pathPrefix: &prefix,
			targetURI:  "/dashboard",
			want:       "http://portal.local:8081/contract/dashboard",
		},
		{
			name:       "base path is preserved",
			baseURL:    "https://portal.example.com/root/",
			pathPrefix: &prefix,
			targetURI:  "/dashboard",
			want:       "https://portal.example.com/root/contract/dashboard",
		},
		{
			name:      "legacy environment without prefix",
			baseURL:   "http://portal.local",
			targetURI: "/dashboard",
			want:      "http://portal.local/dashboard",
		},
		{
			name:       "traversal target rejected",
			baseURL:    "http://portal.local",
			pathPrefix: &prefix,
			targetURI:  "/../admin",
			wantErr:    ErrNotFound,
		},
		{
			name:      "base query rejected",
			baseURL:   "http://portal.local?source=bad",
			targetURI: "/dashboard",
			wantErr:   ErrNotFound,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, err := joinEnvironmentBaseURLAndTargetURI(test.baseURL, test.pathPrefix, test.targetURI)
			if !errors.Is(err, test.wantErr) {
				t.Fatalf("error = %v, want %v", err, test.wantErr)
			}
			if got != test.want {
				t.Fatalf("URI = %q, want %q", got, test.want)
			}
		})
	}
}

func TestResolveActiveTargetURIUsesPublicBaseAndPathPrefix(t *testing.T) {
	const (
		tenantID      = "01ARZ3NDEKTSV4RRFFQ69G5FAV"
		applicationID = "01ARZ3NDEKTSV4RRFFQ69G5FAW"
		environmentID = "01ARZ3NDEKTSV4RRFFQ69G5FAX"
		targetID      = "01ARZ3NDEKTSV4RRFFQ69G5FAY"
	)
	baseURL := "http://portal.local:8081"
	upstreamURL := "http://10.0.0.8:8088"
	pathPrefix := "/contract"

	service, err := NewLoginTargetService(loginTargetRepositoryStub{
		target: LoginTarget{
			ID:            targetID,
			TenantID:      tenantID,
			ApplicationID: applicationID,
			EnvironmentID: environmentID,
			TargetCode:    "dashboard",
			TargetURI:     "/dashboard",
			Status:        loginTargetStatusActive,
		},
		environment: Environment{
			ID:            environmentID,
			TenantID:      tenantID,
			ApplicationID: applicationID,
			Environment:   "prod",
			BaseURL:       &baseURL,
			UpstreamURL:   &upstreamURL,
			PathPrefix:    &pathPrefix,
			Status:        "ACTIVE",
		},
	})
	if err != nil {
		t.Fatalf("NewLoginTargetService() error = %v", err)
	}

	got, err := service.ResolveActiveTargetURI(context.Background(), LoginTargetResolveInput{
		TenantID:      tenantID,
		ApplicationID: applicationID,
		EnvironmentID: environmentID,
		TargetCode:    "dashboard",
	})
	if err != nil {
		t.Fatalf("ResolveActiveTargetURI() error = %v", err)
	}
	want := "http://portal.local:8081/contract/dashboard"
	if got != want {
		t.Fatalf("resolved URI = %q, want %q", got, want)
	}
	if got == upstreamURL+"/dashboard" {
		t.Fatalf("resolved URI leaked internal upstream: %q", got)
	}
}
