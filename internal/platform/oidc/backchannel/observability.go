package backchannel

import (
	"context"
	"errors"

	"gorm.io/gorm"
)

// Back-Channel Logout 的可观测面（BIZ-XEVT2-07）：
// 未登记 logout_uri 的 RP 曾经表现为"没有任务、没有告警"，这里把"本应通知却无端点"
// 变成可计数的 gauge 与可盘点的只读清单。
const (
	// MetricUnregisteredRPs 是"存在 ACTIVE token family 但没有 logout_uri 登记"的
	// DISTINCT 客户端数。>0 表示这些 RP 的登出通知无法送达（fail-closed 于监控面）。
	MetricUnregisteredRPs = "platform_oauth_backchannel_unregistered_rp"
	// MetricNoEndpointBacklog 是已生成但缺少登记端点、不可投递的 outbox 行数。
	MetricNoEndpointBacklog = "platform_oauth_backchannel_logout_no_endpoint"
)

// Gauge 是一条待输出的 Prometheus gauge 采样；指标名固定，值是该维度的计数。
type Gauge struct {
	Name  string
	Value int64
}

// UnregisteredClient 描述一个"有活跃登录状态但缺少 logout_uri 登记"的客户端。
// 只读盘点结果用于运维确认后批量登记，不包含任何凭据或会话内容。
type UnregisteredClient struct {
	TenantID       string
	OAuthClientID  string
	ClientID       string
	ActiveFamilies int64
}

// unregisteredClientsSQL 是 gauge 与盘点共用的唯一查询：以 oauth_token_family 为准，
// 而不是以 outbox 为准——尚未发生登出时也必须能发现缺登记。
const unregisteredClientsSQL = `SELECT family.tenant_id AS tenant_id,
	family.oauth_client_id AS oauth_client_id,
	COALESCE(client.client_id, '') AS client_id,
	COUNT(DISTINCT family.id) AS active_families
FROM oauth_token_family family
LEFT JOIN platform_oauth_client client ON client.id = family.oauth_client_id
WHERE family.status = ? AND family.revoked_at IS NULL
	AND NOT EXISTS (
		SELECT 1 FROM platform_oauth_backchannel_logout_uri uri
		WHERE uri.oauth_client_id = family.oauth_client_id AND uri.logout_uri <> ''
	)
GROUP BY family.tenant_id, family.oauth_client_id, client.client_id
ORDER BY family.tenant_id, client.client_id, family.oauth_client_id`

// UnregisteredClients 只读盘点缺少 logout_uri 登记的客户端（存量回填入口）。
// 它是只读函数：不写登记、不改状态，运维确认后再走受控更新/幂等重放补登记。
func UnregisteredClients(ctx context.Context, database *gorm.DB) ([]UnregisteredClient, error) {
	if database == nil {
		return nil, errors.New("back-channel logout inventory requires a database")
	}
	var rows []UnregisteredClient
	if err := database.WithContext(ctx).Raw(unregisteredClientsSQL, "ACTIVE").Scan(&rows).Error; err != nil {
		return nil, err
	}
	return rows, nil
}

// gaugeSamples 把两次计数转成 gauge 采样，便于注入式单测与统一输出。
func gaugeSamples(unregisteredRPs, noEndpointBacklog int64) []Gauge {
	return []Gauge{
		{Name: MetricUnregisteredRPs, Value: unregisteredRPs},
		{Name: MetricNoEndpointBacklog, Value: noEndpointBacklog},
	}
}

// Gauges 采集 Back-Channel Logout 的两条 gauge。调用方（/metrics 采集器）负责缓存与
// 超时；本函数只做两次廉价查询，失败时返回错误而不是输出 0——把采集失败伪装成
// "一切正常"会重新引入静默。
func Gauges(ctx context.Context, database *gorm.DB) ([]Gauge, error) {
	if database == nil {
		return nil, errors.New("back-channel logout gauges require a database")
	}
	unregistered, err := UnregisteredClients(ctx, database)
	if err != nil {
		return nil, err
	}
	var noEndpointBacklog int64
	if err := database.WithContext(ctx).Table("platform_oauth_backchannel_logout_outbox").
		Where("status = ?", StatusNoEndpoint).Count(&noEndpointBacklog).Error; err != nil {
		return nil, err
	}
	return gaugeSamples(int64(len(unregistered)), noEndpointBacklog), nil
}
