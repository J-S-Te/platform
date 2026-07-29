package bootstrap

import (
	"context"
	"errors"
	"log/slog"
	"strings"

	applicationregistryapplication "github.com/J-S-Te/Basic-Platform/backend/internal/platform/applicationregistry/application"
	applicationregistryinfrastructure "github.com/J-S-Te/Basic-Platform/backend/internal/platform/applicationregistry/infrastructure"
	applicationregistryhttp "github.com/J-S-Te/Basic-Platform/backend/internal/platform/applicationregistry/interfaces/http"
	authorizationsys004 "github.com/J-S-Te/Basic-Platform/backend/internal/platform/authorization/sys004"
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
func buildOperationalModules(cfg config.Config, database *gorm.DB, logger *slog.Logger, contractAccessService *authorizationsys004.Service) (httptransport.OperationalModules, error) {
	if database == nil || logger == nil || contractAccessService == nil {
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
		subsystemInitialAccessManager{contractAccess: contractAccessService},
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

// subsystemInitialAccessManager bridges subsystem onboarding to the protected
// contract-management authorization catalog. Generic applications keep their existing
// authorization model; contract-management receives an explicit initial administrator.
type subsystemInitialAccessManager struct {
	contractAccess *authorizationsys004.Service
}

func (manager subsystemInitialAccessManager) AssignInitialAdministrator(
	ctx context.Context,
	tenantID string,
	applicationCode string,
	userID string,
	operatorID string,
) (string, error) {
	if strings.TrimSpace(applicationCode) != authorizationsys004.ApplicationCode {
		return "", nil
	}
	if manager.contractAccess == nil {
		return "", errors.New("contract application access service is unavailable")
	}
	_, err := manager.contractAccess.UpdateAccess(ctx, authorizationsys004.UpdateAccessInput{
		TenantID:   tenantID,
		UserID:     userID,
		OperatorID: operatorID,
		RoleCode:   "admin",
	})
	if err != nil {
		switch {
		case errors.Is(err, authorizationsys004.ErrValidation):
			return "", applicationregistryapplication.ErrValidation
		case errors.Is(err, authorizationsys004.ErrNotFound):
			return "", applicationregistryapplication.ErrNotFound
		default:
			return "", err
		}
	}
	return "admin", nil
}
