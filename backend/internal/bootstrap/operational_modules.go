package bootstrap

import (
	"context"
	"errors"
	"log/slog"

	applicationregistryapplication "github.com/J-S-Te/Basic-Platform/backend/internal/platform/applicationregistry/application"
	applicationregistryinfrastructure "github.com/J-S-Te/Basic-Platform/backend/internal/platform/applicationregistry/infrastructure"
	applicationregistryhttp "github.com/J-S-Te/Basic-Platform/backend/internal/platform/applicationregistry/interfaces/http"
	applicationaccess "github.com/J-S-Te/Basic-Platform/backend/internal/platform/authorization/applicationaccess"
	filetaskapplication "github.com/J-S-Te/Basic-Platform/backend/internal/platform/filetask/application"
	filetaskinfrastructure "github.com/J-S-Te/Basic-Platform/backend/internal/platform/filetask/infrastructure"
	filetaskhttp "github.com/J-S-Te/Basic-Platform/backend/internal/platform/filetask/interfaces/http"
	notificationapplication "github.com/J-S-Te/Basic-Platform/backend/internal/platform/notification/application"
	notificationinfrastructure "github.com/J-S-Te/Basic-Platform/backend/internal/platform/notification/infrastructure"
	notificationhttp "github.com/J-S-Te/Basic-Platform/backend/internal/platform/notification/interfaces/http"
	"github.com/J-S-Te/Basic-Platform/backend/internal/shared/config"
	"github.com/J-S-Te/Basic-Platform/backend/internal/shared/ulid"
	httptransport "github.com/J-S-Te/Basic-Platform/backend/internal/transport/http"
	"gorm.io/gorm"
)

// buildOperationalModules wires the remaining platform operations modules through their public
// application contracts. Schema changes remain explicit SQL migrations; this function never calls
// GORM AutoMigrate.
func buildOperationalModules(cfg config.Config, database *gorm.DB, logger *slog.Logger, applicationAccessService *applicationaccess.Service) (httptransport.OperationalModules, error) {
	if database == nil || logger == nil || applicationAccessService == nil {
		return httptransport.OperationalModules{}, errors.New("operational module dependencies must not be nil")
	}

	loginTargetRepository, err := applicationregistryinfrastructure.NewLoginTargetGORMRepository(database)
	if err != nil {
		return httptransport.OperationalModules{}, err
	}
	loginTargetService, err := applicationregistryapplication.NewLoginTargetManagementService(loginTargetRepository, ulid.Generator{}, applicationregistryapplication.SystemClock{})
	if err != nil {
		return httptransport.OperationalModules{}, err
	}
	loginTargetHandler, err := applicationregistryhttp.NewLoginTargetManagementHandler(loginTargetService, logger)
	if err != nil {
		return httptransport.OperationalModules{}, err
	}

	subsystemRepository, err := applicationregistryinfrastructure.NewSubsystemOnboardingGORMRepository(database)
	if err != nil {
		return httptransport.OperationalModules{}, err
	}
	subsystemService, err := applicationregistryapplication.NewSubsystemOnboardingService(
		subsystemRepository, ulid.Generator{}, applicationregistryapplication.SystemClock{},
		applicationregistryapplication.RedirectURIValidationPolicy{
			AllowInsecureHTTP: cfg.Auth.OAuthClientAllowInsecureHTTPRedirectURIs,
		},
	)
	if err != nil {
		return httptransport.OperationalModules{}, err
	}
	subsystemProvisioner, err := applicationregistryinfrastructure.NewUnixSocketSubsystemProvisioner(
		cfg.SubsystemOnboarding.Enabled,
		cfg.SubsystemOnboarding.SocketPath,
		cfg.SubsystemOnboarding.Timeout,
	)
	if err != nil {
		return httptransport.OperationalModules{}, err
	}
	subsystemHandler, err := applicationregistryhttp.NewSubsystemOnboardingHandler(
		subsystemService,
		subsystemProvisioner,
		subsystemInitialAccessManager{applicationAccess: applicationAccessService},
		cfg.Auth.OIDCIssuer,
		logger,
	)
	if err != nil {
		return httptransport.OperationalModules{}, err
	}

	notificationRepository, err := notificationinfrastructure.NewRepository(database)
	if err != nil {
		return httptransport.OperationalModules{}, err
	}
	inboxPolicy, err := notificationinfrastructure.NewInboxPolicy(database)
	if err != nil {
		return httptransport.OperationalModules{}, err
	}
	recipientResolver, err := notificationinfrastructure.NewRecipientResolver(database)
	if err != nil {
		return httptransport.OperationalModules{}, err
	}
	notificationService, err := notificationapplication.NewService(notificationRepository, inboxPolicy, recipientResolver, ulid.Generator{}, notificationapplication.SystemClock{})
	if err != nil {
		return httptransport.OperationalModules{}, err
	}
	notificationHandler, err := notificationhttp.NewHandler(notificationService, logger)
	if err != nil {
		return httptransport.OperationalModules{}, err
	}

	localStore, err := filetaskinfrastructure.NewLocalStore(cfg.FileStorageRoot)
	if err != nil {
		return httptransport.OperationalModules{}, err
	}
	fileRepository, err := filetaskinfrastructure.NewGORMRepository(database)
	if err != nil {
		return httptransport.OperationalModules{}, err
	}
	fileService, err := filetaskapplication.NewFileService(fileRepository, localStore, ulid.Generator{}, filetaskapplication.SystemClock{}, filetaskapplication.DefaultUploadPolicy())
	if err != nil {
		return httptransport.OperationalModules{}, err
	}
	jobService, err := filetaskapplication.NewJobService(fileRepository, ulid.Generator{}, filetaskapplication.SystemClock{})
	if err != nil {
		return httptransport.OperationalModules{}, err
	}
	fileTaskHandler, err := filetaskhttp.NewHandler(fileService, jobService, logger)
	if err != nil {
		return httptransport.OperationalModules{}, err
	}

	return httptransport.OperationalModules{
		LoginTargets:        loginTargetHandler,
		SubsystemOnboarding: subsystemHandler,
		Notifications:       notificationHandler,
		FilesAndJobs:        fileTaskHandler,
	}, nil
}

// subsystemInitialAccessManager assigns a conventional initial administrator role when
// the newly registered application's catalog already defines one. The manager is intentionally
// application-agnostic: application code is only data passed to the generic authorization service.
type subsystemInitialAccessManager struct {
	applicationAccess *applicationaccess.Service
}

func (manager subsystemInitialAccessManager) AssignInitialAdministrator(
	ctx context.Context,
	tenantID string,
	applicationCode string,
	userID string,
	operatorID string,
) (string, error) {
	if manager.applicationAccess == nil {
		return "", errors.New("application authorization service is unavailable")
	}
	_, err := manager.applicationAccess.UpdateAccess(ctx, applicationaccess.UpdateAccessInput{
		TenantID:      tenantID,
		UserID:        userID,
		OperatorID:    operatorID,
		Roles:         []applicationaccess.RoleInput{{RoleCode: "admin", ScopeType: "APPLICATION"}},
		RolesProvided: true,
	}, applicationCode)
	if err != nil {
		// A subsystem may be onboarded before it publishes its role catalog. In that
		// case there is no generic admin role to assign yet; onboarding itself remains
		// successful and the platform administrator can assign roles after catalog sync.
		if errors.Is(err, applicationaccess.ErrValidation) {
			return "", nil
		}
		switch {
		case errors.Is(err, applicationaccess.ErrNotFound):
			return "", applicationregistryapplication.ErrNotFound
		default:
			return "", err
		}
	}
	return "admin", nil
}
