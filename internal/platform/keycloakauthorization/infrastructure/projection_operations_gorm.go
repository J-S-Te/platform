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

// ProjectionOperationsStore 是持久化 FAILED outbox 记录的读取/恢复适配器。
// 刻意复用既有 outbox 表结构：不复制任何重试载荷，全部状态转移都发生在原事件上。
type ProjectionOperationsStore struct{ database *gorm.DB }

// NewProjectionOperationsStore 构造运维存储；database 为空时返回错误。
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

// ListFailedProjections 返回租户内 FAILED 投影死信的分页视图；无环境的事件不
// 阻塞切换门禁，因此 BlocksCutover 按是否存在 environment_id 判定。
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

// GetProjectionAlertStatus 聚合租户内 FAILED 死信数量、受影响环境数与最早失败时间，
// 归一化为无敏感信息的告警状态。
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

// ReplayFailedProjection 在行锁下执行 FAILED -> PENDING 的条件转移并清零尝试计数；
// 事件已处于 PENDING/RUNNING 时按幂等成功处理，绝不重置已被领取的事件。
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
			// 第二次请求不得重置尝试计数，也不得移动已被其他 worker 领取的行。
			// 这使浏览器/网络重试天然安全，无需额外的幂等表。
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
