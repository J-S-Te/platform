package application

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"

	"github.com/J-S-Te/Basic-Platform/internal/platform/license/coordination"
	"github.com/J-S-Te/Basic-Platform/internal/platform/license/domain"
)

// RuntimeLicenseApproval is supplied by the isolated Agent's reviewed release,
// never by an HTTP onboarding request or a mutable image label.
type RuntimeLicenseApproval struct {
	ServiceID      string `json:"service_id"`
	Protocol       int    `json:"protocol"`
	CoverageDigest string `json:"coverage_digest"`
	ImageDigest    string `json:"image_digest"`
}
type RuntimeLicenseCredential struct {
	Approval      RuntimeLicenseApproval `json:"approval"`
	ClientID      string                 `json:"client_id"`
	ClientSecret  string                 `json:"client_secret"`
	ReleaseDigest string                 `json:"release_digest,omitempty"`
}
type RuntimeLicenseSettings struct {
	InstanceID            string
	Environment           string
	PlatformBaseURL       string
	PlatformPublicKeyPath string
	StateDirectory        string
	AllowHTTP             bool
}
type RuntimeLicenseApprovalSource interface {
	ApprovedRuntimeLicenseComponents(context.Context, string, string) ([]RuntimeLicenseApproval, error)
}
type RuntimeLicenseReleaseApproval struct {
	ReleaseGeneration  string                   `json:"release_generation,omitempty"`
	Components         []RuntimeLicenseApproval `json:"components"`
	RequiredServiceIDs []string                 `json:"required_service_ids"`
	RetireServiceIDs   []string                 `json:"retire_service_ids"`
}
type RuntimeLicenseLifecycleApprovalSource interface {
	ApprovedRuntimeLicenseLifecycle(context.Context, string, string) (RuntimeLicenseReleaseApproval, error)
}
type runtimeLicenseLifecycleVerifier struct {
	source RuntimeLicenseLifecycleApprovalSource
}

func NewRuntimeLicenseLifecycleVerifier(source RuntimeLicenseLifecycleApprovalSource) (coordination.LifecycleApprovalVerifier, error) {
	if source == nil {
		return nil, ErrValidation
	}
	return runtimeLicenseLifecycleVerifier{source}, nil
}
func RuntimeLicenseClientID(app, env string, item RuntimeLicenseApproval) string {
	return runtimeLicenseClientIDWithGeneration(app, env, item, "")
}
func runtimeLicenseClientIDWithGeneration(app, env string, item RuntimeLicenseApproval, generation string) string {
	raw, _ := json.Marshal([]any{app, env, item.ServiceID, item.Protocol, item.CoverageDigest, item.ImageDigest, generation})
	digest := sha256.Sum256(raw)
	prefix := app + "-" + env + "-license-" + item.ServiceID
	if len(prefix) > 95 {
		prefix = prefix[:95]
	}
	return prefix + "-" + hex.EncodeToString(digest[:16])
}
func canonicalRuntimeRelease(release RuntimeLicenseReleaseApproval) (RuntimeLicenseReleaseApproval, string, error) {
	if ValidateRuntimeLicenseApprovals(release.Components) != nil || len(release.RequiredServiceIDs) == 0 || (release.ReleaseGeneration != "" && !runtimeLicenseID.MatchString(release.ReleaseGeneration)) {
		return release, "", ErrValidation
	}
	release.Components = append([]RuntimeLicenseApproval(nil), release.Components...)
	release.RequiredServiceIDs = append([]string(nil), release.RequiredServiceIDs...)
	release.RetireServiceIDs = append([]string(nil), release.RetireServiceIDs...)
	ids := map[string]bool{}
	for _, item := range release.Components {
		ids[item.ServiceID] = true
	}
	seen := map[string]bool{}
	for _, id := range release.RequiredServiceIDs {
		if !ids[id] || seen[id] {
			return release, "", ErrValidation
		}
		seen[id] = true
	}
	seen = map[string]bool{}
	for _, id := range release.RetireServiceIDs {
		if !runtimeLicenseID.MatchString(id) || ids[id] || seen[id] {
			return release, "", ErrValidation
		}
		seen[id] = true
	}
	sort.Slice(release.Components, func(i, j int) bool { return release.Components[i].ServiceID < release.Components[j].ServiceID })
	sort.Strings(release.RequiredServiceIDs)
	sort.Strings(release.RetireServiceIDs)
	raw, err := json.Marshal(release)
	if err != nil {
		return release, "", err
	}
	digest := sha256.Sum256(raw)
	return release, "sha256:" + hex.EncodeToString(digest[:]), nil
}

// RuntimeLicenseReleaseDigest binds delivery and retirement to the same complete
// approval as coordination CAS, including selection, required IDs and generation.
func RuntimeLicenseReleaseDigest(release RuntimeLicenseReleaseApproval) (string, error) {
	_, digest, err := canonicalRuntimeRelease(release)
	return digest, err
}
func (v runtimeLicenseLifecycleVerifier) VerifyRuntimeLifecycle(ctx context.Context, in coordination.LifecycleInput) (coordination.LifecycleApproval, error) {
	release, err := v.source.ApprovedRuntimeLicenseLifecycle(ctx, in.Application, in.Environment)
	if err != nil {
		return coordination.LifecycleApproval{}, err
	}
	release, digest, err := canonicalRuntimeRelease(release)
	if err != nil || digest != in.ReleaseDigest {
		return coordination.LifecycleApproval{}, coordination.ErrForbidden
	}
	approved := coordination.LifecycleApproval{RequiredServiceIDs: release.RequiredServiceIDs, RetireServiceIDs: release.RetireServiceIDs}
	for _, item := range release.Components {
		approved.Specs = append(approved.Specs, coordination.ServiceSpec{Application: in.Application, Environment: in.Environment, ServiceID: item.ServiceID, OAuthClientID: runtimeLicenseClientIDWithGeneration(in.Application, in.Environment, item, release.ReleaseGeneration), CoverageDigest: item.CoverageDigest, ImageDigest: item.ImageDigest})
	}
	return approved, nil
}

type runtimeLicenseLifecycleCoordinator interface {
	Status(context.Context, string) (coordination.Status, error)
	ReconcileComponents(context.Context, coordination.LifecycleInput, domain.Actor) error
}
type RuntimeLicenseClientManager interface {
	ListOAuthClients(context.Context, string) ([]OAuthClientView, error)
	CreateOAuthClient(context.Context, OAuthClientCreateInput) (OAuthClientCreateResult, error)
	CreateOAuthClientSecret(context.Context, OAuthClientSecretCreateInput) (OAuthClientSecretResult, error)
}
type RuntimeLicenseRegistrar interface {
	Register(context.Context, coordination.ServiceSpec) error
}
type RuntimeLicenseEnrollmentService struct {
	source    RuntimeLicenseApprovalSource
	clients   RuntimeLicenseClientManager
	registrar RuntimeLicenseRegistrar
}

var ErrRuntimeLicenseNotInitialized = errors.New("commercial runtime instance not initialized")

func NewRuntimeLicenseEnrollmentService(source RuntimeLicenseApprovalSource, clients RuntimeLicenseClientManager, registrar RuntimeLicenseRegistrar) (*RuntimeLicenseEnrollmentService, error) {
	if source == nil || clients == nil || registrar == nil {
		return nil, ErrValidation
	}
	return &RuntimeLicenseEnrollmentService{source, clients, registrar}, nil
}

var runtimeLicenseID = regexp.MustCompile(`^[a-z][a-z0-9_-]{0,63}$`)
var runtimeLicenseDigest = regexp.MustCompile(`^sha256:[a-f0-9]{64}$`)

func ValidateRuntimeLicenseApprovals(items []RuntimeLicenseApproval) error {
	if len(items) == 0 || len(items) > 32 {
		return ErrValidation
	}
	seen := map[string]bool{}
	for _, item := range items {
		if !runtimeLicenseID.MatchString(item.ServiceID) || item.Protocol != 1 || !runtimeLicenseDigest.MatchString(item.CoverageDigest) || !runtimeLicenseDigest.MatchString(item.ImageDigest) || seen[item.ServiceID] {
			return ErrValidation
		}
		seen[item.ServiceID] = true
	}
	return nil
}

// Prepare delivers independent machine identities from a reviewed complete
// release. Existing membership changes use explicit CAS reconciliation, never
// Register overwrite. Activated applications remain restrictive during rollout.
func (s *RuntimeLicenseEnrollmentService) Prepare(ctx context.Context, tenant, applicationID, environmentID, app, env, operator string) ([]RuntimeLicenseCredential, error) {
	if ctx == nil || strings.TrimSpace(tenant) == "" || applicationID == "" || environmentID == "" || operator == "" || !runtimeLicenseID.MatchString(app) || !runtimeLicenseID.MatchString(env) {
		return nil, ErrValidation
	}
	items, err := s.source.ApprovedRuntimeLicenseComponents(ctx, app, env)
	if err != nil {
		return nil, errors.New("commercial runtime release approval unavailable")
	}
	if ValidateRuntimeLicenseApprovals(items) != nil {
		return nil, ErrValidation
	}
	var release RuntimeLicenseReleaseApproval
	var releaseDigest string
	if source, ok := s.source.(RuntimeLicenseLifecycleApprovalSource); ok {
		release, err = source.ApprovedRuntimeLicenseLifecycle(ctx, app, env)
		if err != nil {
			return nil, errors.New("commercial runtime lifecycle approval unavailable")
		}
		release, releaseDigest, err = canonicalRuntimeRelease(release)
		if err != nil {
			return nil, err
		}
		// Re-reading a mutable approval cannot combine credentials from one
		// release with retirement permissions from another.
		base, _, e := canonicalRuntimeRelease(RuntimeLicenseReleaseApproval{Components: items, RequiredServiceIDs: release.RequiredServiceIDs})
		if e != nil {
			return nil, e
		}
		baseJSON, _ := json.Marshal(base.Components)
		releaseJSON, _ := json.Marshal(release.Components)
		if string(baseJSON) != string(releaseJSON) {
			return nil, ErrConflict
		}
		items = release.Components
	}
	clients, err := s.clients.ListOAuthClients(ctx, tenant)
	if err != nil {
		return nil, errors.New("commercial runtime client lookup failed")
	}
	byID := map[string]OAuthClientView{}
	for _, c := range clients {
		byID[c.ClientID] = c
	}
	result := make([]RuntimeLicenseCredential, 0, len(items))
	specs := make([]coordination.ServiceSpec, 0, len(items))
	rotate := make([]OAuthClientView, 0, len(items))
	for _, item := range items {
		id := runtimeLicenseClientIDWithGeneration(app, env, item, release.ReleaseGeneration)
		client, found := byID[id]
		var secret string
		if !found {
			created, e := s.clients.CreateOAuthClient(ctx, OAuthClientCreateInput{TenantID: tenant, ApplicationID: applicationID, EnvironmentID: environmentID, OperatorID: operator, ClientID: id, ClientName: app + " Runtime " + item.ServiceID, ClientType: "service", TokenAuthMethod: "client_secret_basic", AccessTokenTTLSeconds: 300, GrantTypes: []string{"client_credentials"}, Scopes: []string{"license.runtime"}})
			if e != nil {
				return nil, errors.New("commercial runtime client creation failed")
			}
			client, secret = created.Client, created.PlaintextSecret
		}
		if client.ClientID != id || client.ApplicationID != applicationID || client.EnvironmentID != environmentID || client.Status != "ACTIVE" || client.ClientType != "service" || client.TokenAuthMethod != "client_secret_basic" || len(client.Scopes) != 1 || client.Scopes[0] != "license.runtime" || len(client.GrantTypes) != 1 || client.GrantTypes[0] != "client_credentials" {
			return nil, ErrConflict
		}
		if client.ClientID == "" || (!found && secret == "") {
			return nil, errors.New("commercial runtime credential incomplete")
		}
		specs = append(specs, coordination.ServiceSpec{Application: app, Environment: env, ServiceID: item.ServiceID, OAuthClientID: client.ClientID, CoverageDigest: item.CoverageDigest, ImageDigest: item.ImageDigest})
		if found {
			rotate = append(rotate, client)
		} else {
			rotate = append(rotate, OAuthClientView{})
		}
		result = append(result, RuntimeLicenseCredential{Approval: item, ClientID: client.ClientID, ClientSecret: secret, ReleaseDigest: releaseDigest})
	}
	registered := false
	if coordinator, ok := s.registrar.(runtimeLicenseLifecycleCoordinator); ok {
		status, e := coordinator.Status(ctx, app)
		if e != nil && !errors.Is(e, coordination.ErrNotReady) {
			return nil, errors.New("commercial runtime component status unavailable")
		}
		if e == nil {
			current := map[string]coordination.ServiceSpec{}
			for _, member := range status.Members {
				if member.RetiredAt == nil {
					current[member.ServiceID] = coordination.ServiceSpec{Application: member.Application, Environment: member.Environment, ServiceID: member.ServiceID, OAuthClientID: member.OAuthClientID, CoverageDigest: member.CoverageDigest, ImageDigest: member.ImageDigest}
				}
			}
			changed := len(current) != len(specs)
			for _, spec := range specs {
				if current[spec.ServiceID] != spec {
					changed = true
				}
			}
			if changed {
				if releaseDigest == "" {
					return nil, errors.New("commercial runtime explicit replacement approval required")
				}
				operationDigest := sha256.Sum256([]byte(app + "\x00" + env + "\x00" + releaseDigest))
				operationID := fmt.Sprintf("release-%x-%d", operationDigest, status.Revision)
				if e = coordinator.ReconcileComponents(ctx, coordination.LifecycleInput{Application: app, Environment: env, ExpectedRevision: status.Revision, OperationID: operationID, ReleaseDigest: releaseDigest}, domain.Actor{TenantID: tenant, UserID: operator}); e != nil {
					return nil, errors.New("commercial runtime controlled component replacement failed")
				}
			}
			registered = true
		}
	}
	if !registered {
		for _, spec := range specs {
			if e := s.registrar.Register(ctx, spec); e != nil {
				return nil, errors.New("commercial runtime component registration failed")
			}
		}
	}
	// Rotate only after registration/CAS succeeds; rejected releases must not
	// destroy a currently working component credential.
	for i, client := range rotate {
		if client.ID != "" {
			rotated, e := s.clients.CreateOAuthClientSecret(ctx, OAuthClientSecretCreateInput{TenantID: tenant, OAuthClientID: client.ID, OperatorID: operator})
			if e != nil || rotated.PlaintextSecret == "" {
				return nil, errors.New("commercial runtime credential delivery failed")
			}
			result[i].ClientSecret = rotated.PlaintextSecret
		}
	}
	return result, nil
}
