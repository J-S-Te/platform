package http

// centralClientSessionAttributes 让平台管理的在线客户端使用八小时技术窗口，业务空闲仍由中央判定。
// 参数：previous为原客户端属性，enabled为平台受控总开关。
// 返回值：启用时返回保留其他属性的新投影；禁用时返回nil以保留旧策略。
func centralClientSessionAttributes(previous map[string]string, enabled bool) map[string]string {
	if !enabled {
		return nil
	}
	result := make(map[string]string, len(previous)+2)
	for key, value := range previous {
		result[key] = value
	}
	result["client.session.idle.timeout"] = "28800"
	result["client.session.max.lifespan"] = "28800"
	return result
}
