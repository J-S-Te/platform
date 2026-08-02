package application

import (
	"context"
	"errors"
	"time"
)

// ErrSubsystemProvisioningUnavailable marks failures in the local deployment automation. The
// HTTP layer maps this sentinel to a dependency-unavailable response without exposing command
// output, filesystem paths, credentials, or other infrastructure details to the browser.
var ErrSubsystemProvisioningUnavailable = errors.New("subsystem provisioning unavailable")

const (
	SubsystemDeploymentStatusProvisioning = "PROVISIONING"
	SubsystemDeploymentStatusUpdating     = "UPDATING"
	SubsystemDeploymentStatusVerifying    = "VERIFYING"
	SubsystemDeploymentStatusReady        = "READY"
	SubsystemDeploymentStatusFailed       = "PROVISION_FAILED"
	SubsystemDeploymentStatusDraining     = "DRAINING"
	SubsystemDeploymentStatusOffboarded   = "OFFBOARDED"
)

// SubsystemDeploymentState is the durable control-plane view of the last deployment attempt.
// It deliberately contains no credentials or infrastructure command output.
type SubsystemDeploymentState struct {
	TenantID        string
	ApplicationID   string
	EnvironmentID   string
	ApplicationCode string
	Environment     string
	Status          string
	Operation       string
	Generation      uint64
	AttemptCount    uint
	LastErrorCode   string
	LastError       string
	StartedAt       *time.Time
	CompletedAt     *time.Time
	CreatedAt       time.Time
	UpdatedAt       time.Time
}

// SubsystemDeploymentStateStore persists lifecycle transitions independently from the slow
// deployment Agent. This lets an operator retry a failed deployment without repeating onboarding.
type SubsystemDeploymentStateStore interface {
	TransitionSubsystemDeployment(context.Context, string, string, string, string, string, string, string, time.Time) error
	GetSubsystemDeploymentState(context.Context, string, string, string) (SubsystemDeploymentState, error)
}

// SubsystemProvisioningInput contains the generated integration values that must be delivered to
// the subsystem runtime. ClientSecret is deliberately confined to this in-process contract and
// must never be logged, returned from an API, or passed on a command line.
type SubsystemProvisioningInput struct {
	TenantID        string
	ApplicationID   string
	ApplicationCode string
	Environment     string
	Issuer          string
	ClientID        string
	ClientSecret    string
	// CatalogPublisherClientID and CatalogPublisherClientSecret are a separate
	// service credential for authorization catalog synchronization.
	CatalogPublisherClientID     string
	CatalogPublisherClientSecret string
	// ServiceCredentials are one-time, purpose-bound credentials created during onboarding.
	// They are delivered only to the isolated deployment Agent and written to mode-0600 runtime
	// environment files; the browser response and operational logs never receive them.
	ServiceCredentials []SubsystemServiceCredential
	RedirectURI        string
	PublicURL          string
	PathPrefix         string
	UpstreamURL        string
}

func (input SubsystemProvisioningInput) ServiceCredential(purpose string) (SubsystemServiceCredential, bool) {
	for _, credential := range input.ServiceCredentials {
		if credential.Purpose == purpose && credential.OAuthClient.ClientID != "" && credential.PlaintextSecret != "" {
			return credential, true
		}
	}
	return SubsystemServiceCredential{}, false
}

// SubsystemProvisioner validates and performs the local subsystem deployment workflow.
//
// Lifecycle (called via Unix-socket transport from the API process):
//   - Preflight: cheap environment checks before any state-changing operation.
//   - Provision: full atomic onboard (write .env.local + docker compose up + reload nginx).
//   - Update:    re-apply the integration to a subsystem that is already onboarded (rewrite
//     .env.local, rebuild containers, reload nginx). DB rows are not touched; caller must
//     have already updated them via PATCH /environments when BaseURL/UpstreamURL/PathPrefix
//     changed.
//   - Teardown:  full atomic offboard of the subsystem (docker compose down + remove
//     .env.local + remove gateway include + reload nginx). DB rows are not touched; the
//     HTTP layer is responsible for the subsequent DELETE on /environments and
//     /applications.
type SubsystemProvisioner interface {
	Preflight(context.Context, string) error
	Provision(context.Context, SubsystemProvisioningInput) error
	Update(context.Context, SubsystemProvisioningInput) error
	Teardown(context.Context, string, string) error
}
