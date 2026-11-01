package http

import "testing"

// TestCentralClientSessionAttributes 验证平台统一周期不会被旧客户端的更短技术期限打断。
// 参数：t为配置投影测试上下文。
// 返回值：无；未启用时改变策略、启用后未覆盖短期限或覆盖无关属性即失败。
func TestCentralClientSessionAttributes(t *testing.T) {
	old := map[string]string{"client.session.idle.timeout": "60", "client.session.max.lifespan": "300", "pkce.code.challenge.method": "S256"}
	if got := centralClientSessionAttributes(old, false); got != nil {
		t.Fatal("disabled policy must not alter attributes")
	}
	got := centralClientSessionAttributes(old, true)
	if got["client.session.idle.timeout"] != "28800" || got["client.session.max.lifespan"] != "28800" || got["pkce.code.challenge.method"] != "S256" {
		t.Fatal("central technical policy was not projected independently")
	}
	if old["client.session.idle.timeout"] != "60" {
		t.Fatal("input representation mutated")
	}
}
