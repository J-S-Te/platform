package securityevent

import (
	"net/http"
	"time"
)

type coverageRow struct {
	ApplicationCode     string     `json:"application_code"`
	LastEventAt         *time.Time `json:"last_event_at"`
	Events24h           int64      `json:"events_24h"`
	LastSecurityEventAt *time.Time `json:"last_security_event_at"`
	SecurityEvents24h   int64      `json:"security_events_24h"`
	OutboxPending       *int64     `json:"outbox_pending"`
	OutboxDead          *int64     `json:"outbox_dead"`
	OutboxOldestAt      *time.Time `json:"outbox_oldest_at"`
}

func (h *Handler) Coverage(w http.ResponseWriter, r *http.Request) {
	if h == nil || h.store == nil || h.store.DB == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": "security_event_unavailable"})
		return
	}
	since := time.Now().UTC().Add(-24 * time.Hour)
	type auditAgg struct {
		ApplicationCode string
		LastEventAt     *time.Time
		Events24h       int64
	}
	var audits []auditAgg
	if err := h.store.DB.WithContext(r.Context()).Raw("SELECT application_code, MAX(occurred_at) AS last_event_at, SUM(CASE WHEN occurred_at >= ? THEN 1 ELSE 0 END) AS events_24h FROM audit_event GROUP BY application_code", since).Scan(&audits).Error; err != nil {
		h.logger.Error("audit coverage query failed", "error", err)
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": "audit_coverage_unavailable"})
		return
	}
	type securityAgg struct {
		ApplicationCode string
		LastEventAt     *time.Time
		Events24h       int64
	}
	var securities []securityAgg
	if err := h.store.DB.WithContext(r.Context()).Raw("SELECT application_code, MAX(occurred_at) AS last_event_at, SUM(CASE WHEN occurred_at >= ? THEN 1 ELSE 0 END) AS events_24h FROM platform_security_event GROUP BY application_code", since).Scan(&securities).Error; err != nil {
		h.logger.Error("security coverage query failed", "error", err)
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": "security_coverage_unavailable"})
		return
	}
	rows := make(map[string]*coverageRow)
	for _, item := range audits {
		rows[item.ApplicationCode] = &coverageRow{ApplicationCode: item.ApplicationCode, LastEventAt: item.LastEventAt, Events24h: item.Events24h}
	}
	for _, item := range securities {
		row := rows[item.ApplicationCode]
		if row == nil {
			row = &coverageRow{ApplicationCode: item.ApplicationCode}
			rows[item.ApplicationCode] = row
		}
		row.LastSecurityEventAt = item.LastEventAt
		row.SecurityEvents24h = item.Events24h
	}
	health := h.latestOutboxHealth(r)
	for key, item := range health {
		row := rows[key]
		if row == nil {
			row = &coverageRow{ApplicationCode: key}
			rows[key] = row
		}
		pending, dead, oldest := item.Pending, item.Dead, item.OldestAt
		row.OutboxPending, row.OutboxDead, row.OutboxOldestAt = &pending, &dead, oldest
	}
	items := make([]coverageRow, 0, len(rows))
	for _, row := range rows {
		items = append(items, *row)
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": items, "generated_at": time.Now().UTC(), "outbox_available": len(health) > 0, "note": "outbox 快照由各子系统 worker 周期上报；未上报时字段为 null。"})
}
