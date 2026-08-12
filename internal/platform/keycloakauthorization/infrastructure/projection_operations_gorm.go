package infrastructure

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	projectionapplication "github.com/J-S-Te/Basic-Platform/internal/platform/keycloakauthorization/application"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// ProjectionOperationsStore is the read/recovery adapter for durable FAILED
// outbox records.  It intentionally uses the existing outbox schema: no retry
// payload is copied, and all state transitions remain on the original event.
type ProjectionOperationsStore struct{ database *gorm.DB }

func NewProjectionOperationsStore(database *gorm.DB) (*ProjectionOperationsStore, error) {
	if database == nil {
		return nil, errors.New("Keycloak projection operations database must not be nil")
	}
	return &ProjectionOperationsStore{database: database}, nil
}

type failedProjectionRow struct {
	EventID, IdentityID, ApplicationID, EnvironmentID string
	ApplicationCode, Environment, EventType           string
	Attempts                                          uint
	FailedAt                                          time.Time
	ErrorCode, ErrorMessage                           string
}

func (store *ProjectionOperationsStore) ListFailedProjections(ctx context.Context, tenantID string, query projectionapplication.FailurePageRequest) (projectionapplication.FailurePageResult, error) {
	base := store.failedQuery(ctx, tenantID, query)
	var total int64
	if err := base.Session(&gorm.Session{}).Count(&total).Error; err != nil {
		return projectionapplication.FailurePageResult{}, fmt.Errorf("count failed Keycloak projections: %w", err)
	}
	rows := make([]failedProjectionRow, 0)
	offset := (query.Page - 1) * query.PageSize
	if err := base.Select(`outbox.id AS event_id, outbox.identity_id, outbox.application_id, COALESCE(outbox.environment_id, '') AS environment_id,
application.code AS application_code, COALESCE(environment.environment, '') AS environment, outbox.event_type, outbox.attempts,
outbox.completed_at AS failed_at, COALESCE(outbox.last_error_code, '') AS error_code, COALESCE(outbox.last_error_message, '') AS error_message`).
		Order("outbox.completed_at ASC, outbox.created_at ASC").Offset(offset).Limit(query.PageSize).Scan(&rows).Error; err != nil {
		return projectionapplication.FailurePageResult{}, fmt.Errorf("list failed Keycloak projections: %w", err)
	}
	items := make([]projectionapplication.FailedProjection, 0, len(rows))
	for _, row := range rows {
		items = append(items, projectionapplication.FailedProjection{
			EventID: row.EventID, IdentityID: row.IdentityID, ApplicationID: row.ApplicationID, EnvironmentID: row.EnvironmentID,
			ApplicationCode: row.ApplicationCode, Environment: row.Environment, EventType: row.EventType, Attempts: row.Attempts,
			FailedAt: row.FailedAt, ErrorCode: row.ErrorCode, ErrorMessage: row.ErrorMessage,
			BlocksCutover: row.EnvironmentID != "",
		})
	}
	return projectionapplication.FailurePageResult{Items: items, Page: query.Page, PageSize: query.PageSize, Total: total}, nil
}

func (store *ProjectionOperationsStore) GetProjectionAlertStatus(ctx context.Context, tenantID string) (projectionapplication.AlertStatus, error) {
	var aggregate struct {
		FailedCount              int64      `gorm:"column:failed_count"`
		AffectedEnvironmentCount int64      `gorm:"column:affected_environment_count"`
		OldestFailedAt           *time.Time `gorm:"column:oldest_failed_at"`
	}
	err := store.database.WithContext(ctx).Table("keycloak_authorization_outbox").
		Select("COUNT(*) AS failed_count, COUNT(DISTINCT CASE WHEN environment_id IS NULL THEN NULL ELSE CONCAT(application_id, ':', environment_id) END) AS affected_environment_count, MIN(completed_at) AS oldest_failed_at").
		Where("tenant_id = ? AND status = ?", strings.TrimSpace(tenantID), "FAILED").Scan(&aggregate).Error
	if err != nil {
		return projectionapplication.AlertStatus{}, fmt.Errorf("inspect failed Keycloak projection alert: %w", err)
	}
	if aggregate.FailedCount == 0 {
		return projectionapplication.AlertStatus{Severity: "INFO", State: "CLEAR", Summary: "没有待处理的 Keycloak 授权投影死信。"}, nil
	}
	return projectionapplication.AlertStatus{
		Severity: "CRITICAL", State: "ACTIVE", FailedCount: aggregate.FailedCount,
		AffectedEnvironmentCount: aggregate.AffectedEnvironmentCount, OldestFailedAt: aggregate.OldestFailedAt,
		Summary: "存在 Keycloak 授权投影死信；受影响环境的认证切换门禁已阻塞。",
	}, nil
}

func (store *ProjectionOperationsStore) ReplayFailedProjection(ctx context.Context, input projectionapplication.ReplayInput, availableAt time.Time) (projectionapplication.ReplayResult, error) {
	result := projectionapplication.ReplayResult{EventID: input.EventID}
	err := store.database.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var row authorizationOutboxRow
		err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).
			Where("id = ? AND tenant_id = ?", strings.TrimSpace(input.EventID), strings.TrimSpace(input.TenantID)).Take(&row).Error
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return projectionapplication.ErrProjectionNotFound
		}
		if err != nil {
			return fmt.Errorf("load Keycloak projection for replay: %w", err)
		}
		switch row.Status {
		case "FAILED":
			updates := map[string]any{
				"status": "PENDING", "available_at": availableAt.UTC(), "locked_by": nil, "locked_at": nil,
				"attempts": 0, "completed_at": nil,
			}
			updated := tx.Model(&authorizationOutboxRow{}).Where("id = ? AND tenant_id = ? AND status = ?", row.ID, strings.TrimSpace(input.TenantID), "FAILED").Updates(updates)
			if updated.Error != nil {
				return fmt.Errorf("schedule Keycloak projection replay: %w", updated.Error)
			}
			if updated.RowsAffected != 1 {
				return projectionapplication.ErrProjectionConflict
			}
			result.Replayed, result.AvailableAt = true, availableAt.UTC()
			return nil
		case "PENDING", "RUNNING":
			// A second request must not reset the attempt counter or move a row
			// already claimed by another worker. This makes browser/network retry
			// safe without an additional idempotency table.
			result.AlreadyPending = true
			return nil
		default:
			return projectionapplication.ErrProjectionConflict
		}
	})
	if err != nil {
		return projectionapplication.ReplayResult{}, err
	}
	return result, nil
}

func (store *ProjectionOperationsStore) failedQuery(ctx context.Context, tenantID string, query projectionapplication.FailurePageRequest) *gorm.DB {
	database := store.database.WithContext(ctx).Table("keycloak_authorization_outbox AS outbox").
		Joins("JOIN platform_application AS application ON application.tenant_id = outbox.tenant_id AND application.id = outbox.application_id").
		Joins("LEFT JOIN platform_application_environment AS environment ON environment.tenant_id = outbox.tenant_id AND environment.application_id = outbox.application_id AND environment.id = outbox.environment_id").
		Where("outbox.tenant_id = ? AND outbox.status = ?", strings.TrimSpace(tenantID), "FAILED")
	if query.ApplicationCode != "" {
		database = database.Where("application.code = ?", query.ApplicationCode)
	}
	if query.Environment != "" {
		database = database.Where("environment.environment = ?", query.Environment)
	}
	return database
}

var _ projectionapplication.OperationsStore = (*ProjectionOperationsStore)(nil)
