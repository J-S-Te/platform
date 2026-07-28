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
)

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
}

// SubsystemOnboardingResult returns the newly created control-plane objects. PlaintextSecret is
// returned exactly once and must never be logged, persisted or included in later list responses.
type SubsystemOnboardingResult struct {
	Application     Application
	Environment     Environment
	LoginTarget     LoginTargetManagementItem
	OAuthClient     OAuthClientView
	PlaintextSecret string
	RedirectURI     string
	PublicURL       string
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
	ListPortalApplications(context.Context, string, string) ([]PortalApplication, error)
}

// SubsystemOnboardingService coordinates the simplified subsystem registration workflow.
type SubsystemOnboardingService struct {
	repository SubsystemOnboardingRepository
	ids        ManagementIdentifierGenerator
	clock      Clock
}

// NewSubsystemOnboardingService constructs the simplified subsystem onboarding service.
func NewSubsystemOnboardingService(repository SubsystemOnboardingRepository, ids ManagementIdentifierGenerator, clock Clock) (*SubsystemOnboardingService, error) {
	if repository == nil || ids == nil || clock == nil {
		return nil, errors.New("subsystem onboarding dependencies must not be nil")
	}
	return &SubsystemOnboardingService{repository: repository, ids: ids, clock: clock}, nil
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
	})
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

	result, err := service.repository.CreateSubsystem(ctx, SubsystemOnboardingWrite{
		Application: applicationInput, ApplicationID: applicationID,
		Environment: environmentInput, EnvironmentID: environmentID,
		LoginTarget: loginTargetInput, LoginTargetID: loginTargetID,
		OAuthClient: oauthClientInput, OAuthClientID: oauthClientID, OAuthClientSecret: secret,
	}, now)
	if err != nil {
		return SubsystemOnboardingResult{}, err
	}
	result.PlaintextSecret = plaintextSecret
	result.RedirectURI = redirectURI
	result.PublicURL = publicURL
	return result, nil
}

// ListPortalApplications returns active, resolvable subsystem cards for the authenticated tenant.
// When environment is blank the repository applies its deterministic environment preference.
func (service *SubsystemOnboardingService) ListPortalApplications(ctx context.Context, tenantID, environment string) ([]PortalApplication, error) {
	tenantID, environment = strings.TrimSpace(tenantID), strings.ToLower(strings.TrimSpace(environment))
	if tenantID == "" || (environment != "" && !validEnvironmentCode(environment)) {
		return nil, ErrValidation
	}
	items, err := service.repository.ListPortalApplications(ctx, tenantID, environment)
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
		if !validRedirectURI(targetURI) {
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
