package application

import (
	"fmt"
	"net/url"
	"strings"
)

// CentralSessionRuntimePolicy 只由基础平台组合根构造，随签名部署请求下发。
// 不包含RP自定义期限；期限始终由中央会话状态接口裁决。
type CentralSessionRuntimePolicy struct {
	Enabled       bool   `json:"enabled"`
	PublicBaseURL string `json:"public_base_url"`
}

// NewCentralSessionRuntimePolicy 验证基础平台公开来源，禁止部署Agent猜测浏览器入口。
// 参数：enabled为平台总开关，publicBaseURL为已审核的平台公开来源。
// 返回值：可签名的统一接入策略；非纯HTTP来源或含凭据时返回校验错误。
func NewCentralSessionRuntimePolicy(enabled bool, publicBaseURL string) (*CentralSessionRuntimePolicy, error) {
	parsed, err := url.Parse(strings.TrimSpace(publicBaseURL))
	if err != nil || parsed.Host == "" || parsed.User != nil ||
		(parsed.Scheme != "http" && parsed.Scheme != "https") ||
		(parsed.Path != "" && parsed.Path != "/") || parsed.RawQuery != "" || parsed.Fragment != "" {
		return nil, fmt.Errorf("central session public origin: %w", ErrValidation)
	}
	return &CentralSessionRuntimePolicy{Enabled: enabled, PublicBaseURL: parsed.Scheme + "://" + parsed.Host}, nil
}
