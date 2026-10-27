package infrastructure

import (
	"context"
	"errors"
	"fmt"
	"github.com/J-S-Te/Basic-Platform/internal/platform/applicationregistry/application"
)

// SubsystemAccessRuntimeEnrollment reuses the same enrollment as the legacy
// authenticated onboarding handler. Credentials are passed in memory to the Agent.
type SubsystemAccessRuntimeEnrollment interface {
	PrepareWithSettings(context.Context, string, string, string, string, string, string, application.RuntimeLicenseSettings) ([]application.RuntimeLicenseCredential, error)
	WaitForRuntimeConfirmation(context.Context, string, string, []application.RuntimeLicenseCredential) error
}

func (a *SubsystemAccessDeploymentAdapter) SetRuntimeLicenseEnrollment(service SubsystemAccessRuntimeEnrollment, settings func(context.Context) (application.RuntimeLicenseSettings, error), local bool) {
	a.runtimeLicenses, a.runtimeSettings, a.runtimeLocal = service, settings, local
}
func (a *SubsystemAccessDeploymentAdapter) prepareRuntimeLicense(ctx context.Context, op application.SubsystemAccessOperation, input *application.SubsystemProvisioningInput) error {
	if a.runtimeLicenses == nil {
		return nil
	} // Backwards-compatible for explicitly unlicensed embeddings.
	if a.runtimeSettings == nil {
		return application.ErrValidation
	}
	settings, err := a.runtimeSettings(ctx)
	if errors.Is(err, application.ErrRuntimeLicenseNotInitialized) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("read runtime license settings: %w", err)
	}
	credentials, err := a.runtimeLicenses.PrepareWithSettings(ctx, op.TenantID, op.ApplicationID, op.EnvironmentID, op.ApplicationCode, op.EnvironmentCode, op.OperatorID, settings)
	if err != nil {
		return fmt.Errorf("enroll runtime license components: %w", err)
	}
	if len(credentials) == 0 {
		return application.ErrValidation
	}
	input.RuntimeLicenseSettings, input.RuntimeLicenseCredentials = settings, credentials
	return nil
}
func (a *SubsystemAccessDeploymentAdapter) confirmRuntimeLicense(ctx context.Context, op application.SubsystemAccessOperation, input application.SubsystemProvisioningInput) error {
	if !a.runtimeLocal || len(input.RuntimeLicenseCredentials) == 0 {
		return nil
	}
	return a.runtimeLicenses.WaitForRuntimeConfirmation(ctx, op.ApplicationCode, op.EnvironmentCode, input.RuntimeLicenseCredentials)
}
