package backchannel

import (
	"strings"
	"testing"
)

// gauge 采样必须输出固定指标名与真实计数：缺少 logout_uri 的 RP 数曾经完全不可见。
func TestGaugeSamplesExposeUnregisteredRPsAndNoEndpointBacklog(t *testing.T) {
	gauges := gaugeSamples(3, 7)
	if len(gauges) != 2 {
		t.Fatalf("gauges = %#v", gauges)
	}
	want := map[string]int64{MetricUnregisteredRPs: 3, MetricNoEndpointBacklog: 7}
	for _, gauge := range gauges {
		if want[gauge.Name] != gauge.Value {
			t.Fatalf("gauge %s = %d, want %d", gauge.Name, gauge.Value, want[gauge.Name])
		}
	}
	if MetricUnregisteredRPs != "platform_oauth_backchannel_unregistered_rp" || MetricNoEndpointBacklog != "platform_oauth_backchannel_logout_no_endpoint" {
		t.Fatalf("metric names changed: %s / %s", MetricUnregisteredRPs, MetricNoEndpointBacklog)
	}
}

// 盘点 SQL 必须按 ACTIVE token family 统计 DISTINCT 客户端，并且只把"没有任何
// logout_uri 登记"的客户端算作缺口；否则 gauge 会把已登记 RP 也算进告警。
func TestUnregisteredClientsSQLCountsDistinctActiveFamilies(t *testing.T) {
	normalized := strings.Join(strings.Fields(unregisteredClientsSQL), " ")
	for _, fragment := range []string{
		"COUNT(DISTINCT family.id)",
		"family.status = ?",
		"family.revoked_at IS NULL",
		"NOT EXISTS ( SELECT 1 FROM platform_oauth_backchannel_logout_uri uri",
		"GROUP BY family.tenant_id, family.oauth_client_id",
	} {
		if !strings.Contains(normalized, fragment) {
			t.Fatalf("inventory SQL lost %q:\n%s", fragment, normalized)
		}
	}
}

// Claim 只领取可投递状态：NO_ENDPOINT 是配置缺陷，不能被 Worker 抢走并反复重试。
func TestClaimQueryNeverClaimsNoEndpointRows(t *testing.T) {
	normalized := strings.Join(strings.Fields(claimSelectSQL), " ")
	if strings.Contains(normalized, StatusNoEndpoint) {
		t.Fatalf("claim query mentions %s and may pick it up:\n%s", StatusNoEndpoint, normalized)
	}
	if !strings.Contains(normalized, "o.status IN ('PENDING','RETRY')") || !strings.Contains(normalized, "FOR UPDATE SKIP LOCKED") {
		t.Fatalf("claim query changed shape:\n%s", normalized)
	}
	for _, name := range []string{StatusPending, StatusRetry, StatusProcessing} {
		if !IsDeliverable(name) {
			t.Fatalf("status %s should be deliverable", name)
		}
	}
	for _, name := range []string{StatusNoEndpoint, StatusDelivered, StatusFailed} {
		if IsDeliverable(name) {
			t.Fatalf("status %s must never be claimed for delivery", name)
		}
	}
}

// 端点消失与补登记都必须由 Claim 的同一事务收敛：前者降级为 NO_ENDPOINT（可见），
// 后者重新武装为 PENDING（可补投），不允许任何一侧被 JOIN 静默跳过。
func TestClaimSynchronizesMissingAndRestoredEndpoints(t *testing.T) {
	demote := strings.Join(strings.Fields(demoteMissingEndpointSQL), " ")
	if !strings.Contains(demote, "SET o.status=?,o.locked_until=NULL,o.last_error=?") || !strings.Contains(demote, "NOT EXISTS (SELECT 1 FROM platform_oauth_backchannel_logout_uri") {
		t.Fatalf("demote statement lost its guarantee:\n%s", demote)
	}
	rearm := strings.Join(strings.Fields(rearmRegisteredEndpointSQL), " ")
	if !strings.Contains(rearm, "SET o.status=?,o.next_attempt_at=?,o.last_error=NULL") ||
		!strings.Contains(rearm, "EXISTS (SELECT 1 FROM platform_oauth_backchannel_logout_uri") {
		t.Fatalf("rearm statement lost its guarantee:\n%s", rearm)
	}
}

func TestMissingLogoutURIMetadataIsMachineReadable(t *testing.T) {
	metadata := MissingLogoutURIMetadata("01KOAUTH000000000000000001")
	if metadata[ReasonMissingLogoutURI] != true || metadata["oauth_client_id"] != "01KOAUTH000000000000000001" {
		t.Fatalf("metadata = %#v", metadata)
	}
	if ReasonMissingLogoutURI != "missing_logout_uri" {
		t.Fatalf("reason code changed to %q", ReasonMissingLogoutURI)
	}
}
