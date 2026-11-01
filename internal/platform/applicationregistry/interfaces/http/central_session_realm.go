package http

// WithCentralSessionGovernance 在所有RP就绪后使用中央唯一业务空闲时钟。
// 参数：enabled为受控运行开关；调用方须先完成中央强制校验部署。
// 返回值：当前控制面，便于组合根保持单一配置来源。
func (control *keycloakControlPlane) WithCentralSessionGovernance(enabled bool) *keycloakControlPlane {
	control.centralSessionGovernance = enabled
	return control
}

// centralRealmSessionPatch 设置上游技术期限，业务三十分钟空闲由中央严格执行。
// 参数：realm为当前完整Realm投影，enabled表示中央强制校验已启用。
// 返回值：是否变化及完整更新投影；关闭时不改变上游现有策略。
func centralRealmSessionPatch(realm map[string]any, enabled bool) (bool, map[string]any) {
	if !enabled {
		return false, realm
	}
	payload := make(map[string]any, len(realm)+4)
	for key, value := range realm {
		payload[key] = value
	}
	changed := false
	for _, key := range []string{"ssoSessionIdleTimeout", "ssoSessionMaxLifespan", "clientSessionIdleTimeout", "clientSessionMaxLifespan"} {
		if !asInt64Equal(realm[key], 28800) {
			changed = true
		}
		payload[key] = int64(28800)
	}
	return changed, payload
}
