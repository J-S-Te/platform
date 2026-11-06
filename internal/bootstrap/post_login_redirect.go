package bootstrap

import (
	applicationregistryapplication "github.com/J-S-Te/Basic-Platform/internal/platform/applicationregistry/application"
	"github.com/J-S-Te/Basic-Platform/internal/shared/config"
)

// postLoginRedirectPolicy 汇总登录成功后允许跳转的绝对地址来源，供登记路径
// （应用/环境管理、子系统接入、登录目标管理）与解析路径（登录完成重定向）共用同一口径：
//   - 平台公开 origin（APP_PUBLIC_BASE_URL、AUTH.OIDCIssuer）与 CORS 允许来源；
//   - 平台自身 OIDC issuer 与 Keycloak 公开 issuer（浏览器登录往返都在这些来源上）；
//   - AUTH_LOGIN_TARGET_ALLOWED_ORIGINS 显式声明的子系统公开来源。
//
// 未列出的绝对目标在登记时被拒绝、在解析时失败关闭为平台同源页面。此函数只读取
// 受信启动配置，绝不读取请求输入。
func postLoginRedirectPolicy(cfg config.Config) applicationregistryapplication.PostLoginRedirectPolicy {
	entries := make([]string, 0, len(cfg.CORSOrigins)+len(cfg.Auth.LoginTargetAllowedOrigins)+5)
	entries = append(entries,
		cfg.HTTP.PublicBaseURL,
		cfg.Auth.PublicBaseURL,
		cfg.Auth.OIDCIssuer,
		cfg.Auth.KeycloakOIDCIssuer,
		cfg.Keycloak.PublicURL,
	)
	entries = append(entries, cfg.CORSOrigins...)
	entries = append(entries, cfg.Auth.LoginTargetAllowedOrigins...)
	return applicationregistryapplication.NewPostLoginRedirectPolicy(entries...)
}
