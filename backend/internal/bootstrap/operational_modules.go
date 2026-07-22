package bootstrap

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"path/filepath"
	"strings"
	"time"

	applicationregistryapplication "github.com/J-S-Te/Basic-Platform/backend/internal/platform/applicationregistry/application"
	applicationregistryinfrastructure "github.com/J-S-Te/Basic-Platform/backend/internal/platform/applicationregistry/infrastructure"
	applicationregistryhttp "github.com/J-S-Te/Basic-Platform/backend/internal/platform/applicationregistry/interfaces/http"
	auditapplication "github.com/J-S-Te/Basic-Platform/backend/internal/platform/audit/application"
	auditinfrastructure "github.com/J-S-Te/Basic-Platform/backend/internal/platform/audit/infrastructure"
	audithttp "github.com/J-S-Te/Basic-Platform/backend/internal/platform/audit/interfaces/http"
	auditworker "github.com/J-S-Te/Basic-Platform/backend/internal/platform/audit/worker"
	filetaskapplication "github.com/J-S-Te/Basic-Platform/backend/internal/platform/filetask/application"
	filetaskinfrastructure "github.com/J-S-Te/Basic-Platform/backend/internal/platform/filetask/infrastructure"
	filetaskhttp "github.com/J-S-Te/Basic-Platform/backend/internal/platform/filetask/interfaces/http"
	notificationapplication "github.com/J-S-Te/Basic-Platform/backend/internal/platform/notification/application"
	notificationdomain "github.com/J-S-Te/Basic-Platform/backend/internal/platform/notification/domain"
	notificationinfrastructure "github.com/J-S-Te/Basic-Platform/backend/internal/platform/notification/infrastructure"
	notificationhttp "github.com/J-S-Te/Basic-Platform/backend/internal/platform/notification/interfaces/http"
	observabilityapplication "github.com/J-S-Te/Basic-Platform/backend/internal/platform/observability/application"
	observabilitydomain "github.com/J-S-Te/Basic-Platform/backend/internal/platform/observability/domain"
	observabilityinfrastructure "github.com/J-S-Te/Basic-Platform/backend/internal/platform/observability/infrastructure"
	observabilityhttp "github.com/J-S-Te/Basic-Platform/backend/internal/platform/observability/interfaces/http"
	observabilityworker "github.com/J-S-Te/Basic-Platform/backend/internal/platform/observability/worker"
	"github.com/J-S-Te/Basic-Platform/backend/internal/shared/config"
	"github.com/J-S-Te/Basic-Platform/backend/internal/shared/ulid"
	httptransport "github.com/J-S-Te/Basic-Platform/backend/internal/transport/http"
	"gorm.io/gorm"
)

const observabilityAlertTemplateCode = "OBSERVABILITY_ALERT"

// operationalBuild contains the shared HTTP modules and the in-process runner that evaluates
// metrics captured by the same API process. Runtime telemetry is intentionally bounded and local,
// so starting this runner in a separate worker process would evaluate an empty memory store.
type operationalBuild struct {
	modules     httptransport.OperationalModules
	alertRunner *observabilityworker.AlertRunner
}

// buildOperationalModules wires the optional platform operations modules through their public
// application contracts. Schema changes remain explicit SQL migrations; this function never calls
// GORM AutoMigrate.
func buildOperationalModules(cfg config.Config, database *gorm.DB, logger *slog.Logger, auditService *auditapplication.Service, auditRepository *auditinfrastructure.Repository) (operationalBuild, error) {
	if database == nil || logger == nil || auditService == nil || auditRepository == nil {
		return operationalBuild{}, errors.New("operational module dependencies must not be nil")
	}

	loginTargetRepository, err := applicationregistryinfrastructure.NewLoginTargetGORMRepository(database)
	if err != nil {
		return operationalBuild{}, err
	}
	loginTargetService, err := applicationregistryapplication.NewLoginTargetManagementService(loginTargetRepository, ulid.Generator{}, applicationregistryapplication.SystemClock{})
	if err != nil {
		return operationalBuild{}, err
	}
	loginTargetHandler, err := applicationregistryhttp.NewLoginTargetManagementHandler(loginTargetService, logger)
	if err != nil {
		return operationalBuild{}, err
	}

	notificationRepository, err := notificationinfrastructure.NewRepository(database)
	if err != nil {
		return operationalBuild{}, err
	}
	inboxPolicy, err := notificationinfrastructure.NewInboxPolicy(database)
	if err != nil {
		return operationalBuild{}, err
	}
	recipientResolver, err := notificationinfrastructure.NewRecipientResolver(database)
	if err != nil {
		return operationalBuild{}, err
	}
	notificationService, err := notificationapplication.NewService(notificationRepository, inboxPolicy, recipientResolver, ulid.Generator{}, notificationapplication.SystemClock{})
	if err != nil {
		return operationalBuild{}, err
	}
	notificationHandler, err := notificationhttp.NewHandler(notificationService, logger)
	if err != nil {
		return operationalBuild{}, err
	}

	telemetryStore := observabilityinfrastructure.NewMemoryStore(5000)
	telemetrySink, err := observabilityinfrastructure.NewJSONFileSink(filepath.Join(cfg.Logging.Directory, "runtime"))
	if err != nil {
		return operationalBuild{}, err
	}
	telemetryService, err := observabilityapplication.NewTelemetryService(telemetryStore, telemetrySink, observabilityapplication.SystemClock{})
	if err != nil {
		return operationalBuild{}, err
	}
	alertRepository, err := observabilityinfrastructure.NewAlertRepository(database)
	if err != nil {
		return operationalBuild{}, err
	}
	alertService, err := observabilityapplication.NewAlertService(
		alertRepository,
		telemetryStore,
		&stationNotificationAdapter{service: notificationService, logger: logger},
		ulid.Generator{},
		observabilityapplication.SystemClock{},
	)
	if err != nil {
		return operationalBuild{}, err
	}
	observabilityHandler, err := observabilityhttp.NewHandler(telemetryService, alertService, logger)
	if err != nil {
		return operationalBuild{}, err
	}
	telemetryMiddleware, err := observabilityhttp.NewTelemetryMiddleware(telemetryService, observabilitydomain.ResourceLabels{
		ServiceName:           cfg.AppName,
		DeploymentEnvironment: cfg.Environment,
		ServiceInstanceID:     cfg.Worker.ID,
		ApplicationID:         cfg.AppName,
	}, logger)
	if err != nil {
		return operationalBuild{}, err
	}
	alertRunner, err := observabilityworker.NewAlertRunner(alertService, logger, time.Minute)
	if err != nil {
		return operationalBuild{}, err
	}

	archiveWriter, err := auditworker.NewArchiveWriter(cfg.FileStorageRoot)
	if err != nil {
		return operationalBuild{}, err
	}
	governanceRecorder := &governanceAuditAdapter{service: auditService, config: cfg.Audit}
	retentionService, err := auditapplication.NewRetentionService(auditRepository, archiveWriter, ulid.Generator{}, auditapplication.SystemClock{}, governanceRecorder)
	if err != nil {
		return operationalBuild{}, err
	}
	deadLetterService, err := auditapplication.NewDeadLetterService(auditRepository, auditService, ulid.Generator{}, auditapplication.SystemClock{}, governanceRecorder)
	if err != nil {
		return operationalBuild{}, err
	}
	receiptService, err := auditapplication.NewIngestionReceiptService(auditRepository)
	if err != nil {
		return operationalBuild{}, err
	}
	auditOperationsHandler, err := audithttp.NewOperationsHandler(retentionService, deadLetterService, receiptService, logger)
	if err != nil {
		return operationalBuild{}, err
	}

	localStore, err := filetaskinfrastructure.NewLocalStore(cfg.FileStorageRoot)
	if err != nil {
		return operationalBuild{}, err
	}
	fileRepository, err := filetaskinfrastructure.NewGORMRepository(database)
	if err != nil {
		return operationalBuild{}, err
	}
	fileService, err := filetaskapplication.NewFileService(fileRepository, localStore, ulid.Generator{}, filetaskapplication.SystemClock{}, filetaskapplication.DefaultUploadPolicy())
	if err != nil {
		return operationalBuild{}, err
	}
	jobService, err := filetaskapplication.NewJobService(fileRepository, ulid.Generator{}, filetaskapplication.SystemClock{})
	if err != nil {
		return operationalBuild{}, err
	}
	fileTaskHandler, err := filetaskhttp.NewHandler(fileService, jobService, logger)
	if err != nil {
		return operationalBuild{}, err
	}

	return operationalBuild{
		modules: httptransport.OperationalModules{
			LoginTargets:    loginTargetHandler,
			Notifications:   notificationHandler,
			Observability:   observabilityHandler,
			Telemetry:       telemetryMiddleware,
			AuditOperations: auditOperationsHandler,
			FilesAndJobs:    fileTaskHandler,
		},
		alertRunner: alertRunner,
	}, nil
}

// stationNotificationAdapter translates alert transitions to the in-app notification service. It
// lazily provisions a tenant-local system template and never enables email or SMS delivery.
type stationNotificationAdapter struct {
	service *notificationapplication.Service
	logger  *slog.Logger
}

func (adapter *stationNotificationAdapter) PublishStationMessage(ctx context.Context, message observabilityapplication.StationMessage) error {
	if strings.TrimSpace(message.TenantID) == "" || strings.TrimSpace(message.RecipientID) == "" {
		return errors.New("observability station message tenant and recipient are required")
	}
	_, _, err := adapter.service.CreateTemplate(ctx, notificationapplication.CreateTemplateInput{
		TenantID:      message.TenantID,
		OperatorID:    message.RecipientID,
		Code:          observabilityAlertTemplateCode,
		Name:          "可观测性告警通知",
		Status:        notificationdomain.TemplateStatusActive,
		TitleTemplate: "{{title}}",
		BodyTemplate:  "{{content}}",
		Variables: []notificationdomain.VariableDefinition{
			{Name: "title", Required: true, MaxLength: 200},
			{Name: "content", Required: true, MaxLength: 2000},
		},
	})
	if err != nil && !errors.Is(err, notificationapplication.ErrConflict) {
		return fmt.Errorf("ensure observability alert template: %w", err)
	}

	_, err = adapter.service.Create(ctx, notificationapplication.CreateInput{
		TenantID:       message.TenantID,
		OperatorID:     message.RecipientID,
		TemplateCode:   observabilityAlertTemplateCode,
		Category:       message.Category,
		Variables:      map[string]string{"title": message.Title, "content": message.Content},
		Recipients:     []notificationdomain.RecipientTarget{{Type: notificationdomain.RecipientTypeUser, ID: message.RecipientID}},
		TargetURL:      "/console/observability",
		ReferenceType:  "OBSERVABILITY_ALERT",
		ReferenceID:    message.Metadata["rule_id"],
		IdempotencyKey: message.DeduplicationKey,
	})
	if err != nil {
		return fmt.Errorf("create observability station notification: %w", err)
	}
	return nil
}

// governanceAuditAdapter records retention and dead-letter operator actions through the same
// append-only audit ingestion path used by the rest of the platform.
type governanceAuditAdapter struct {
	service *auditapplication.Service
	config  config.AuditConfig
}

func (adapter *governanceAuditAdapter) RecordGovernanceAudit(ctx context.Context, record auditapplication.AuditRecord) error {
	applicationCode := strings.TrimSpace(record.ApplicationCode)
	if applicationCode == "" {
		applicationCode = adapter.config.ApplicationCode
	}
	environmentCode := strings.TrimSpace(record.EnvironmentCode)
	if environmentCode == "" {
		environmentCode = adapter.config.EnvironmentCode
	}
	_, err := adapter.service.Ingest(ctx, record.TenantID, auditapplication.EventInput{
		EventID:         record.EventID,
		ApplicationCode: applicationCode,
		EnvironmentCode: environmentCode,
		ActorType:       "USER",
		ActorID:         record.ActorID,
		ActorName:       record.ActorName,
		OccurredAt:      record.OccurredAt,
		Action:          record.Action,
		ResourceType:    record.ResourceType,
		ResourceID:      record.ResourceID,
		Result:          record.Result,
		RiskLevel:       record.RiskLevel,
		Classification:  "INTERNAL",
		Summary:         record.Summary,
		Metadata:        record.Metadata,
		EventCategory:   "PLATFORM_GOVERNANCE",
		EventType:       record.Action,
	})
	return err
}
