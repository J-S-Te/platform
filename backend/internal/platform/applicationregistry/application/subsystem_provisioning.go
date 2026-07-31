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
	RedirectURI                  string
	PublicURL                    string
	PathPrefix                   string
	UpstreamURL                  string
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
