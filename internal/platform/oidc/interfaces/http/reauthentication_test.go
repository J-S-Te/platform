package oidchttp

import (
	"net/http/httptest"
	"testing"
)

// TestAuthorizationMaxAgeParsing 验证新鲜认证参数不能被静默忽略或通过溢出绕过。
// 参数：t为原生授权请求解析测试上下文。
// 返回值：无；负数、重复、非数字或超界被接受时失败。
func TestAuthorizationMaxAgeParsing(t *testing.T) {
	base := "https://platform.test/authorize?client_id=client&response_type=code&redirect_uri=https%3A%2F%2Fapp.test%2Fcallback&scope=openid"
	for _, value := range []string{"-1", "bad", "4294967296", "1&max_age=2"} {
		if _, err := parseAuthorizationRequest(httptest.NewRequest("GET", base+"&max_age="+value, nil)); err == nil {
			t.Errorf("invalid max_age accepted: %s", value)
		}
	}
	parsed, err := parseAuthorizationRequest(httptest.NewRequest("GET", base+"&prompt=login&max_age=0", nil))
	if err != nil || parsed.input.Prompt != "login" || parsed.input.MaxAgeSeconds == nil || *parsed.input.MaxAgeSeconds != 0 {
		t.Fatal("freshness request lost", err)
	}
}
