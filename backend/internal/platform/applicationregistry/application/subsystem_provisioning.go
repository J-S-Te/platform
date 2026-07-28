package application

import (
	"context"
	"errors"
)

// ErrSubsystemProvisioningUnavailable marks failures in the local deployment automation. The
// HTTP layer maps this sentinel to a dependency-unavailable response without exposing command
// output, filesystem paths, credentials, or other infrastructure details to the browser.
var ErrSubsystemProvisioningUnavailable = errors.New("subsystem provisioning unavailable")

// SubsystemProvisioningInput contains the generated integration values that must be delivered to
// the subsystem runtime. ClientSecret is deliberately confined to this in-process contract and
// must never be logged, returned from an API, or passed on a command line.
type SubsystemProvisioningInput struct {
	TenantID        string
	ApplicationCode string
	Environment     string
	Issuer          string
	ClientID        string
	ClientSecret    string
	RedirectURI     string
	PublicURL       string
	PathPrefix      string
	UpstreamURL     string
}

// SubsystemProvisioner validates and performs the local subsystem deployment workflow.
type SubsystemProvisioner interface {
	Preflight(context.Context, string) error
	Provision(context.Context, SubsystemProvisioningInput) error
}
