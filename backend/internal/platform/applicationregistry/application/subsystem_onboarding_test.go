package application

import (
	"context"
	"fmt"
	"testing"
	"time"

	"golang.org/x/crypto/bcrypt"
)

type subsystemOnboardingRepositoryStub struct {
	write       SubsystemOnboardingWrite
	portalItems []PortalApplication
	createCalls int
	listCalls   int
}

func (repository *subsystemOnboardingRepositoryStub) CreateSubsystem(_ context.Context, write SubsystemOnboardingWrite, now time.Time) (SubsystemOnboardingResult, error) {
	repository.write = write
	repository.createCalls++
	return SubsystemOnboardingResult{
		Application: Application{
			ID: write.ApplicationID, TenantID: write.Application.TenantID, Code: write.Application.Code,
			Name: write.Application.Name, ApplicationType: write.Application.ApplicationType,
			HomepageURL: write.Application.HomepageURL, Description: write.Application.Description,
			Status: write.Application.Status, Version: 1, CreatedAt: now, UpdatedAt: now,
		},
		Environment: Environment{
			ID: write.EnvironmentID, TenantID: write.Environment.TenantID, ApplicationID: write.Environment.ApplicationID,
			Environment: write.Environment.Environment, BaseURL: write.Environment.BaseURL,
			UpstreamURL: write.Environment.UpstreamURL, PathPrefix: write.Environment.PathPrefix,
			Status: write.Environment.Status, Version: 1, CreatedAt: now, UpdatedAt: now,
		},
		LoginTarget: LoginTargetManagementItem{
			ID: write.LoginTargetID, TenantID: write.LoginTarget.TenantID,
			ApplicationID: write.LoginTarget.ApplicationID, EnvironmentID: write.LoginTarget.EnvironmentID,
			TargetCode: write.LoginTarget.TargetCode, Name: write.LoginTarget.Name,
			TargetURI: write.LoginTarget.TargetURI, Status: write.LoginTarget.Status,
		},
		OAuthClient: OAuthClientView{
			ID: write.OAuthClientID, TenantID: write.OAuthClient.TenantID,
			ApplicationID: write.OAuthClient.ApplicationID, EnvironmentID: write.OAuthClient.EnvironmentID,
			ClientID: write.OAuthClient.ClientID, ClientName: write.OAuthClient.ClientName,
			ClientType: write.OAuthClient.ClientType, TokenAuthMethod: write.OAuthClient.TokenAuthMethod,
			AccessTokenTTLSeconds:  write.OAuthClient.AccessTokenTTLSeconds,
			RefreshTokenTTLSeconds: write.OAuthClient.RefreshTokenTTLSeconds,
			RequirePKCE:            write.OAuthClient.RequirePKCE, GrantTypes: write.OAuthClient.GrantTypes,
			Scopes: write.OAuthClient.Scopes, RedirectURIs: write.OAuthClient.RedirectURIs,
			Status: oauthClientStatusActive,
		},
	}, nil
}

func (repository *subsystemOnboardingRepositoryStub) ListPortalApplications(_ context.Context, _, _, _ string) ([]PortalApplication, error) {
	repository.listCalls++
	return repository.portalItems, nil
}

type sequentialManagementIDs struct{ next int }

func (generator *sequentialManagementIDs) New(time.Time) (string, error) {
	generator.next++
	return fmt.Sprintf("01K10C%020d", generator.next), nil
}

type fixedSubsystemClock struct{ now time.Time }

func (clock fixedSubsystemClock) Now() time.Time { return clock.now }

func TestOnboardSubsystemBuildsAtomicOIDCRegistration(t *testing.T) {
	repository := &subsystemOnboardingRepositoryStub{}
	clock := fixedSubsystemClock{now: time.Date(2026, time.July, 27, 10, 0, 0, 0, time.UTC)}
	service, err := NewSubsystemOnboardingService(repository, &sequentialManagementIDs{}, clock, RedirectURIValidationPolicy{})
	if err != nil {
		t.Fatalf("construct service: %v", err)
	}

	input := SubsystemOnboardingInput{
		TenantID: "01K10A00000000000000000001", OperatorID: "01K10B00000000000000000001", ApplicationCode: " Business-App ",
		ApplicationName: "业务应用", PublicBaseURL: "https://portal.example.com/",
		UpstreamURL: "http://10.0.0.8:8081/",
	}
	result, err := service.OnboardSubsystem(context.Background(), input)
	if err != nil {
		t.Fatalf("onboard subsystem: %v", err)
	}
	if repository.createCalls != 1 {
		t.Fatalf("expected one atomic repository call, got %d", repository.createCalls)
	}
	write := repository.write
	if write.Application.Code != "business-app" || write.Environment.Environment != "prod" {
		t.Fatalf("unexpected normalized registration: code=%q environment=%q", write.Application.Code, write.Environment.Environment)
	}
	if write.Environment.PathPrefix == nil || *write.Environment.PathPrefix != "/business-app" {
		t.Fatalf("expected generated path prefix, got %#v", write.Environment.PathPrefix)
	}
	if write.LoginTarget.TargetCode != "home" || write.LoginTarget.TargetURI != "/business-app/" {
		t.Fatalf("unexpected home login target: %#v", write.LoginTarget)
	}
	if write.OAuthClient.ClientID != "business-app-prod-web" || write.OAuthClient.TokenAuthMethod != "client_secret_basic" || !write.OAuthClient.RequirePKCE {
		t.Fatalf("unexpected OAuth client registration: %#v", write.OAuthClient)
	}
	if len(write.OAuthClient.RedirectURIs) != 1 || write.OAuthClient.RedirectURIs[0] != "https://portal.example.com/business-app/auth/callback" {
		t.Fatalf("unexpected redirect URIs: %#v", write.OAuthClient.RedirectURIs)
	}
	if result.PublicURL != "https://portal.example.com/business-app/" || result.PlaintextSecret == "" {
		t.Fatalf("unexpected integration result: public_url=%q has_secret=%t", result.PublicURL, result.PlaintextSecret != "")
	}
	if write.OAuthClientSecret == nil || len(write.OAuthClientSecret.SecretHash) == 0 {
		t.Fatal("expected only a protected client secret to cross the repository boundary")
	}
	if err := bcrypt.CompareHashAndPassword(write.OAuthClientSecret.SecretHash, []byte(result.PlaintextSecret)); err != nil {
		t.Fatalf("stored hash does not match returned one-time secret: %v", err)
	}
}

func TestListPortalApplicationsResolvesRelativeTargets(t *testing.T) {
	repository := &subsystemOnboardingRepositoryStub{portalItems: []PortalApplication{
		{ApplicationID: "app-1", Code: "business-app", Name: "业务应用", Environment: "prod", PublicURL: "https://portal.example.com", TargetURI: "/business-app/"},
		{ApplicationID: "app-broken", Code: "broken", Name: "错误配置", Environment: "prod", PublicURL: "", TargetURI: "relative"},
	}}
	service, err := NewSubsystemOnboardingService(repository, &sequentialManagementIDs{}, fixedSubsystemClock{now: time.Now()}, RedirectURIValidationPolicy{})
	if err != nil {
		t.Fatalf("construct service: %v", err)
	}

	items, err := service.ListPortalApplications(context.Background(), "tenant-1", "user-1", "prod")
	if err != nil {
		t.Fatalf("list portal applications: %v", err)
	}
	if len(items) != 1 || items[0].PublicURL != "https://portal.example.com/business-app/" {
		t.Fatalf("unexpected portal catalog: %#v", items)
	}
}
