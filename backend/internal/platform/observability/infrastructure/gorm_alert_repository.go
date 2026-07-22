// Package infrastructure contains GORM adapters for observability configuration metadata.
package infrastructure

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/J-S-Te/Basic-Platform/backend/internal/platform/observability/application"
	"github.com/J-S-Te/Basic-Platform/backend/internal/platform/observability/domain"
	"gorm.io/gorm"
)

// AlertRepository persists alert rule/evaluation metadata. It intentionally never writes runtime
// log, trace, or metric samples into MySQL.
type AlertRepository struct {
	database *gorm.DB
}

// NewAlertRepository constructs the GORM alert metadata adapter.
func NewAlertRepository(database *gorm.DB) (*AlertRepository, error) {
	if database == nil {
		return nil, errors.New("observability database must not be nil")
	}
	return &AlertRepository{database: database}, nil
}

type alertRuleModel struct {
	RuleID          string
	TenantID        string
	ApplicationID   string
	Name            string
	MetricName      string
	Comparator      string
	Severity        string
	Status          string
	Threshold       float64
	WindowSeconds   uint
	CreatedBy       string
	UpdatedBy       string
	LastState       string
	LastTriggeredAt *time.Time
	CreatedAt       time.Time
	UpdatedAt       time.Time
	Version         uint
}

func (alertRuleModel) TableName() string {
	return "obs_alert_rule"
}

type alertEvaluationModel struct {
	EvaluationID       string
	RuleID             string
	TenantID           string
	State              string
	NotificationStatus string
	ErrorMessage       string
	ObservedValue      float64
	EvaluatedAt        time.Time
}

func (alertEvaluationModel) TableName() string {
	return "obs_alert_evaluation"
}

// CreateAlertRule inserts one versioned alert rule.
func (repository *AlertRepository) CreateAlertRule(ctx context.Context, rule domain.AlertRule) (domain.AlertRule, error) {
	model := toAlertRuleModel(rule)
	if err := repository.database.WithContext(ctx).Create(&model).Error; err != nil {
		return domain.AlertRule{}, err
	}
	return fromAlertRuleModel(model), nil
}

// GetAlertRule loads one tenant-isolated alert rule.
func (repository *AlertRepository) GetAlertRule(ctx context.Context, tenantID, ruleID string) (domain.AlertRule, error) {
	var model alertRuleModel
	if err := repository.database.WithContext(ctx).
		Where("tenant_id = ? AND rule_id = ?", tenantID, ruleID).
		Take(&model).Error; err != nil {
		return domain.AlertRule{}, mapError(err)
	}
	return fromAlertRuleModel(model), nil
}

// ListAlertRules returns one tenant's alert rules in a deterministic order.
func (repository *AlertRepository) ListAlertRules(ctx context.Context, tenantID string) ([]domain.AlertRule, error) {
	var models []alertRuleModel
	if err := repository.database.WithContext(ctx).
		Where("tenant_id = ?", tenantID).
		Order("created_at ASC, rule_id ASC").
		Find(&models).Error; err != nil {
		return nil, err
	}
	return fromAlertRuleModels(models), nil
}

// ListEnabledAlertRules is used by the worker and intentionally includes all tenants.
func (repository *AlertRepository) ListEnabledAlertRules(ctx context.Context) ([]domain.AlertRule, error) {
	var models []alertRuleModel
	if err := repository.database.WithContext(ctx).
		Where("status = ?", domain.AlertRuleEnabled).
		Order("created_at ASC, rule_id ASC").
		Find(&models).Error; err != nil {
		return nil, err
	}
	return fromAlertRuleModels(models), nil
}

// UpdateAlertRule updates threshold configuration with optimistic locking.
func (repository *AlertRepository) UpdateAlertRule(ctx context.Context, rule domain.AlertRule, expectedVersion uint) (domain.AlertRule, error) {
	updates := map[string]any{
		"application_id": rule.ApplicationID,
		"name":           rule.Name,
		"metric_name":    rule.MetricName,
		"comparator":     rule.Comparator,
		"severity":       rule.Severity,
		"status":         rule.Status,
		"threshold":      rule.Threshold,
		"window_seconds": rule.WindowSeconds,
		"updated_by":     rule.UpdatedBy,
		"updated_at":     rule.UpdatedAt,
		"version":        gorm.Expr("version + 1"),
	}
	result := repository.database.WithContext(ctx).
		Model(&alertRuleModel{}).
		Where("tenant_id = ? AND rule_id = ? AND version = ?", rule.TenantID, rule.RuleID, expectedVersion).
		Updates(updates)
	if result.Error != nil {
		return domain.AlertRule{}, result.Error
	}
	if result.RowsAffected != 1 {
		return domain.AlertRule{}, application.ErrConflict
	}

	rule.Version = expectedVersion + 1
	return rule, nil
}

// PersistEvaluationAndRuleState atomically stores the evaluation and updates the rule state.
// The optimistic lock ensures concurrent workers cannot emit duplicate transition notifications.
func (repository *AlertRepository) PersistEvaluationAndRuleState(
	ctx context.Context,
	rule domain.AlertRule,
	evaluation domain.AlertEvaluation,
	nextState string,
	now time.Time,
) (domain.AlertRule, error) {
	var updated domain.AlertRule
	err := repository.database.WithContext(ctx).Transaction(func(transaction *gorm.DB) error {
		evaluationModel := alertEvaluationModel{
			EvaluationID:       evaluation.EvaluationID,
			RuleID:             evaluation.RuleID,
			TenantID:           evaluation.TenantID,
			State:              evaluation.State,
			NotificationStatus: evaluation.NotificationStatus,
			ErrorMessage:       strings.TrimSpace(evaluation.ErrorMessage),
			ObservedValue:      evaluation.ObservedValue,
			EvaluatedAt:        evaluation.EvaluatedAt,
		}
		if err := transaction.Create(&evaluationModel).Error; err != nil {
			return err
		}

		updates := map[string]any{
			"last_state": nextState,
			"updated_at": now,
			"version":    gorm.Expr("version + 1"),
		}
		if !rule.LastTriggeredAt.IsZero() {
			updates["last_triggered_at"] = rule.LastTriggeredAt
		}
		result := transaction.Model(&alertRuleModel{}).
			Where("tenant_id = ? AND rule_id = ? AND version = ?", rule.TenantID, rule.RuleID, rule.Version).
			Updates(updates)
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected != 1 {
			return application.ErrConflict
		}

		updated = rule
		updated.LastState = nextState
		updated.UpdatedAt = now
		updated.Version++
		return nil
	})
	if err != nil {
		return domain.AlertRule{}, err
	}
	return updated, nil
}

func toAlertRuleModel(rule domain.AlertRule) alertRuleModel {
	model := alertRuleModel{
		RuleID:        rule.RuleID,
		TenantID:      rule.TenantID,
		ApplicationID: rule.ApplicationID,
		Name:          rule.Name,
		MetricName:    rule.MetricName,
		Comparator:    rule.Comparator,
		Severity:      rule.Severity,
		Status:        rule.Status,
		Threshold:     rule.Threshold,
		WindowSeconds: rule.WindowSeconds,
		CreatedBy:     rule.CreatedBy,
		UpdatedBy:     rule.UpdatedBy,
		LastState:     rule.LastState,
		CreatedAt:     rule.CreatedAt,
		UpdatedAt:     rule.UpdatedAt,
		Version:       rule.Version,
	}
	if !rule.LastTriggeredAt.IsZero() {
		triggeredAt := rule.LastTriggeredAt
		model.LastTriggeredAt = &triggeredAt
	}
	return model
}

func fromAlertRuleModels(models []alertRuleModel) []domain.AlertRule {
	rules := make([]domain.AlertRule, 0, len(models))
	for _, model := range models {
		rules = append(rules, fromAlertRuleModel(model))
	}
	return rules
}

func fromAlertRuleModel(model alertRuleModel) domain.AlertRule {
	rule := domain.AlertRule{
		RuleID:        model.RuleID,
		TenantID:      model.TenantID,
		ApplicationID: model.ApplicationID,
		Name:          model.Name,
		MetricName:    model.MetricName,
		Comparator:    model.Comparator,
		Severity:      model.Severity,
		Status:        model.Status,
		Threshold:     model.Threshold,
		WindowSeconds: model.WindowSeconds,
		CreatedBy:     model.CreatedBy,
		UpdatedBy:     model.UpdatedBy,
		LastState:     model.LastState,
		CreatedAt:     model.CreatedAt,
		UpdatedAt:     model.UpdatedAt,
		Version:       model.Version,
	}
	if model.LastTriggeredAt != nil {
		rule.LastTriggeredAt = *model.LastTriggeredAt
	}
	return rule
}

func mapError(err error) error {
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return application.ErrNotFound
	}
	return err
}
