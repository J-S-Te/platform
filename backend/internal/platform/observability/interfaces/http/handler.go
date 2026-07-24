// Package http exposes Gin adapters for protected observability query and alert-rule operations.
package http

import (
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	app "github.com/J-S-Te/Basic-Platform/backend/internal/platform/observability/application"
	"github.com/J-S-Te/Basic-Platform/backend/internal/platform/observability/domain"
	"github.com/J-S-Te/Basic-Platform/backend/internal/shared/authctx"
	"github.com/J-S-Te/Basic-Platform/backend/internal/shared/httperror"
	"github.com/J-S-Te/Basic-Platform/backend/internal/shared/httpresponse"
	"github.com/gin-gonic/gin"
)

const maxQueryLimit = 1000

// Handler adapts the observability application services to Gin. Authorization remains a router
// concern; every request is still tenant-scoped from the verified cookie principal here.
type Handler struct {
	telemetry *app.TelemetryService
	alerts    *app.AlertService
	logger    *slog.Logger
}

// NewHandler constructs a handler for a fully configured observability module.
func NewHandler(telemetry *app.TelemetryService, alerts *app.AlertService, logger *slog.Logger) (*Handler, error) {
	if telemetry == nil || alerts == nil || logger == nil {
		return nil, errors.New("observability handler dependencies must not be nil")
	}
	return &Handler{telemetry: telemetry, alerts: alerts, logger: logger}, nil
}

// ListLogs returns redacted structured runtime logs from the configured bounded telemetry store.
func (handler *Handler) ListLogs(context *gin.Context) {
	principal, ok := handler.principal(context)
	if !ok {
		return
	}
	from, to, ok := parseTimeRange(context)
	if !ok {
		return
	}
	limit, ok := parseLimit(context)
	if !ok {
		return
	}

	items, err := handler.telemetry.QueryLogs(context.Request.Context(), app.LogQuery{
		TenantID:      principal.Tenant.ID,
		ApplicationID: strings.TrimSpace(context.Query("application_id")),
		TraceID:       strings.TrimSpace(context.Query("trace_id")),
		RequestID:     strings.TrimSpace(context.Query("request_id")),
		Severity:      strings.ToUpper(strings.TrimSpace(context.Query("severity"))),
		Keyword:       strings.TrimSpace(context.Query("keyword")),
		From:          from,
		To:            to,
		Limit:         limit,
	})
	if err != nil {
		handler.writeError(context, err)
		return
	}
	httpresponse.WriteSuccess(context.Writer, context.Request, http.StatusOK, "运行日志查询成功", gin.H{"items": items})
}

// ListTraces returns compact trace spans scoped to the verified caller tenant.
func (handler *Handler) ListTraces(context *gin.Context) {
	principal, ok := handler.principal(context)
	if !ok {
		return
	}
	from, to, ok := parseTimeRange(context)
	if !ok {
		return
	}
	limit, ok := parseLimit(context)
	if !ok {
		return
	}

	items, err := handler.telemetry.QueryTraces(context.Request.Context(), app.TraceQuery{
		TenantID:      principal.Tenant.ID,
		ApplicationID: strings.TrimSpace(context.Query("application_id")),
		TraceID:       strings.TrimSpace(context.Query("trace_id")),
		RequestID:     strings.TrimSpace(context.Query("request_id")),
		From:          from,
		To:            to,
		Limit:         limit,
	})
	if err != nil {
		handler.writeError(context, err)
		return
	}
	httpresponse.WriteSuccess(context.Writer, context.Request, http.StatusOK, "链路追踪查询成功", gin.H{"items": items})
}

// ListMetrics returns metric observations scoped to the verified caller tenant.
func (handler *Handler) ListMetrics(context *gin.Context) {
	principal, ok := handler.principal(context)
	if !ok {
		return
	}
	from, to, ok := parseTimeRange(context)
	if !ok {
		return
	}
	limit, ok := parseLimit(context)
	if !ok {
		return
	}

	items, err := handler.telemetry.QueryMetrics(context.Request.Context(), app.MetricQuery{
		TenantID:      principal.Tenant.ID,
		ApplicationID: strings.TrimSpace(context.Query("application_id")),
		Name:          strings.TrimSpace(context.Query("name")),
		From:          from,
		To:            to,
		Limit:         limit,
	})
	if err != nil {
		handler.writeError(context, err)
		return
	}
	httpresponse.WriteSuccess(context.Writer, context.Request, http.StatusOK, "指标查询成功", gin.H{"items": items})
}

// ListAlertRules lists alert rule metadata. Evaluations are intentionally not exposed here until
// the shared OpenAPI contract adds a tenant-authorized evaluation query endpoint.
func (handler *Handler) ListAlertRules(context *gin.Context) {
	principal, ok := handler.principal(context)
	if !ok {
		return
	}
	items, err := handler.alerts.ListRules(context.Request.Context(), principal.Tenant.ID)
	if err != nil {
		handler.writeError(context, err)
		return
	}
	httpresponse.WriteSuccess(context.Writer, context.Request, http.StatusOK, "告警规则查询成功", gin.H{"items": items})
}

type rulePayload struct {
	ApplicationID string  `json:"application_id"`
	Name          string  `json:"name"`
	MetricName    string  `json:"metric_name"`
	Comparator    string  `json:"comparator"`
	Severity      string  `json:"severity"`
	Status        string  `json:"status"`
	Threshold     float64 `json:"threshold"`
	WindowSeconds uint    `json:"window_seconds"`
	Version       uint    `json:"version"`
}

// CreateAlertRule persists a threshold rule. The operator and tenant IDs are never accepted from
// the browser; they are obtained only from verified authentication middleware state.
func (handler *Handler) CreateAlertRule(context *gin.Context) {
	principal, ok := handler.principal(context)
	if !ok {
		return
	}
	var payload rulePayload
	if err := context.ShouldBindJSON(&payload); err != nil {
		httpresponse.WriteError(context.Writer, context.Request, http.StatusUnprocessableEntity, httperror.Validation)
		return
	}

	rule, err := handler.alerts.CreateRule(context.Request.Context(), app.AlertRuleInput{
		TenantID:      principal.Tenant.ID,
		ApplicationID: payload.ApplicationID,
		Name:          payload.Name,
		MetricName:    payload.MetricName,
		Comparator:    strings.ToUpper(strings.TrimSpace(payload.Comparator)),
		Severity:      strings.ToUpper(strings.TrimSpace(payload.Severity)),
		Status:        strings.ToUpper(strings.TrimSpace(payload.Status)),
		OperatorID:    principal.User.ID,
		Threshold:     payload.Threshold,
		WindowSeconds: payload.WindowSeconds,
	})
	if err != nil {
		handler.writeError(context, err)
		return
	}
	httpresponse.WriteSuccess(context.Writer, context.Request, http.StatusCreated, "告警规则已创建", gin.H{"alert_rule": rule})
}

// UpdateAlertRule applies a complete versioned rule update.
func (handler *Handler) UpdateAlertRule(context *gin.Context) {
	principal, ok := handler.principal(context)
	if !ok {
		return
	}
	var payload rulePayload
	if err := context.ShouldBindJSON(&payload); err != nil {
		httpresponse.WriteError(context.Writer, context.Request, http.StatusUnprocessableEntity, httperror.Validation)
		return
	}

	rule, err := handler.alerts.UpdateRule(context.Request.Context(), app.AlertRuleUpdateInput{
		TenantID:      principal.Tenant.ID,
		RuleID:        context.Param("rule_id"),
		ApplicationID: payload.ApplicationID,
		Name:          payload.Name,
		MetricName:    payload.MetricName,
		Comparator:    strings.ToUpper(strings.TrimSpace(payload.Comparator)),
		Severity:      strings.ToUpper(strings.TrimSpace(payload.Severity)),
		Status:        strings.ToUpper(strings.TrimSpace(payload.Status)),
		OperatorID:    principal.User.ID,
		Threshold:     payload.Threshold,
		WindowSeconds: payload.WindowSeconds,
		Version:       payload.Version,
	})
	if err != nil {
		handler.writeError(context, err)
		return
	}
	httpresponse.WriteSuccess(context.Writer, context.Request, http.StatusOK, "告警规则已更新", gin.H{"alert_rule": rule})
}

// ExecuteAlertRule performs a one-off evaluation for an enabled or disabled rule. Router wiring
// should limit this operational endpoint to the explicit alert-execute permission.
func (handler *Handler) ExecuteAlertRule(context *gin.Context) {
	principal, ok := handler.principal(context)
	if !ok {
		return
	}
	execution, err := handler.alerts.ExecuteRule(context.Request.Context(), principal.Tenant.ID, context.Param("rule_id"))
	if err != nil {
		handler.writeError(context, err)
		return
	}
	httpresponse.WriteSuccess(context.Writer, context.Request, http.StatusOK, "告警规则执行完成", gin.H{"execution": execution})
}

func (handler *Handler) principal(context *gin.Context) (authctx.Principal, bool) {
	principal, ok := authctx.PrincipalFromContext(context.Request.Context())
	if !ok || strings.TrimSpace(principal.Tenant.ID) == "" || strings.TrimSpace(principal.User.ID) == "" {
		httpresponse.WriteError(context.Writer, context.Request, http.StatusUnauthorized, httperror.Unauthenticated)
		return authctx.Principal{}, false
	}
	return principal, true
}

func (handler *Handler) writeError(context *gin.Context, err error) {
	switch {
	case errors.Is(err, app.ErrValidation):
		httpresponse.WriteError(context.Writer, context.Request, http.StatusUnprocessableEntity, httperror.Validation)
	case errors.Is(err, app.ErrNotFound):
		httpresponse.WriteError(context.Writer, context.Request, http.StatusNotFound, httperror.NotFound)
	case errors.Is(err, app.ErrConflict):
		httpresponse.WriteError(context.Writer, context.Request, http.StatusConflict, httperror.VersionConflict)
	default:
		handler.logger.Error("observability request failed", "error", err)
		httpresponse.WriteError(context.Writer, context.Request, http.StatusInternalServerError, httperror.Internal)
	}
}

func parseTimeRange(context *gin.Context) (*time.Time, *time.Time, bool) {
	from, ok := parseOptionalTime(context, "from")
	if !ok {
		return nil, nil, false
	}
	to, ok := parseOptionalTime(context, "to")
	if !ok {
		return nil, nil, false
	}
	if from != nil && to != nil && to.Before(*from) {
		httpresponse.WriteError(context.Writer, context.Request, http.StatusUnprocessableEntity, httperror.Validation)
		return nil, nil, false
	}
	return from, to, true
}

func parseOptionalTime(context *gin.Context, key string) (*time.Time, bool) {
	raw := strings.TrimSpace(context.Query(key))
	if raw == "" {
		return nil, true
	}
	value, err := time.Parse(time.RFC3339, raw)
	if err != nil {
		httpresponse.WriteError(context.Writer, context.Request, http.StatusUnprocessableEntity, httperror.Validation)
		return nil, false
	}
	value = value.UTC()
	return &value, true
}

func parseLimit(context *gin.Context) (int, bool) {
	raw := strings.TrimSpace(context.Query("limit"))
	if raw == "" {
		return 100, true
	}
	value, err := strconv.Atoi(raw)
	if err != nil || value < 1 || value > maxQueryLimit {
		httpresponse.WriteError(context.Writer, context.Request, http.StatusUnprocessableEntity, httperror.Validation)
		return 0, false
	}
	return value, true
}

var _ = domain.AlertRule{}
