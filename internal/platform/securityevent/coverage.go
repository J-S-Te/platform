package securityevent

import (
	"net/http"
	"time"

	"github.com/J-S-Te/Basic-Platform/internal/shared/httperror"
	"github.com/J-S-Te/Basic-Platform/internal/shared/httpresponse"
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

// Coverage 汇总平台已接收的审计、安全事件及各应用的上报状态。
// 参数：w 为响应写入器，r 提供查询上下文及请求编号。
// 返回值：无；返回覆盖度数据，聚合依赖失败时返回可诊断的错误。
func (h *Handler) Coverage(w http.ResponseWriter, r *http.Request) {
	if h == nil || h.store == nil || h.store.DB == nil {
		httpresponse.WriteError(w, r, http.StatusServiceUnavailable,
			httperror.New("AUDIT_COVERAGE_UNAVAILABLE", "审计覆盖度暂不可用", nil))
		return
	}
	since := time.Now().UTC().Add(-24 * time.Hour)
	type auditAgg struct {
		ApplicationCode string
		LastEventAt     *time.Time
		Events24h       int64
	}
	var audits []auditAgg
	const auditCoverageQuery = `SELECT app.code AS application_code, MAX(event.occurred_at) AS last_event_at,
		SUM(CASE WHEN event.occurred_at >= ? THEN 1 ELSE 0 END) AS events_24h
		FROM audit_event AS event
		JOIN platform_application AS app ON app.id = event.application_id AND app.tenant_id = event.tenant_id
		GROUP BY app.code`
	if err := h.store.DB.WithContext(r.Context()).Raw(auditCoverageQuery, since).Scan(&audits).Error; err != nil {
		h.logger.Error("audit coverage query failed", "error", err)
		httpresponse.WriteError(w, r, http.StatusServiceUnavailable,
			httperror.New("AUDIT_COVERAGE_UNAVAILABLE", "审计覆盖度暂不可用", nil))
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
		httpresponse.WriteError(w, r, http.StatusServiceUnavailable,
			httperror.New("SECURITY_COVERAGE_UNAVAILABLE", "安全事件覆盖度暂不可用", nil))
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
	httpresponse.WriteSuccess(w, r, http.StatusOK, "审计覆盖度查询成功", map[string]any{
		"items": items, "generated_at": time.Now().UTC(), "outbox_available": len(health) > 0,
		"note": "outbox 快照由各子系统 worker 周期上报；未上报时字段为 null。",
	})
}
