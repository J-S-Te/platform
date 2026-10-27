package bootstrap

import (
	"context"
	"errors"
	"strings"
	"time"

	registry "github.com/J-S-Te/Basic-Platform/internal/platform/applicationregistry/application"
	registryinfra "github.com/J-S-Te/Basic-Platform/internal/platform/applicationregistry/infrastructure"
	license "github.com/J-S-Te/Basic-Platform/internal/platform/license/application"
	"github.com/J-S-Te/Basic-Platform/internal/platform/license/coordination"
	"github.com/J-S-Te/Basic-Platform/internal/platform/license/domain"
	licenseinfra "github.com/J-S-Te/Basic-Platform/internal/platform/license/infrastructure"
	"github.com/J-S-Te/Basic-Platform/internal/platform/license/trust"
	"github.com/J-S-Te/Basic-Platform/internal/shared/config"
	"github.com/J-S-Te/Basic-Platform/internal/shared/security"
	"gorm.io/gorm"
)

// Access jobs execute in the independent Worker, not in the HTTP handler. Build
// the same persisted coordinator here; signing keys are loaded only when an
// initialized installation actually requests a controlled enrollment.
type subsystemAccessRuntimeEnrollment struct {
	images     registryinfra.SubsystemAccessImageReader
	production bool
	build      func() (*registry.RuntimeLicenseEnrollmentService, error)
}

func (p subsystemAccessRuntimeEnrollment) PrepareWithSettings(ctx context.Context, tenant, appID, envID, app, env, operator string, settings registry.RuntimeLicenseSettings) ([]registry.RuntimeLicenseCredential, error) {
	if p.production {
		ids, err := subsystemAccessSelectedRuntimeServices(ctx, p.images, app, env)
		if err != nil {
			return nil, err
		}
		settings.ServiceIDs = ids
	}
	service, err := p.build()
	if err != nil {
		return nil, err
	}
	return service.PrepareWithSettings(ctx, tenant, appID, envID, app, env, operator, settings)
}
func (p subsystemAccessRuntimeEnrollment) WaitForRuntimeConfirmation(ctx context.Context, app, env string, credentials []registry.RuntimeLicenseCredential) error {
	service, err := p.build()
	if err != nil {
		return err
	}
	return service.WaitForRuntimeConfirmation(ctx, app, env, credentials)
}
func configureSubsystemAccessRuntimeEnrollment(cfg config.Config, db *gorm.DB, clients registry.RuntimeLicenseClientManager, source interface {
	registry.RuntimeLicenseApprovalSource
	registryinfra.SubsystemAccessImageReader
}, adapter *registryinfra.SubsystemAccessDeploymentAdapter) error {
	repository, err := licenseinfra.NewRepository(db)
	if err != nil {
		return err
	}
	reader, err := license.NewService(repository, trust.Keys(), time.Now)
	if err != nil {
		return err
	}
	port := subsystemAccessRuntimeEnrollment{images: source, production: strings.EqualFold(cfg.SubsystemOnboarding.Mode, "production"), build: func() (*registry.RuntimeLicenseEnrollmentService, error) {
		signer, err := security.LoadApplicationJWTManager(cfg.Auth.JWTIssuer, cfg.Auth.ApplicationJWTAudience, cfg.Auth.JWTPrivateKeyPath, cfg.Auth.JWTPublicKeyPath)
		if err != nil {
			return nil, err
		}
		options := []coordination.Option{coordination.WithApplicationEnvironment(cfg.Audit.EnvironmentCode), coordination.WithBuiltinComponentSource(registryinfra.BuiltinRuntimeComponentCatalog{})}
		if lifecycle, ok := source.(registry.RuntimeLicenseLifecycleApprovalSource); ok {
			verifier, err := registry.NewRuntimeLicenseLifecycleVerifier(lifecycle)
			if err != nil {
				return nil, err
			}
			options = append(options, coordination.WithLifecycleApprovalVerifier(verifier))
		}
		coordinator, err := coordination.NewService(db, reader, trust.Keys(), licenseSnapshotSigner{signer, trust.Keys()}, time.Now, options...)
		if err != nil {
			return nil, err
		}
		return registry.NewRuntimeLicenseEnrollmentServiceWithCatalog(source, registryinfra.BuiltinRuntimeComponentCatalog{}, clients, coordinator)
	}}
	adapter.SetRuntimeLicenseEnrollment(port, func(ctx context.Context) (registry.RuntimeLicenseSettings, error) {
		state, err := reader.Read(ctx)
		if errors.Is(err, domain.ErrNotInitialized) {
			return registry.RuntimeLicenseSettings{}, registry.ErrRuntimeLicenseNotInitialized
		}
		if err != nil {
			return registry.RuntimeLicenseSettings{}, err
		}
		return registry.RuntimeLicenseSettings{InstanceID: state.InstanceID, Environment: state.Environment, PlatformBaseURL: cfg.CommercialRuntime.PlatformBaseURL, PlatformPublicKeyPath: cfg.CommercialRuntime.PlatformPublicKeyPath, StateDirectory: cfg.CommercialRuntime.StateDirectory, AllowHTTP: strings.HasPrefix(cfg.CommercialRuntime.PlatformBaseURL, "http://")}, nil
	}, strings.EqualFold(cfg.SubsystemOnboarding.Mode, "local"))
	return nil
}

func subsystemAccessSelectedRuntimeServices(ctx context.Context, source registryinfra.SubsystemAccessImageReader, app, env string) ([]string, error) {
	if source == nil {
		return nil, registry.ErrValidation
	}
	images, reported, err := source.ReadSubsystemAccessImages(ctx, app, env)
	if err != nil {
		return nil, err
	}
	if !reported || len(images) == 0 {
		return nil, registry.ErrValidation
	}
	set, err := (registryinfra.BuiltinRuntimeComponentCatalog{}).RuntimeComponentSet(ctx, app, env)
	if err != nil {
		return nil, err
	}
	allowed := map[string]bool{}
	for _, component := range set.Components {
		allowed[component.ServiceID] = true
	}
	ids := []string{}
	for _, image := range images {
		if !image.Present {
			return nil, registry.ErrValidation
		}
		if allowed[image.ServiceID] {
			ids = append(ids, image.ServiceID)
		}
	}
	selected, err := registry.SelectRuntimeComponentSet(set, ids)
	if len(ids) == 0 || err != nil || registry.ValidateRuntimeComponentSet(selected) != nil {
		return nil, registry.ErrValidation
	}
	return ids, nil
}
