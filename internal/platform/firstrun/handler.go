package firstrun

import (
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/J-S-Te/Basic-Platform/internal/shared/authctx"
	"github.com/J-S-Te/Basic-Platform/internal/shared/httperror"
	"github.com/J-S-Te/Basic-Platform/internal/shared/httpresponse"
)

const (
	maxEventBytes = 8 * 1024
	guideVersion  = 2
)

var allowedEvents = map[string]struct{}{
	"tour_started":             {},
	"tour_step":                {},
	"tour_paused":              {},
	"tour_finished":            {},
	"guide_restarted":          {},
	"checklist_opened":         {},
	"checklist_task_completed": {},
	"checklist_reset":          {},
}

var allowedOutcomes = map[string]struct{}{
	"": {}, "done": {}, "skipped": {}, "paused": {},
}

// Handler 暴露已认证用户的导览配置与事件写入端点。
type Handler struct {
	store   EventStore
	enabled bool
	logger  *slog.Logger
	now     func() time.Time
}

// NewHandler 校验依赖并构造处理器。
func NewHandler(store EventStore, enabled bool, logger *slog.Logger) (*Handler, error) {
	if store == nil || logger == nil {
		return nil, errors.New("first-run handler dependencies must not be nil")
	}
	return &Handler{store: store, enabled: enabled, logger: logger, now: func() time.Time { return time.Now().UTC() }}, nil
}

type configResponse struct {
	Enabled bool `json:"enabled"`
	Version int  `json:"version"`
}

// Config 返回服务端功能开关与导览版本；关闭后前端停止自动启动。
func (handler *Handler) Config(w http.ResponseWriter, r *http.Request) {
	httpresponse.WriteSuccess(w, r, http.StatusOK, "OK", configResponse{Enabled: handler.enabled, Version: guideVersion})
}

type recordRequest struct {
	Event         string `json:"event"`
	GuideKey      string `json:"guide_key"`
	Step          int    `json:"step"`
	Outcome       string `json:"outcome"`
	ClientVersion string `json:"client_version"`
	TaskKey       string `json:"task_key"`
	OccurredAt    string `json:"occurred_at"`
}

// Record 追加一条导览事件；事件只用于产品运营，不参与权限判断。
func (handler *Handler) Record(w http.ResponseWriter, r *http.Request) {
	principal, ok := authctx.PrincipalFromContext(r.Context())
	if !ok || strings.TrimSpace(principal.Tenant.ID) == "" || strings.TrimSpace(principal.User.ID) == "" {
		httpresponse.WriteError(w, r, http.StatusUnauthorized, httperror.Error{Code: "FIRST_RUN_UNAUTHENTICATED", Message: "登录状态已失效"})
		return
	}
	if !handler.enabled {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	var payload recordRequest
	decoder := json.NewDecoder(io.LimitReader(r.Body, maxEventBytes))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&payload); err != nil {
		httpresponse.WriteError(w, r, http.StatusBadRequest, httperror.Error{Code: "FIRST_RUN_INVALID_EVENT", Message: "导览事件格式不正确"})
		return
	}
	if _, ok := allowedEvents[payload.Event]; !ok {
		httpresponse.WriteError(w, r, http.StatusBadRequest, httperror.Error{Code: "FIRST_RUN_INVALID_EVENT", Message: "导览事件类型不受支持"})
		return
	}
	if len(payload.GuideKey) > 64 || len(payload.ClientVersion) > 32 || len(payload.TaskKey) > 64 || payload.Step < 0 || payload.Step > 100 {
		httpresponse.WriteError(w, r, http.StatusBadRequest, httperror.Error{Code: "FIRST_RUN_INVALID_EVENT", Message: "导览事件字段超出范围"})
		return
	}
	if _, ok := allowedOutcomes[payload.Outcome]; !ok {
		httpresponse.WriteError(w, r, http.StatusBadRequest, httperror.Error{Code: "FIRST_RUN_INVALID_EVENT", Message: "导览结果不受支持"})
		return
	}
	occurredAt := handler.now()
	if strings.TrimSpace(payload.OccurredAt) != "" {
		if parsed, err := time.Parse(time.RFC3339, payload.OccurredAt); err == nil {
			occurredAt = parsed.UTC()
		}
	}
	if occurredAt.After(handler.now().Add(5*time.Minute)) || occurredAt.Before(handler.now().Add(-30*24*time.Hour)) {
		occurredAt = handler.now()
	}
	event := Event{
		TenantID: principal.Tenant.ID, UserID: principal.User.ID,
		Event: payload.Event, GuideKey: strings.TrimSpace(payload.GuideKey), Step: payload.Step,
		Outcome: payload.Outcome, ClientVersion: strings.TrimSpace(payload.ClientVersion), TaskKey: strings.TrimSpace(payload.TaskKey),
		OccurredAt: occurredAt, CreatedAt: handler.now(),
	}
	if err := handler.store.Insert(r.Context(), event); err != nil {
		handler.logger.Error("record first-run event", "error", err)
		httpresponse.WriteError(w, r, http.StatusServiceUnavailable, httperror.Error{Code: "FIRST_RUN_UNAVAILABLE", Message: "导览事件暂不可用"})
		return
	}
	w.WriteHeader(http.StatusAccepted)
}
