package application

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"
)

const (
	defaultSubsystemAccessTokenTTLSeconds  = 15 * 60
	defaultSubsystemRefreshTokenTTLSeconds = 30 * 24 * 60 * 60
	integratedCustomerApplicationCode      = "customer_and_opportunity"
	integratedCustomerPathPrefix           = "/customer-opportunity"
	integratedCustomerUpstreamURL          = "http://customer-api:8090"
	legacyCustomerPathPrefix               = "/customer_and_opportunity"
	legacyCustomerUpstreamURL              = "http://opportunity-api:8082"
	integratedPortalApplicationCode        = "customer_portal"
	integratedPortalPathPrefix             = "/customer-portal"
	integratedPortalUpstreamURL            = "http://portal-api:8091"
)

const (
	ServiceCredentialExternalUserProvision  = "external_user_provision"
	ServiceCredentialApplicationRoleAssign  = "application_role_assign"
	ServiceCredentialApplicationRoleRevoke  = "application_role_revoke"
	ServiceCredentialPortalMappingProvision = "portal_mapping_provision"
	ServiceCredentialPortalMappingDisable   = "portal_mapping_disable"
	ServiceCredentialPortalInviteVerify     = "portal_invite_verify"
)

// ErrSubsystemOnboardingAlreadyExists marks a create-only onboarding request that would
// overwrite an already registered application environment. Callers can use errors.Is to return
// a client-safe, actionable conflict without exposing credential or database details.
var ErrSubsystemOnboardingAlreadyExists = errors.New("subsystem onboarding environment already exists")

// SubsystemOnboardingConflict identifies the tenant-scoped resource which prevents a create-only
// onboarding request from proceeding. It deliberately carries only operator-supplied application
// and environment codes plus the non-sensitive lifecycle status; OAuth client credentials and IDs
// are never attached to this error or serialized to clients.
type SubsystemOnboardingConflict struct {
	ApplicationCode string
	Environment     string
	Status          string
}

func (conflict *SubsystemOnboardingConflict) Error() string {
	return fmt.Sprintf("subsystem onboarding environment already exists: application=%s environment=%s status=%s", conflict.ApplicationCode, conflict.Environment, conflict.Status)
}

// Is supports errors.Is(err, ErrSubsystemOnboardingAlreadyExists).
func (*SubsystemOnboardingConflict) Is(target error) bool {
	return target == ErrSubsystemOnboardingAlreadyExists
}

// SubsystemOnboardingInput contains the minimum information required to register a browser-facing
// subsystem. IDs, login target, OAuth callback and OAuth credentials are derived by the service so
// administrators do not have to coordinate four independent management APIs manually.
type SubsystemOnboardingInput struct {
	TenantID        string
	OperatorID      string
	ApplicationCode string
	ApplicationName string
	Description     *string
	Environment     string
	PublicBaseURL   string
	UpstreamURL     string
	PathPrefix      string
	ClientType      string
}

// SubsystemOnboardingWrite is the complete, validated aggregate passed to one atomic repository
// transaction. PlaintextSecret is intentionally excluded; only its bcrypt-protected SecretWrite
// may cross the persistence boundary.
type SubsystemOnboardingWrite struct {
	Application       ApplicationCreateInput
	ApplicationID     string
	Environment       EnvironmentCreateInput
	EnvironmentID     string
	LoginTarget       LoginTargetCreateInput
	LoginTargetID     string
	OAuthClient       OAuthClientCreateInput
	OAuthClientID     string
	OAuthClientSecret *SecretWrite

	// CatalogPublisherOAuthClient is an isolated machine client used only by the
	// subsystem to publish its authorization catalog. It must never be reused by
	// a browser OIDC callback or another operational integration.
	CatalogPublisherOAuthClient       OAuthClientCreateInput
	CatalogPublisherOAuthClientID     string
	CatalogPublisherOAuthClientSecret *SecretWrite

	// ServiceClients contains optional, purpose-bound machine clients required by an integrated
	// subsystem. Each client has one exact scope and an independent secret. Generic applications
	// receive no extra clients; customer_portal receives only the six integration capabilities it
	// needs for external identity provisioning and CRM/Portal communication.
	ServiceClients []SubsystemServiceClientWrite
}

type SubsystemServiceClientWrite struct {
	Purpose           string
	OAuthClient       OAuthClientCreateInput
	OAuthClientID     string
	OAuthClientSecret *SecretWrite
}

type SubsystemServiceCredential struct {
	Purpose         string
	OAuthClient     OAuthClientView
	PlaintextSecret string
}

// SubsystemOnboardingResult returns the newly created control-plane objects. PlaintextSecret is
// returned exactly once and must never be logged, persisted or included in later list responses.
type SubsystemOnboardingResult struct {
	Application Application
	Environment Environment
	LoginTarget LoginTargetManagementItem
	OAuthClient OAuthClientView
	// CatalogPublisherOAuthClient is returned without credential material for
	// auditability. Its plaintext secret is deliberately private to onboarding
	// and is forwarded only to the trusted deployment provisioner.
	CatalogPublisherOAuthClient     OAuthClientView
	PlaintextSecret                 string
	CatalogPublisherPlaintextSecret string
	ServiceCredentials              []SubsystemServiceCredential
	RedirectURI                     string
	PublicURL                       string
}

// PortalApplication is the safe, read-only projection shown on the authenticated subsystem portal.
type PortalApplication struct {
	ApplicationID string
	Code          string
	Name          string
	Description   *string
	EnvironmentID string
	Environment   string
	PathPrefix    *string
	TargetCode    string
	TargetURI     string
	PublicURL     string
}

// SubsystemOnboardingRepository persists an onboarding aggregate atomically and reads active
// portal registrations. Implementations must keep every query tenant-scoped.
type SubsystemOnboardingRepository interface {
	CreateSubsystem(context.Context, SubsystemOnboardingWrite, time.Time) (SubsystemOnboardingResult, error)
	ListPortalApplications(context.Context, string, string, string) ([]PortalApplication, error)
}

// SubsystemOnboardingService coordinates the simplified subsystem registration workflow.
type SubsystemOnboardingService struct {
	repository                  SubsystemOnboardingRepository
	ids                         ManagementIdentifierGenerator
	clock                       Clock
	redirectURIValidationPolicy RedirectURIValidationPolicy
}

// NewSubsystemOnboardingService constructs the simplified subsystem onboarding service.
func NewSubsystemOnboardingService(repository SubsystemOnboardingRepository, ids ManagementIdentifierGenerator, clock Clock, redirectURIValidationPolicy RedirectURIValidationPolicy) (*SubsystemOnboardingService, error) {
	if repository == nil || ids == nil || clock == nil {
		return nil, errors.New("subsystem onboarding dependencies must not be nil")
	}
	return &SubsystemOnboardingService{
		repository: repository, ids: ids, clock: clock, redirectURIValidationPolicy: redirectURIValidationPolicy,
	}, nil
}

// ValidateSubsystemOnboardingInput validates the public one-click onboarding contract without
// creating database records or credentials. It lets infrastructure preflight run before the atomic
// registration transaction.
func ValidateSubsystemOnboardingInput(input SubsystemOnboardingInput) error {
	input = normalizeSubsystemOnboardingInput(input)
	if !validSubsystemOnboardingInput(input) {
		return ErrValidation
	}
	return nil
}

// OnboardSubsystem validates and creates the application, environment, login target and OAuth
// client in one repository transaction.
func (service *SubsystemOnboardingService) OnboardSubsystem(ctx context.Context, input SubsystemOnboardingInput) (SubsystemOnboardingResult, error) {
	input = normalizeSubsystemOnboardingInput(input)
	if !validSubsystemOnboardingInput(input) {
		return SubsystemOnboardingResult{}, ErrValidation
	}

	publicURL := input.PublicBaseURL + input.PathPrefix + "/"
	redirectURI := input.PublicBaseURL + input.PathPrefix + "/auth/callback"
	clientID := input.ApplicationCode + "-" + input.Environment + "-web"
	tokenAuthMethod := "client_secret_basic"
	if input.ClientType == "public" {
		tokenAuthMethod = "none"
	}

	applicationInput := normalizeApplicationCreate(ApplicationCreateInput{
		TenantID: input.TenantID, OperatorID: input.OperatorID, Code: input.ApplicationCode,
		Name: input.ApplicationName, ApplicationType: "web", HomepageURL: stringPointer(publicURL),
		Description: input.Description, Status: "ACTIVE",
	})
	if !validApplicationCreate(applicationInput) {
		return SubsystemOnboardingResult{}, ErrValidation
	}

	now := service.clock.Now().UTC()
	applicationID, err := service.newID(now, "application")
	if err != nil {
		return SubsystemOnboardingResult{}, err
	}
	environmentID, err := service.newID(now, "application environment")
	if err != nil {
		return SubsystemOnboardingResult{}, err
	}
	loginTargetID, err := service.newID(now, "application login target")
	if err != nil {
		return SubsystemOnboardingResult{}, err
	}
	oauthClientID, err := service.newID(now, "OAuth client")
	if err != nil {
		return SubsystemOnboardingResult{}, err
	}
	catalogPublisherOAuthClientID, err := service.newID(now, "authorization catalog publisher OAuth client")
	if err != nil {
		return SubsystemOnboardingResult{}, err
	}

	environmentInput := normalizeEnvironmentCreate(EnvironmentCreateInput{
		TenantID: input.TenantID, OperatorID: input.OperatorID, ApplicationID: applicationID,
		Environment: input.Environment, BaseURL: stringPointer(input.PublicBaseURL),
		UpstreamURL: stringPointer(input.UpstreamURL), PathPrefix: stringPointer(input.PathPrefix), Status: "ACTIVE",
	})
	if !validEnvironmentCreate(environmentInput) {
		return SubsystemOnboardingResult{}, ErrValidation
	}
	loginTargetInput := normalizeLoginTargetCreate(LoginTargetCreateInput{
		TenantID: input.TenantID, OperatorID: input.OperatorID, ApplicationID: applicationID,
		EnvironmentID: environmentID, TargetCode: "home", Name: input.ApplicationName + "首页",
		TargetURI: input.PathPrefix + "/", Status: "ACTIVE",
	})
	if !validLoginTargetCreate(loginTargetInput) {
		return SubsystemOnboardingResult{}, ErrValidation
	}
	oauthClientInput, err := normalizeOAuthClientCreate(OAuthClientCreateInput{
		TenantID: input.TenantID, OperatorID: input.OperatorID, ApplicationID: applicationID,
		EnvironmentID: environmentID, ClientID: clientID, ClientName: input.ApplicationName + " Web",
		ClientType: input.ClientType, TokenAuthMethod: tokenAuthMethod,
		AccessTokenTTLSeconds:  defaultSubsystemAccessTokenTTLSeconds,
		RefreshTokenTTLSeconds: defaultSubsystemRefreshTokenTTLSeconds,
		RequirePKCE:            true, GrantTypes: []string{"authorization_code", "refresh_token"},
		Scopes: []string{"openid", "profile"}, RedirectURIs: []string{redirectURI},
	}, service.redirectURIValidationPolicy)
	if err != nil {
		return SubsystemOnboardingResult{}, err
	}

	var secret *SecretWrite
	plaintextSecret := ""
	if tokenAuthMethod == "client_secret_basic" {
		write, plaintext, secretErr := newOAuthClientSecretWrite(service.ids, now, nil)
		if secretErr != nil {
			return SubsystemOnboardingResult{}, secretErr
		}
		secret, plaintextSecret = &write, plaintext
	}

	// Every onboarded subsystem receives a separate confidential service client
	// for authorization-catalog publishing. This client is intentionally fixed
	// to client_credentials/client_secret_basic and has exactly one scope.
	catalogPublisherClientInput, err := normalizeOAuthClientCreate(OAuthClientCreateInput{
		TenantID: input.TenantID, OperatorID: input.OperatorID, ApplicationID: applicationID,
		EnvironmentID: environmentID, ClientID: input.ApplicationCode + "-" + input.Environment + "-catalog-publisher",
		ClientName: input.ApplicationName + " Authorization Catalog Publisher",
		ClientType: "service", TokenAuthMethod: "client_secret_basic",
		AccessTokenTTLSeconds:  defaultSubsystemAccessTokenTTLSeconds,
		RefreshTokenTTLSeconds: 0, RequirePKCE: false,
		GrantTypes: []string{"client_credentials"},
		Scopes:     []string{"authorization.catalog.sync"},
	}, service.redirectURIValidationPolicy)
	if err != nil {
		return SubsystemOnboardingResult{}, err
	}
	catalogPublisherSecretWrite, catalogPublisherPlaintextSecret, err := newOAuthClientSecretWrite(service.ids, now, nil)
	if err != nil {
		return SubsystemOnboardingResult{}, err
	}
	serviceClients, serviceCredentials, err := service.buildIntegratedServiceClients(input, applicationID, environmentID, now)
	if err != nil {
		return SubsystemOnboardingResult{}, err
	}

	result, err := service.repository.CreateSubsystem(ctx, SubsystemOnboardingWrite{
		Application: applicationInput, ApplicationID: applicationID,
		Environment: environmentInput, EnvironmentID: environmentID,
		LoginTarget: loginTargetInput, LoginTargetID: loginTargetID,
		OAuthClient: oauthClientInput, OAuthClientID: oauthClientID, OAuthClientSecret: secret,
		CatalogPublisherOAuthClient:       catalogPublisherClientInput,
		CatalogPublisherOAuthClientID:     catalogPublisherOAuthClientID,
		CatalogPublisherOAuthClientSecret: &catalogPublisherSecretWrite,
		ServiceClients:                    serviceClients,
	}, now)
	if err != nil {
		return SubsystemOnboardingResult{}, err
	}
	result.PlaintextSecret = plaintextSecret
	result.CatalogPublisherPlaintextSecret = catalogPublisherPlaintextSecret
	for index := range result.ServiceCredentials {
		if secret, ok := serviceCredentials[result.ServiceCredentials[index].Purpose]; ok {
			result.ServiceCredentials[index].PlaintextSecret = secret
		}
	}
	result.RedirectURI = redirectURI
	result.PublicURL = publicURL
	return result, nil
}

func (service *SubsystemOnboardingService) buildIntegratedServiceClients(input SubsystemOnboardingInput, applicationID, environmentID string, now time.Time) ([]SubsystemServiceClientWrite, map[string]string, error) {
	if input.ApplicationCode != integratedPortalApplicationCode {
		return nil, nil, nil
	}
	definitions := []struct {
		purpose string
		suffix  string
		name    string
		scope   string
	}{
		{ServiceCredentialExternalUserProvision, "external-user-provision", "External User Provisioner", "external_user.provision"},
		{ServiceCredentialApplicationRoleAssign, "role-assign", "Application Role Assigner", "application_role.assign"},
		{ServiceCredentialApplicationRoleRevoke, "role-revoke", "Application Role Revoker", "application_role.revoke"},
		{ServiceCredentialPortalMappingProvision, "portal-mapping-provision", "Portal Identity Mapping Provisioner", "portal.identity_mapping.provision"},
		{ServiceCredentialPortalMappingDisable, "portal-mapping-disable", "Portal Identity Mapping Disabler", "portal.identity_mapping.disable"},
		{ServiceCredentialPortalInviteVerify, "portal-invite-verify", "Portal Invite Verifier", "portal.invite.verify"},
	}
	writes := make([]SubsystemServiceClientWrite, 0, len(definitions))
	secrets := make(map[string]string, len(definitions))
	for _, definition := range definitions {
		client, err := normalizeOAuthClientCreate(OAuthClientCreateInput{
			TenantID: input.TenantID, OperatorID: input.OperatorID, ApplicationID: applicationID,
			EnvironmentID: environmentID,
			ClientID:      input.ApplicationCode + "-" + input.Environment + "-" + definition.suffix,
			ClientName:    input.ApplicationName + " " + definition.name,
			ClientType:    "service", TokenAuthMethod: "client_secret_basic",
			AccessTokenTTLSeconds: defaultSubsystemAccessTokenTTLSeconds, RequirePKCE: false,
			GrantTypes: []string{"client_credentials"}, Scopes: []string{definition.scope},
		}, service.redirectURIValidationPolicy)
		if err != nil {
			return nil, nil, err
		}
		clientID, err := service.newID(now, definition.purpose+" OAuth client")
		if err != nil {
			return nil, nil, err
		}
		secretWrite, plaintext, err := newOAuthClientSecretWrite(service.ids, now, nil)
		if err != nil {
			return nil, nil, err
		}
		writes = append(writes, SubsystemServiceClientWrite{
			Purpose: definition.purpose, OAuthClient: client, OAuthClientID: clientID,
			OAuthClientSecret: &secretWrite,
		})
		secrets[definition.purpose] = plaintext
	}
	return writes, secrets, nil
}

// ListPortalApplications returns active, resolvable subsystem cards for the authenticated tenant.
// When environment is blank the repository applies its deterministic environment preference.
func (service *SubsystemOnboardingService) ListPortalApplications(ctx context.Context, tenantID, userID, environment string) ([]PortalApplication, error) {
	tenantID = strings.TrimSpace(tenantID)
	userID = strings.TrimSpace(userID)
	environment = strings.ToLower(strings.TrimSpace(environment))
	if tenantID == "" || userID == "" || (environment != "" && !validEnvironmentCode(environment)) {
		return nil, ErrValidation
	}
	items, err := service.repository.ListPortalApplications(ctx, tenantID, userID, environment)
	if err != nil {
		return nil, err
	}
	result := make([]PortalApplication, 0, len(items))
	for _, item := range items {
		publicURL, resolveErr := resolvePortalTarget(item.PublicURL, item.TargetURI)
		if resolveErr != nil {
			continue
		}
		item.PublicURL = publicURL
		result = append(result, item)
	}
	return result, nil
}

func (service *SubsystemOnboardingService) newID(now time.Time, resource string) (string, error) {
	identifier, err := service.ids.New(now)
	if err != nil {
		return "", fmt.Errorf("generate %s ID: %w", resource, err)
	}
	return identifier, nil
}

func normalizeSubsystemOnboardingInput(input SubsystemOnboardingInput) SubsystemOnboardingInput {
	input.TenantID = strings.TrimSpace(input.TenantID)
	input.OperatorID = strings.TrimSpace(input.OperatorID)
	input.ApplicationCode = strings.ToLower(strings.TrimSpace(input.ApplicationCode))
	input.ApplicationName = strings.TrimSpace(input.ApplicationName)
	input.Description = normalizeOptional(input.Description)
	input.Environment = strings.ToLower(strings.TrimSpace(input.Environment))
	if input.Environment == "" {
		input.Environment = "prod"
	}
	input.PublicBaseURL = strings.TrimRight(strings.TrimSpace(input.PublicBaseURL), "/")
	input.UpstreamURL = strings.TrimRight(strings.TrimSpace(input.UpstreamURL), "/")
	input.PathPrefix = strings.TrimRight(strings.TrimSpace(input.PathPrefix), "/")
	if input.PathPrefix == "" && input.ApplicationCode != "" {
		input.PathPrefix = "/" + input.ApplicationCode
	}
	// The local workspace ships customer_and_opportunity inside the unified frontend/Compose
	// topology. Accept the values used by the original standalone prototype, but persist the
	// canonical route and Docker network alias so OAuth callbacks and gateway routing agree.
	if input.ApplicationCode == integratedCustomerApplicationCode &&
		input.PathPrefix == legacyCustomerPathPrefix && input.UpstreamURL == legacyCustomerUpstreamURL {
		input.PathPrefix = integratedCustomerPathPrefix
		input.UpstreamURL = integratedCustomerUpstreamURL
	}
	if input.ApplicationCode == integratedPortalApplicationCode {
		if input.PathPrefix == "/customer_portal" {
			input.PathPrefix = integratedPortalPathPrefix
		}
		if input.UpstreamURL == "http://customer-portal-api:8091" {
			input.UpstreamURL = integratedPortalUpstreamURL
		}
	}
	input.ClientType = strings.ToLower(strings.TrimSpace(input.ClientType))
	if input.ClientType == "" {
		input.ClientType = "confidential"
	}
	return input
}

func validSubsystemOnboardingInput(input SubsystemOnboardingInput) bool {
	baseURL, upstreamURL, pathPrefix := input.PublicBaseURL, input.UpstreamURL, input.PathPrefix
	return input.TenantID != "" && input.OperatorID != "" && validCode(input.ApplicationCode, 64) &&
		validManagementText(input.ApplicationName, 128, false) && validEnvironmentCode(input.Environment) &&
		validOptionalBaseURL(&baseURL) && validOptionalUpstreamURL(&upstreamURL) &&
		validOptionalPathPrefix(&pathPrefix) && validGatewayTripleConsistent(&baseURL, &upstreamURL, &pathPrefix) &&
		oneOf(input.ClientType, "public", "confidential")
}

func resolvePortalTarget(baseURL, targetURI string) (string, error) {
	targetURI = strings.TrimSpace(targetURI)
	parsed, err := url.Parse(targetURI)
	if err != nil {
		return "", err
	}
	if parsed.IsAbs() {
		if !validRedirectURI(targetURI, RedirectURIValidationPolicy{}) {
			return "", ErrValidation
		}
		return targetURI, nil
	}
	baseURL = strings.TrimRight(strings.TrimSpace(baseURL), "/")
	if baseURL == "" || !strings.HasPrefix(targetURI, "/") {
		return "", ErrValidation
	}
	return baseURL + targetURI, nil
}

func stringPointer(value string) *string { return &value }
