package infrastructure

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	notificationapplication "github.com/J-S-Te/Basic-Platform/internal/platform/notification/application"
	notificationdomain "github.com/J-S-Te/Basic-Platform/internal/platform/notification/domain"
)

// ProjectionFailureNotifier sends in-app notifications when Keycloak authorization
// projections fail repeatedly. It implements the FailureNotifier interface from
// the keycloakauthorization/application package.
type ProjectionFailureNotifier struct {
	notificationService *notificationapplication.Service
	logger              *slog.Logger
}

// NewProjectionFailureNotifier constructs a notifier that sends alerts through
// the existing notification service. The logger is used for diagnostic output
// when notification delivery itself fails.
func NewProjectionFailureNotifier(notificationService *notificationapplication.Service, logger *slog.Logger) (*ProjectionFailureNotifier, error) {
	if notificationService == nil {
		return nil, fmt.Errorf("projection failure notifier requires a notification service")
	}
	if logger == nil {
		logger = slog.Default()
	}
	return &ProjectionFailureNotifier{notificationService: notificationService, logger: logger}, nil
}

// NotifyProjectionFailure sends a tenant-scoped in-app notification to the
// tenant's administrator about repeated Keycloak projection failures. It uses
// the keycloak.projection.failed template code; if the template does not exist
// in the tenant, the notification is silently skipped.
func (notifier *ProjectionFailureNotifier) NotifyProjectionFailure(ctx context.Context, tenantID, applicationID, identityID string, err error) {
	if notifier == nil || notifier.notificationService == nil {
		return
	}
	tenantID = strings.TrimSpace(tenantID)
	if tenantID == "" {
		return
	}
	idempotency := fmt.Sprintf("keycloak.projection.failure.%s.%s.%d", applicationID, identityID, time.Now().UTC().UnixNano()/int64(time.Minute))
	templateCode := "keycloak.projection.failed"
	// Send to the identity user if available, otherwise skip.
	// The notification service requires at least one recipient.
	identityID = strings.TrimSpace(identityID)
	if identityID == "" {
		return
	}
	_, createErr := notifier.notificationService.Create(ctx, notificationapplication.CreateInput{
		TenantID:     tenantID,
		OperatorID:   "system-keycloak",
		TemplateCode: templateCode,
		Category:     "keycloak_sync",
		Variables: map[string]string{
			"application_id": applicationID,
			"identity_id":    identityID,
			"error":          trimError(err, 500),
		},
		Recipients:     []notificationdomain.RecipientTarget{{Type: notificationdomain.RecipientTypeUser, ID: identityID}},
		IdempotencyKey: idempotency,
	})
	if createErr != nil {
		notifier.logger.Warn("failed to send projection failure notification",
			"tenant_id", tenantID, "application_id", applicationID,
			"identity_id", identityID, "error", createErr)
	}
}

func trimError(err error, maxLen int) string {
	if err == nil {
		return ""
	}
	msg := err.Error()
	if len(msg) > maxLen {
		return msg[:maxLen] + "…"
	}
	return msg
}
