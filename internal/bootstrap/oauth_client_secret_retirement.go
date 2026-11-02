package bootstrap

import (
	"fmt"
	"strings"

	applicationregistryapplication "github.com/J-S-Te/Basic-Platform/internal/platform/applicationregistry/application"
	"github.com/J-S-Te/Basic-Platform/internal/shared/config"
)

// oauthClientSecretRetirementOption 把运维配置映射为 OAuth 客户端管理服务的旧 Secret
// 退役策略（RUNTIME-05 的显式开关）。默认 keep：不改变现网密钥轮换窗口与客户端兼容性。
// 只有显式配置 revoke/grace 时，受控更新与许可登记追加新 Secret 才会在同一事务内
// 吊销或收紧旧值；宽限窗口只缩短有效期，绝不延长既有截止时间。
func oauthClientSecretRetirementOption(cfg config.Config) (applicationregistryapplication.OAuthClientManagementOption, error) {
	policy := strings.TrimSpace(cfg.Auth.OAuthClientSecretPreviousRetirement)
	grace := cfg.Auth.OAuthClientSecretPreviousGrace
	if !strings.EqualFold(policy, string(applicationregistryapplication.OAuthClientSecretRetirementGrace)) {
		// keep/revoke 不使用宽限窗口；避免应用层把多余窗口当成配置错误。
		grace = 0
	}
	retirement, err := applicationregistryapplication.ParseOAuthClientSecretRetirement(policy, grace)
	if err != nil {
		return nil, fmt.Errorf("AUTH_OAUTH_CLIENT_SECRET_PREVIOUS_RETIREMENT: %w", err)
	}
	return applicationregistryapplication.WithOAuthClientSecretRetirement(retirement), nil
}
