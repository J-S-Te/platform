package bootstrap

import (
	"testing"
	"time"

	"github.com/J-S-Te/Basic-Platform/internal/shared/config"
)

// 旧 Secret 退役的运维开关映射（RUNTIME-05）：默认 keep 必须被接受且不阻断启动
// （保持既有轮换窗口与客户端兼容），显式 revoke/grace 才收紧旧值；非法配置在启动
// 阶段失败关闭，而不是静默降级为“旧密钥永久可用”。
// 策略落到服务实例后的签发行为由 applicationregistry/application 的
// oauth_client_secret_retirement_test.go 覆盖。
func TestOAuthClientSecretRetirementOption(t *testing.T) {
	tests := []struct {
		name        string
		policy      string
		grace       time.Duration
		wantStartup bool
	}{
		{name: "default keeps previous secrets", policy: "keep", grace: 24 * time.Hour},
		{name: "empty keeps previous secrets", policy: ""},
		{name: "revoke", policy: "REVOKE"},
		{name: "grace", policy: "grace", grace: 2 * time.Hour},
		{name: "unknown policy fails startup", policy: "forever", wantStartup: true},
		{name: "grace without window fails startup", policy: "grace", wantStartup: true},
		{name: "grace beyond cap fails startup", policy: "grace", grace: 31 * 24 * time.Hour, wantStartup: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			cfg := config.Config{}
			cfg.Auth.OAuthClientSecretPreviousRetirement = test.policy
			cfg.Auth.OAuthClientSecretPreviousGrace = test.grace
			option, err := oauthClientSecretRetirementOption(cfg)
			if test.wantStartup {
				if err == nil {
					t.Fatal("invalid retirement configuration must fail startup")
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if option == nil {
				t.Fatal("valid retirement configuration must produce an option")
			}
		})
	}
}
