package application

import "strings"

// SubsystemUpdateBrowserClientName 返回受控更新使用的浏览器客户端规范名：应用编码-环境-web。
// 与既有实现一致（大小写归一化：编码与环境均转小写）。
func SubsystemUpdateBrowserClientName(applicationCode, environment string) string {
	return strings.ToLower(strings.TrimSpace(applicationCode)) + "-" + strings.ToLower(strings.TrimSpace(environment)) + "-web"
}

// SubsystemUpdateBrowserClientExpectation 描述浏览器客户端在受控更新前必须满足的归属与类型。
func SubsystemUpdateBrowserClientExpectation(tenantID, applicationID, environmentID, applicationCode, environment, redirectURI string) SubsystemUpdateServiceClientExpectation {
	requirePKCE := true
	return SubsystemUpdateServiceClientExpectation{
		CanonicalName:   SubsystemUpdateBrowserClientName(applicationCode, environment),
		TenantID:        strings.TrimSpace(tenantID),
		ApplicationID:   strings.TrimSpace(applicationID),
		EnvironmentID:   strings.TrimSpace(environmentID),
		ClientType:      "confidential",
		TokenAuthMethod: "client_secret_basic",
		GrantTypes:      []string{"authorization_code", "refresh_token"},
		// 与既有实现一致：浏览器客户端必须启用 PKCE，且回调地址必须精确匹配。
		RequirePKCE:  &requirePKCE,
		RedirectURIs: []string{strings.TrimSpace(redirectURI)},
		// 浏览器客户端的范围随授权目录变化，不在此处约束（与既有实现一致）。
	}
}
