package securityevent

import (
	"encoding/json"
	"net/http"
	"time"

	"github.com/J-S-Te/Basic-Platform/internal/shared/appctx"
	"gorm.io/gorm/clause"
)

type outboxHealthRow struct {
	ApplicationCode string
	EnvironmentCode string
	Pending         int64
	Dead            int64
	OldestAt        *time.Time
	ReportedAt      time.Time
}

// ReportOutboxHealth 接收子系统 worker 上报的最新 outbox 快照。
// 应用/环境取自应用令牌；同一应用环境覆盖为最新值。
func (h *Handler) ReportOutboxHealth(w http.ResponseWriter, r *http.Request) {
	principal, ok := appctx.PrincipalFromContext(r.Context())
	if !ok {
		writeJSON(w, http.StatusUnauthorized, map[string]any{"error": "unauthenticated"})
		return
	}
	var payload map[string]any
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&payload); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid_request"})
		return
	}
	pending, okPending := nonNegativeInt(payload["pending"])
	dead, okDead := nonNegativeInt(payload["dead"])
	if !okPending || !okDead {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid_count"})
		return
	}
	var oldest *time.Time
	if value, exists := payload["oldest_at"]; exists && value != nil && value != "" {
		parsed, err := time.Parse(time.RFC3339Nano, value.(string))
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid_oldest_at"})
			return
		}
		oldest = &parsed
	}
	now := time.Now().UTC()
	row := map[string]any{
		"application_code": principal.ApplicationCode, "environment_code": principal.EnvironmentCode,
		"pending": pending, "dead": dead, "oldest_at": oldest, "reported_at": now,
	}
	if err := h.store.DB.WithContext(r.Context()).Table("platform_outbox_health").Clauses(clause.OnConflict{
		Columns:   []clause.Column{{Name: "application_code"}, {Name: "environment_code"}},
		DoUpdates: clause.AssignmentColumns([]string{"pending", "dead", "oldest_at", "reported_at"}),
	}).Create(row).Error; err != nil {
		h.logger.Error("record outbox health", "error", err, "application", principal.ApplicationCode)
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": "outbox_health_unavailable"})
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]any{"status": "ACCEPTED", "reported_at": now})
}

func nonNegativeInt(value any) (int64, bool) {
	number, ok := value.(float64)
	if !ok || number < 0 || number != float64(int64(number)) {
		return 0, false
	}
	return int64(number), true
}

// latestOutboxHealth 返回应用/环境最新快照，供覆盖查询合并；失败时返回空表，
// 不把“上报缺失”伪装成 0。
func (h *Handler) latestOutboxHealth(r *http.Request) map[string]outboxHealthRow {
	result := map[string]outboxHealthRow{}
	if h == nil || h.store == nil || h.store.DB == nil {
		return result
	}
	var rows []outboxHealthRow
	if err := h.store.DB.WithContext(r.Context()).Table("platform_outbox_health").Find(&rows).Error; err != nil {
		h.logger.Error("read outbox health", "error", err)
		return result
	}
	for _, row := range rows {
		result[row.ApplicationCode] = row
	}
	return result
}
