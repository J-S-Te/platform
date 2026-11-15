package securityevent

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"log/slog"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	"encoding/json"

	"github.com/J-S-Te/Basic-Platform/internal/shared/appctx"
)

const (
	maxEventID   = 128
	maxPath      = 512
	maxUserAgent = 1000
)

// Handler 同时服务应用摄取、平台认证失败写入和平台管理员查询。
type Handler struct {
	store  *Store
	logger *slog.Logger
}

func NewHandler(store *Store, logger *slog.Logger) (*Handler, error) {
	if store == nil || store.DB == nil || logger == nil {
		return nil, http.ErrNotSupported
	}
	return &Handler{store: store, logger: logger}, nil
}

// Record 是平台认证失败中间件使用的窄写入口；来源应用/环境由调用方配置。
func (h *Handler) Record(ctx context.Context, event Event) error {
	if h == nil || h.store == nil {
		return http.ErrNotSupported
	}
	if strings.TrimSpace(event.EventID) == "" {
		event.EventID = randomID()
	}
	if event.OccurredAt.IsZero() {
		event.OccurredAt = time.Now().UTC()
	}
	if event.ReceivedAt.IsZero() {
		event.ReceivedAt = time.Now().UTC()
	}
	if event.Result == "" {
		event.Result = "DENIED"
	}
	if event.RiskLevel == "" {
		event.RiskLevel = "HIGH"
	}
	return h.store.Record(ctx, event)
}

type ingestRequest struct {
	EventID    string `json:"event_id"`
	OccurredAt string `json:"occurred_at"`
	ActorID    string `json:"actor_id"`
	ActorName  string `json:"actor_name"`
	Action     string `json:"action"`
	Method     string `json:"method"`
	Path       string `json:"path"`
	ClientIP   string `json:"client_ip"`
	UserAgent  string `json:"user_agent"`
	RequestID  string `json:"request_id"`
	Result     string `json:"result"`
	ReasonCode string `json:"reason_code"`
	RiskLevel  string `json:"risk_level"`
	Summary    string `json:"summary"`
}

// Ingest 接收子系统上报的认证失败事件。应用/环境/租户一律取自应用令牌，
// 请求体不能声明来源，避免伪造其他应用的安全事件。
func (h *Handler) Ingest(w http.ResponseWriter, r *http.Request) {
	principal, ok := appctx.PrincipalFromContext(r.Context())
	if !ok {
		writeJSON(w, http.StatusUnauthorized, map[string]any{"error": "unauthenticated"})
		return
	}
	var payload ingestRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&payload); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid_request"})
		return
	}
	event, err := h.eventFromRequest(principal, payload)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid_request"})
		return
	}
	if err := h.store.Record(r.Context(), event); err != nil {
		h.logger.Error("record security event", "error", err, "application", principal.ApplicationCode)
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": "security_event_unavailable"})
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]any{"event_id": event.EventID, "status": "ACCEPTED"})
}

func (h *Handler) eventFromRequest(principal appctx.Principal, payload ingestRequest) (Event, error) {
	eventID := strings.TrimSpace(payload.EventID)
	if eventID == "" {
		eventID = randomID()
	}
	if len(eventID) > maxEventID {
		return Event{}, errInvalid
	}
	occurredAt := time.Now().UTC()
	if value := strings.TrimSpace(payload.OccurredAt); value != "" {
		parsed, err := time.Parse(time.RFC3339Nano, value)
		if err != nil || parsed.After(time.Now().UTC().Add(5*time.Minute)) {
			return Event{}, errInvalid
		}
		occurredAt = parsed.UTC()
	}
	method := strings.ToUpper(strings.TrimSpace(payload.Method))
	if method == "" || len(method) > 16 || !strings.HasPrefix(strings.TrimSpace(payload.Path), "/") || len(payload.Path) > maxPath {
		return Event{}, errInvalid
	}
	result := strings.ToUpper(strings.TrimSpace(payload.Result))
	if result == "" {
		result = "DENIED"
	}
	if !validResult(result) {
		return Event{}, errInvalid
	}
	risk := strings.ToUpper(strings.TrimSpace(payload.RiskLevel))
	if risk == "" {
		risk = "HIGH"
	}
	if !validRisk(risk) {
		return Event{}, errInvalid
	}
	clientIP := strings.TrimSpace(payload.ClientIP)
	if clientIP != "" && net.ParseIP(clientIP) == nil {
		clientIP = ""
	}
	return Event{
		EventID: eventID, TenantID: principal.TenantID,
		ApplicationID: principal.ApplicationID, ApplicationCode: principal.ApplicationCode,
		EnvironmentID: principal.EnvironmentID, EnvironmentCode: principal.EnvironmentCode,
		OccurredAt: occurredAt, ReceivedAt: time.Now().UTC(),
		ActorType: "ANONYMOUS", ActorID: strings.TrimSpace(payload.ActorID), ActorName: strings.TrimSpace(payload.ActorName),
		Action: trimLimit(payload.Action, 128), Method: method, Path: payload.Path,
		ClientIP: clientIP, UserAgent: trimLimit(payload.UserAgent, maxUserAgent),
		RequestID: trimLimit(payload.RequestID, 128), Result: result,
		ReasonCode: trimLimit(payload.ReasonCode, 128), RiskLevel: risk,
		Summary: trimLimit(payload.Summary, 500),
	}, nil
}

// List 返回全局安全事件；路由层已要求 platform:audit:view，不做租户过滤。
func (h *Handler) List(w http.ResponseWriter, r *http.Request) {
	query := Query{
		ApplicationCode: strings.TrimSpace(r.URL.Query().Get("application_code")),
		Result:          strings.ToUpper(strings.TrimSpace(r.URL.Query().Get("result"))),
		Page:            parseInt(r.URL.Query().Get("page"), 1),
		PageSize:        parseInt(r.URL.Query().Get("page_size"), 20),
	}
	if value := strings.TrimSpace(r.URL.Query().Get("occurred_from")); value != "" {
		parsed, err := time.Parse(time.RFC3339, value)
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid_occurred_from"})
			return
		}
		query.OccurredFrom = &parsed
	}
	if value := strings.TrimSpace(r.URL.Query().Get("occurred_to")); value != "" {
		parsed, err := time.Parse(time.RFC3339, value)
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid_occurred_to"})
			return
		}
		query.OccurredTo = &parsed
	}
	items, total, err := h.store.List(r.Context(), query)
	if err != nil {
		h.logger.Error("list security events", "error", err)
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": "security_event_unavailable"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": items, "total": total, "page": query.Page, "page_size": query.PageSize})
}

var (
	errInvalid = &invalidError{}
)

type invalidError struct{}

func (*invalidError) Error() string { return "invalid security event" }

func validResult(value string) bool {
	switch value {
	case "DENIED", "FAILURE", "SUCCESS", "BLOCKED":
		return true
	default:
		return false
	}
}

func validRisk(value string) bool {
	switch value {
	case "LOW", "MEDIUM", "HIGH", "CRITICAL":
		return true
	default:
		return false
	}
}

func trimLimit(value string, limit int) string {
	value = strings.TrimSpace(value)
	if len(value) > limit {
		return value[:limit]
	}
	return value
}

func parseInt(value string, fallback int) int {
	if parsed, err := strconv.Atoi(strings.TrimSpace(value)); err == nil && parsed > 0 {
		return parsed
	}
	return fallback
}

func randomID() string {
	buf := make([]byte, 16)
	if _, err := rand.Read(buf); err != nil {
		return strconv.FormatInt(time.Now().UnixNano(), 10)
	}
	return hex.EncodeToString(buf)
}

func writeJSON(w http.ResponseWriter, status int, payload any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(payload)
}
