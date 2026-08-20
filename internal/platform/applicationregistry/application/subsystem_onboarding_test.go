package application

import (
	"context"
	"fmt"
	"testing"
	"time"

	"golang.org/x/crypto/bcrypt"
)

type subsystemOnboardingRepositoryStub struct {
	write                SubsystemOnboardingWrite
	directoryWrite       SubsystemDirectoryRegistrationWrite
	portalItems          []PortalApplication
	createCalls          int
	directoryCreateCalls int
	listCalls            int
}

func (repository *subsystemOnboardingRepositoryStub) CreateSubsystem(_ context.Context, write SubsystemOnboardingWrite, now time.Time) (SubsystemOnboardingResult, error) {
	repository.write = write
	repository.createCalls++
	result := SubsystemOnboardingResult{
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
		CatalogPublisherOAuthClient: OAuthClientView{
			ID: write.CatalogPublisherOAuthClientID, TenantID: write.CatalogPublisherOAuthClient.TenantID,
			ApplicationID: write.CatalogPublisherOAuthClient.ApplicationID, EnvironmentID: write.CatalogPublisherOAuthClient.EnvironmentID,
			ClientID: write.CatalogPublisherOAuthClient.ClientID, ClientName: write.CatalogPublisherOAuthClient.ClientName,
			ClientType: write.CatalogPublisherOAuthClient.ClientType, TokenAuthMethod: write.CatalogPublisherOAuthClient.TokenAuthMethod,
			AccessTokenTTLSeconds:  write.CatalogPublisherOAuthClient.AccessTokenTTLSeconds,
			RefreshTokenTTLSeconds: write.CatalogPublisherOAuthClient.RefreshTokenTTLSeconds,
			RequirePKCE:            write.CatalogPublisherOAuthClient.RequirePKCE, GrantTypes: write.CatalogPublisherOAuthClient.GrantTypes,
			Scopes: write.CatalogPublisherOAuthClient.Scopes, RedirectURIs: write.CatalogPublisherOAuthClient.RedirectURIs,
			Status: oauthClientStatusActive,
		},
	}
	for _, item := range write.ServiceClients {
		result.ServiceCredentials = append(result.ServiceCredentials, SubsystemServiceCredential{
			Purpose: item.Purpose,
			OAuthClient: OAuthClientView{
				ID: item.OAuthClientID, TenantID: item.OAuthClient.TenantID,
				ApplicationID: item.OAuthClient.ApplicationID, EnvironmentID: item.OAuthClient.EnvironmentID,
				ClientID: item.OAuthClient.ClientID, ClientName: item.OAuthClient.ClientName,
				ClientType: item.OAuthClient.ClientType, TokenAuthMethod: item.OAuthClient.TokenAuthMethod,
				AccessTokenTTLSeconds: item.OAuthClient.AccessTokenTTLSeconds,
				GrantTypes:            item.OAuthClient.GrantTypes, Scopes: item.OAuthClient.Scopes,
				Status: oauthClientStatusActive,
			},
		})
	}
	return result, nil
}

func (repository *subsystemOnboardingRepositoryStub) CreateSubsystemDirectory(_ context.Context, write SubsystemDirectoryRegistrationWrite, now time.Time) (SubsystemDirectoryRegistrationResult, error) {
	repository.directoryWrite = write
	repository.directoryCreateCalls++
	return SubsystemDirectoryRegistrationResult{
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
			IssuerAlias: write.Environment.IssuerAlias, Status: write.Environment.Status, Version: 1,
			CreatedAt: now, UpdatedAt: now,
		},
		LoginTarget: LoginTargetManagementItem{
			ID: write.LoginTargetID, TenantID: write.LoginTarget.TenantID,
			ApplicationID: write.LoginTarget.ApplicationID, EnvironmentID: write.LoginTarget.EnvironmentID,
			TargetCode: write.LoginTarget.TargetCode, Name: write.LoginTarget.Name,
			TargetURI: write.LoginTarget.TargetURI, Status: write.LoginTarget.Status, Version: 1,
			CreatedAt: now, UpdatedAt: now,
		},
	}, nil
}

func TestRegisterSubsystemDirectoryDoesNotCreateOIDCOrServiceClients(t *testing.T) {
	repository := &subsystemOnboardingRepositoryStub{}
	service, err := NewSubsystemOnboardingService(repository, &sequentialManagementIDs{}, fixedSubsystemClock{now: time.Date(2026, time.August, 11, 0, 0, 0, 0, time.UTC)}, RedirectURIValidationPolicy{})
	if err != nil {
		t.Fatal(err)
	}
	alias := IssuerAliasKeycloak
	result, err := service.RegisterSubsystemDirectory(context.Background(), SubsystemDirectoryRegistrationInput{
		TenantID: "01K10A00000000000000000001", OperatorID: "01K10B00000000000000000001",
		ApplicationCode: "inventory", ApplicationName: "库存管理系统", Environment: "prod",
		PublicBaseURL: "https://portal.example.com", UpstreamURL: "http://inventory-api:8080",
		PathPrefix: "/inventory", IssuerAlias: &alias,
	})
	if err != nil {
		t.Fatal(err)
	}
	if repository.createCalls != 0 || repository.directoryCreateCalls != 1 {
		t.Fatalf("legacy create calls=%d directory create calls=%d", repository.createCalls, repository.directoryCreateCalls)
	}
	write := repository.directoryWrite
	if write.Application.Code != "inventory" || write.Environment.IssuerAlias == nil || *write.Environment.IssuerAlias != IssuerAliasKeycloak {
		t.Fatalf("unexpected directory write: %#v", write)
	}
	if write.LoginTarget.TargetCode != "home" || write.LoginTarget.TargetURI != "/inventory/" {
		t.Fatalf("unexpected login target: %#v", write.LoginTarget)
	}
	if result.PublicURL != "https://portal.example.com/inventory/" {
		t.Fatalf("public url = %q", result.PublicURL)
	}
}

func TestOnboardCustomerPortalCreatesSixIndependentLeastPrivilegeServiceClients(t *testing.T) {
	repository := &subsystemOnboardingRepositoryStub{}
	service, err := NewSubsystemOnboardingService(repository, &sequentialManagementIDs{}, fixedSubsystemClock{now: time.Date(2026, time.August, 2, 0, 0, 0, 0, time.UTC)}, RedirectURIValidationPolicy{})
	if err != nil {
		t.Fatal(err)
	}
	result, err := service.OnboardSubsystem(context.Background(), SubsystemOnboardingInput{
		TenantID: "01K10A00000000000000000001", OperatorID: "01K10B00000000000000000001",
		ApplicationCode: integratedPortalApplicationCode, ApplicationName: "客户自助门户", Environment: "dev",
		PublicBaseURL: "http://localhost:8081", UpstreamURL: integratedPortalUpstreamURL,
		PathPrefix: integratedPortalPathPrefix, ClientType: "confidential",
	})
	if err != nil {
		t.Fatal(err)
	}
	expectedScopes := map[string]string{
		ServiceCredentialAuditIngest:            "audit.ingest",
		ServiceCredentialExternalUserProvision:  "external_user.provision",
		ServiceCredentialApplicationRoleAssign:  "application_role.assign",
		ServiceCredentialApplicationRoleRevoke:  "application_role.revoke",
		ServiceCredentialPortalMappingProvision: "portal.identity_mapping.provision",
		ServiceCredentialPortalMappingDisable:   "portal.identity_mapping.disable",
		ServiceCredentialPortalInviteVerify:     "portal.invite.verify",
	}
	if len(repository.write.ServiceClients) != len(expectedScopes) || len(result.ServiceCredentials) != len(expectedScopes) {
		t.Fatalf("service clients write=%d result=%d", len(repository.write.ServiceClients), len(result.ServiceCredentials))
	}
	clientIDs, plaintextSecrets := map[string]bool{}, map[string]bool{}
	for _, credential := range result.ServiceCredentials {
		expectedScope, ok := expectedScopes[credential.Purpose]
		if !ok {
			t.Fatalf("unexpected purpose %q", credential.Purpose)
		}
		client := credential.OAuthClient
		if client.ClientType != "service" || client.TokenAuthMethod != "client_secret_basic" || len(client.GrantTypes) != 1 || client.GrantTypes[0] != "client_credentials" || len(client.Scopes) != 1 || client.Scopes[0] != expectedScope {
			t.Fatalf("client %q has broader capabilities: %#v", credential.Purpose, client)
		}
		if credential.PlaintextSecret == "" || plaintextSecrets[credential.PlaintextSecret] {
			t.Fatalf("purpose %q has missing or reused secret", credential.Purpose)
		}
		if clientIDs[client.ClientID] {
			t.Fatalf("duplicate client id %q", client.ClientID)
		}
		clientIDs[client.ClientID], plaintextSecrets[credential.PlaintextSecret] = true, true
	}
	if result.RedirectURI != "http://localhost:8081/customer-portal/auth/callback" {
		t.Fatalf("redirect = %q", result.RedirectURI)
	}
}

func TestOnboardSubsystemServiceBindingsFromManifestDriveClientCreation(t *testing.T) {
	// 清单声明 allowed_service_bindings 时，只创建声明的用途（+审计基线）。
	repository := &subsystemOnboardingRepositoryStub{}
	service, err := NewSubsystemOnboardingService(repository, &sequentialManagementIDs{}, fixedSubsystemClock{now: time.Date(2026, time.August, 2, 0, 0, 0, 0, time.UTC)}, RedirectURIValidationPolicy{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.OnboardSubsystem(context.Background(), SubsystemOnboardingInput{
		TenantID: "01K10A00000000000000000001", OperatorID: "01K10B00000000000000000001",
		ApplicationCode: integratedPortalApplicationCode, ApplicationName: "客户自助门户", Environment: "dev",
		PublicBaseURL: "http://localhost:8081", UpstreamURL: integratedPortalUpstreamURL,
		PathPrefix: integratedPortalPathPrefix, ClientType: "confidential",
		AllowedServiceBindings: []string{
			ServiceCredentialExternalUserProvision,
			ServiceCredentialApplicationRoleAssign,
			ServiceCredentialApplicationRoleRevoke,
			ServiceCredentialPortalMappingProvision,
			ServiceCredentialPortalMappingDisable,
			ServiceCredentialPortalInviteVerify,
		},
	}); err != nil {
		t.Fatal(err)
	}
	got := make(map[string]bool, len(repository.write.ServiceClients))
	for _, item := range repository.write.ServiceClients {
		got[item.Purpose] = true
	}
	want := []string{
		ServiceCredentialAuditIngest,
		ServiceCredentialExternalUserProvision,
		ServiceCredentialApplicationRoleAssign,
		ServiceCredentialApplicationRoleRevoke,
		ServiceCredentialPortalMappingProvision,
		ServiceCredentialPortalMappingDisable,
		ServiceCredentialPortalInviteVerify,
	}
	if len(got) != len(want) {
		t.Fatalf("clients = %v, want %v", got, want)
	}
	for _, purpose := range want {
		if !got[purpose] {
			t.Fatalf("missing client for purpose %s", purpose)
		}
	}
}

func TestOnboardSubsystemServiceBindingsFallbackMatchesHardcodedDefault(t *testing.T) {
	// 未声明 allowed_service_bindings 时回退平台硬编码默认：customer 只创建 owner_directory + 审计。
	repository := &subsystemOnboardingRepositoryStub{}
	service, err := NewSubsystemOnboardingService(repository, &sequentialManagementIDs{}, fixedSubsystemClock{now: time.Date(2026, time.August, 2, 0, 0, 0, 0, time.UTC)}, RedirectURIValidationPolicy{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.OnboardSubsystem(context.Background(), SubsystemOnboardingInput{
		TenantID: "01K10A00000000000000000001", OperatorID: "01K10B00000000000000000001",
		ApplicationCode: integratedCustomerApplicationCode, ApplicationName: "客户与商机管理", Environment: "dev",
		PublicBaseURL: "http://localhost:8081", UpstreamURL: integratedCustomerUpstreamURL,
		PathPrefix: integratedCustomerPathPrefix, ClientType: "confidential",
	}); err != nil {
		t.Fatal(err)
	}
	if len(repository.write.ServiceClients) != 3 {
		t.Fatalf("fallback clients = %d, want audit + owner_directory + notification", len(repository.write.ServiceClients))
	}
	purposes := map[string]bool{}
	for _, item := range repository.write.ServiceClients {
		purposes[item.Purpose] = true
	}
	if !purposes[ServiceCredentialAuditIngest] || !purposes[ServiceCredentialOwnerDirectoryRead] || !purposes[ServiceCredentialNotificationIngest] {
		t.Fatalf("fallback clients = %v", purposes)
	}
}

func TestOnboardSubsystemRejectsUnregisteredServiceBindingPurpose(t *testing.T) {
	repository := &subsystemOnboardingRepositoryStub{}
	service, err := NewSubsystemOnboardingService(repository, &sequentialManagementIDs{}, fixedSubsystemClock{now: time.Date(2026, time.August, 2, 0, 0, 0, 0, time.UTC)}, RedirectURIValidationPolicy{})
	if err != nil {
		t.Fatal(err)
	}
	_, err = service.OnboardSubsystem(context.Background(), SubsystemOnboardingInput{
		TenantID: "01K10A00000000000000000001", OperatorID: "01K10B00000000000000000001",
		ApplicationCode: integratedContractApplicationCode, ApplicationName: "合同管理", Environment: "prod",
		PublicBaseURL: "http://localhost:8081", UpstreamURL: integratedContractApplicationCode + "-api:8081",
		PathPrefix: "/" + integratedContractApplicationCode, ClientType: "confidential",
		AllowedServiceBindings: []string{"unreviewed_scope"},
	})
	if err == nil {
		t.Fatal("unregistered service binding purpose was accepted")
	}
}

func TestOnboardCustomerOpportunityCreatesIsolatedOwnerDirectoryClient(t *testing.T) {
	repository := &subsystemOnboardingRepositoryStub{}
	service, err := NewSubsystemOnboardingService(repository, &sequentialManagementIDs{}, fixedSubsystemClock{now: time.Date(2026, time.August, 2, 0, 0, 0, 0, time.UTC)}, RedirectURIValidationPolicy{})
	if err != nil {
		t.Fatal(err)
	}
	result, err := service.OnboardSubsystem(context.Background(), SubsystemOnboardingInput{
		TenantID: "01K10A00000000000000000001", OperatorID: "01K10B00000000000000000001",
		ApplicationCode: integratedCustomerApplicationCode, ApplicationName: "客户与商机管理", Environment: "dev",
		PublicBaseURL: "http://localhost:8081", UpstreamURL: integratedCustomerUpstreamURL,
		PathPrefix: integratedCustomerPathPrefix, ClientType: "confidential",
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(repository.write.ServiceClients) != 3 || len(result.ServiceCredentials) != 3 {
		t.Fatalf("service clients write=%d result=%d", len(repository.write.ServiceClients), len(result.ServiceCredentials))
	}
	credentials := make(map[string]SubsystemServiceCredential, len(result.ServiceCredentials))
	for _, credential := range result.ServiceCredentials {
		credentials[credential.Purpose] = credential
	}
	auditCredential := credentials[ServiceCredentialAuditIngest]
	if len(auditCredential.OAuthClient.Scopes) != 1 || auditCredential.OAuthClient.Scopes[0] != "audit.ingest" || auditCredential.PlaintextSecret == "" {
		t.Fatalf("audit credential=%#v", auditCredential)
	}
	credential := credentials[ServiceCredentialOwnerDirectoryRead]
	client := credential.OAuthClient
	if credential.Purpose != ServiceCredentialOwnerDirectoryRead || client.ClientType != "service" || client.TokenAuthMethod != "client_secret_basic" || len(client.GrantTypes) != 1 || client.GrantTypes[0] != "client_credentials" || len(client.Scopes) != 1 || client.Scopes[0] != "owner_directory.read" {
		t.Fatalf("owner directory credential=%#v", credential)
	}
	if credential.PlaintextSecret == "" {
		t.Fatal("owner directory plaintext secret must be returned once during onboarding")
	}
	notificationCredential := credentials[ServiceCredentialNotificationIngest]
	if notificationCredential.Purpose != ServiceCredentialNotificationIngest || len(notificationCredential.OAuthClient.Scopes) != 1 || notificationCredential.OAuthClient.Scopes[0] != "notification.ingest" || notificationCredential.PlaintextSecret == "" {
		t.Fatalf("notification credential=%#v", notificationCredential)
	}
}

func (repository *subsystemOnboardingRepositoryStub) ListPortalApplications(_ context.Context, _, _, _ string) ([]PortalApplication, error) {
	repository.listCalls++
	return repository.portalItems, nil
}

func (repository *subsystemOnboardingRepositoryStub) ResolveApplicationEnvironment(context.Context, string, string, string) (string, string, error) {
	return "app-1", "env-1", nil
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
	if write.InitialAdminUserID != input.OperatorID {
		t.Fatalf("initial administrator = %q, want default operator %q", write.InitialAdminUserID, input.OperatorID)
	}
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
	publisher := write.CatalogPublisherOAuthClient
	if publisher.ClientID != "business-app-prod-catalog-publisher" || publisher.ClientType != "service" || publisher.TokenAuthMethod != "client_secret_basic" || publisher.RequirePKCE {
		t.Fatalf("unexpected catalog publisher client: %#v", publisher)
	}
	if len(publisher.GrantTypes) != 1 || publisher.GrantTypes[0] != "client_credentials" || len(publisher.Scopes) != 1 || publisher.Scopes[0] != "authorization.catalog.sync" || len(publisher.RedirectURIs) != 0 || publisher.RefreshTokenTTLSeconds != 0 {
		t.Fatalf("catalog publisher client has broader capabilities than expected: %#v", publisher)
	}
	if publisher.ClientID == write.OAuthClient.ClientID || publisher.ApplicationID != write.OAuthClient.ApplicationID || publisher.EnvironmentID != write.OAuthClient.EnvironmentID {
		t.Fatalf("catalog publisher must be a distinct client within the same application/environment: browser=%#v publisher=%#v", write.OAuthClient, publisher)
	}
	if result.PublicURL != "https://portal.example.com/business-app/" || result.PlaintextSecret == "" || result.CatalogPublisherPlaintextSecret == "" {
		t.Fatalf("unexpected integration result: public_url=%q has_secret=%t", result.PublicURL, result.PlaintextSecret != "")
	}
	if write.OAuthClientSecret == nil || len(write.OAuthClientSecret.SecretHash) == 0 {
		t.Fatal("expected only a protected client secret to cross the repository boundary")
	}
	if err := bcrypt.CompareHashAndPassword(write.OAuthClientSecret.SecretHash, []byte(result.PlaintextSecret)); err != nil {
		t.Fatalf("stored hash does not match returned one-time secret: %v", err)
	}
	if write.CatalogPublisherOAuthClientSecret == nil || len(write.CatalogPublisherOAuthClientSecret.SecretHash) == 0 {
		t.Fatal("expected a protected catalog publisher secret to cross the repository boundary")
	}
	if err := bcrypt.CompareHashAndPassword(write.CatalogPublisherOAuthClientSecret.SecretHash, []byte(result.CatalogPublisherPlaintextSecret)); err != nil {
		t.Fatalf("stored hash does not match catalog publisher secret: %v", err)
	}
	if result.PlaintextSecret == result.CatalogPublisherPlaintextSecret {
		t.Fatal("browser and catalog publisher clients must use independent secrets")
	}
}

func TestOnboardSubsystemCanonicalizesIntegratedCustomerPrototypeAddresses(t *testing.T) {
	repository := &subsystemOnboardingRepositoryStub{}
	service, err := NewSubsystemOnboardingService(repository, &sequentialManagementIDs{}, fixedSubsystemClock{now: time.Date(2026, time.August, 2, 0, 0, 0, 0, time.UTC)}, RedirectURIValidationPolicy{})
	if err != nil {
		t.Fatal(err)
	}
	result, err := service.OnboardSubsystem(context.Background(), SubsystemOnboardingInput{
		TenantID: "01K10A00000000000000000001", OperatorID: "01K10B00000000000000000001",
		ApplicationCode: integratedCustomerApplicationCode, ApplicationName: "客户与商机管理系统", Environment: "dev",
		PublicBaseURL: "http://localhost:8081", UpstreamURL: legacyCustomerUpstreamURL,
		PathPrefix: legacyCustomerPathPrefix, ClientType: "confidential",
	})
	if err != nil {
		t.Fatal(err)
	}
	if repository.write.Environment.UpstreamURL == nil || *repository.write.Environment.UpstreamURL != integratedCustomerUpstreamURL {
		t.Fatalf("canonical upstream = %#v", repository.write.Environment.UpstreamURL)
	}
	if repository.write.Environment.PathPrefix == nil || *repository.write.Environment.PathPrefix != integratedCustomerPathPrefix {
		t.Fatalf("canonical path prefix = %#v", repository.write.Environment.PathPrefix)
	}
	if result.RedirectURI != "http://localhost:8081/customer-opportunity/auth/callback" || result.PublicURL != "http://localhost:8081/customer-opportunity/" {
		t.Fatalf("canonical public registration = redirect %q public %q", result.RedirectURI, result.PublicURL)
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
