package backchannel

// platform_oauth_backchannel_logout_outbox.status 的受控取值。状态是投递语义的一部分：
// 生成侧与投递侧都必须使用同一组常量，避免"某处少写一个状态便静默跳过"。
const (
	// StatusPending 表示任务已生成、端点已登记，等待首次投递。
	StatusPending = "PENDING"
	// StatusProcessing 表示任务已被某个 Worker 通过租约抢占。
	StatusProcessing = "PROCESSING"
	// StatusRetry 表示投递失败，等待指数退避后的下一次尝试。
	StatusRetry = "RETRY"
	// StatusDelivered 表示投递完成。
	StatusDelivered = "DELIVERED"
	// StatusFailed 表示尝试次数达到上限，进入死信。
	StatusFailed = "FAILED"
	// StatusNoEndpoint 表示通知本应产生、但该客户端没有任何 logout_uri 登记
	// （BIZ-XEVT2-07）：任务照常落库，绝不静默丢弃；Claim 不抢占该状态，
	// gauge 与审计使其可见，登记补齐后由 Claim 在同一事务内重新武装为 PENDING。
	StatusNoEndpoint = "NO_ENDPOINT"
)

// ReasonMissingLogoutURI 是"本应通知却无登记端点"的稳定原因码，写入 outbox 的
// last_error 与审计事件 metadata，供告警规则与运维检索使用（不得改写为自由文本）。
const ReasonMissingLogoutURI = "missing_logout_uri"

// MissingLogoutURIMetadata 返回"本应通知却无 logout_uri 登记"的审计事件 metadata。
// 审计事实必须能被机器检索，因此键名固定为 missing_logout_uri。
func MissingLogoutURIMetadata(oauthClientID string) map[string]any {
	return map[string]any{ReasonMissingLogoutURI: true, "oauth_client_id": oauthClientID}
}

// IsDeliverable 判断某状态是否允许进入投递。NO_ENDPOINT 明确不可投递：
// 它代表"缺少登记"这一配置缺陷，而不是可以无限重试的传输故障。
func IsDeliverable(status string) bool {
	switch status {
	case StatusPending, StatusRetry, StatusProcessing:
		return true
	}
	return false
}
