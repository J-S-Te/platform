package http

import "context"

// ReadSubsystemAccessAuthObservation 把既有的 Keycloak 切换门禁读取暴露为端口：
// 只读取状态，不改变任何门禁；只有"观察期未结束/未开始/已存在"才判定为等待，
// 其余原因（配置错误、状态不可用等）一律**不冒充观察期**，交由上层按"未验证"处理。
func (handler *SubsystemOnboardingHandler) ReadSubsystemAccessAuthObservation(ctx context.Context, tenantID, applicationCode, environmentCode string) (bool, string, error) {
	if handler.keycloakCutover == nil {
		// 未启用切换能力：常规接入不存在观察期。
		return false, "cutover:disabled", nil
	}
	err := handler.keycloakCutover.CanKeycloakCutover(ctx, tenantID, applicationCode, environmentCode)
	if err == nil {
		return false, "cutover:allowed", nil
	}
	code := keycloakObservationReasonCode(err)
	switch code {
	// 只有「观察期已开始但未结束」才算等待；从未切换（无记录）或首次切换不属于阻塞条件。
	case "OBSERVATION_WINDOW_NOT_COMPLETED":
		return true, "cutover:" + code, nil
	default:
		// 非观察期原因：保持"未观察到"，且用原因码作为版本标记，便于预览显示"未验证"。
		return false, "cutover:" + code, nil
	}
}
