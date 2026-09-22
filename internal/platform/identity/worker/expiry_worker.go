package worker

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	auditapplication "github.com/J-S-Te/Basic-Platform/internal/platform/audit/application"
	identityapplication "github.com/J-S-Te/Basic-Platform/internal/platform/identity/application"
	notificationapplication "github.com/J-S-Te/Basic-Platform/internal/platform/notification/application"
	notificationdomain "github.com/J-S-Te/Basic-Platform/internal/platform/notification/domain"
)

type ExpirySessionRevoker interface {
	RevokeAccountSessions(context.Context, string, string, time.Time, string) error
}

type ExpiryWorker struct {
	repository                       identityapplication.ExpiryRepository
	revoker                          ExpirySessionRevoker
	audit                            *auditapplication.Service
	notifications                    *notificationapplication.Service
	logger                           *slog.Logger
	applicationCode, environmentCode string
	pollInterval                     time.Duration
}

func NewExpiryWorker(repository identityapplication.ExpiryRepository, revoker ExpirySessionRevoker, audit *auditapplication.Service, notifications *notificationapplication.Service, logger *slog.Logger, applicationCode, environmentCode string, pollInterval time.Duration) (*ExpiryWorker, error) {
	if repository == nil || revoker == nil || audit == nil || notifications == nil || logger == nil || strings.TrimSpace(applicationCode) == "" || strings.TrimSpace(environmentCode) == "" || pollInterval <= 0 {
		return nil, errors.New("identity expiry worker dependencies are invalid")
	}
	return &ExpiryWorker{repository: repository, revoker: revoker, audit: audit, notifications: notifications, logger: logger, applicationCode: strings.TrimSpace(applicationCode), environmentCode: strings.TrimSpace(environmentCode), pollInterval: pollInterval}, nil
}

func (worker *ExpiryWorker) Run(ctx context.Context) {
	ticker := time.NewTicker(worker.pollInterval)
	defer ticker.Stop()
	for {
		worker.ProcessDue(ctx)
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func (worker *ExpiryWorker) ProcessDue(ctx context.Context) int {
	now := time.Now().UTC().Truncate(time.Millisecond)
	candidates, err := worker.repository.ListDueExpirations(ctx, now, 100)
	if err != nil {
		worker.logger.Error("list due identity expirations", "error", err)
		return 0
	}
	processed := 0
	for _, candidate := range candidates {
		if err := worker.processOne(ctx, candidate, now); err != nil {
			worker.logger.Error("process identity expiration", "kind", candidate.Kind, "resource_id", candidate.ResourceID, "tenant_id", candidate.TenantID, "error", err)
			continue
		}
		processed++
	}
	return processed
}

func (worker *ExpiryWorker) processOne(ctx context.Context, candidate identityapplication.ExpiryCandidate, now time.Time) error {
	for _, accountID := range candidate.AccountIDs {
		if err := worker.revoker.RevokeAccountSessions(ctx, candidate.TenantID, accountID, now, candidate.Kind+"_EXPIRED"); err != nil && !errors.Is(err, identityapplication.ErrUnauthenticated) {
			return fmt.Errorf("revoke expired identity sessions: %w", err)
		}
	}
	eventID := fmt.Sprintf("IDENTITY_EXPIRY_%s_%s_%d", candidate.Kind, candidate.ResourceID, candidate.ValidUntil.UnixMilli())
	if _, err := worker.audit.Ingest(ctx, candidate.TenantID, auditapplication.EventInput{
		EventID: eventID, ApplicationCode: worker.applicationCode, EnvironmentCode: worker.environmentCode,
		OccurredAt: candidate.ValidUntil, Action: "identity." + strings.ToLower(candidate.Kind) + ".expired",
		ResourceType: candidate.Kind, ResourceID: candidate.ResourceID, Result: "SUCCESS", RiskLevel: "MEDIUM",
		Summary: "身份有效期已到期，现有登录会话已主动撤销", Metadata: map[string]any{"valid_until": candidate.ValidUntil, "revoked_account_count": len(candidate.AccountIDs)},
	}); err != nil {
		return fmt.Errorf("write identity expiry audit: %w", err)
	}
	if candidate.UserID != "" {
		_, err := worker.notifications.Ingest(ctx, notificationapplication.IngestInput{
			TenantID: candidate.TenantID, SourceApplication: worker.applicationCode, SourceEnvironment: worker.environmentCode,
			Event: notificationdomain.IngestionEvent{EventID: eventID, EventType: "IDENTITY_EXPIRED", NotificationScope: "PLATFORM", Priority: "HIGH", Title: "账号有效期已到期", Content: "您的基础平台身份有效期已到期，现有登录会话已安全注销。如需继续使用，请联系管理员调整有效期。", TargetURL: "/portal", ReferenceType: candidate.Kind, ReferenceID: candidate.ResourceID, IdempotencyKey: eventID, Recipients: []string{candidate.UserID}, OccurredAt: candidate.ValidUntil},
		})
		if err != nil && !errors.Is(err, notificationapplication.ErrNoRecipients) {
			return fmt.Errorf("enqueue identity expiry notification: %w", err)
		}
	}
	return worker.repository.MarkExpirationProcessed(ctx, candidate, now)
}
