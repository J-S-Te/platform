package application

import (
	"errors"
	"strings"
)

// SubsystemUpdateServiceClientExpectation 是"既有服务客户端是否满足受控更新前提"的期望集合。
type SubsystemUpdateServiceClientExpectation struct {
	CanonicalName   string
	TenantID        string
	ApplicationID   string
	EnvironmentID   string
	ClientType      string
	TokenAuthMethod string
	GrantTypes      []string
	Scopes          []string
	// RequirePKCE 非空时要求客户端启用 PKCE（浏览器客户端必须为 true）。
	RequirePKCE *bool
	// RedirectURIs 非空时要求客户端回调地址**精确包含**其中的每一项（浏览器客户端）。
	RedirectURIs []string
}

// ErrSubsystemUpdateServiceClientInvalid 表示既有服务客户端不满足受控更新前提。
var ErrSubsystemUpdateServiceClientInvalid = errors.New("registered service client does not satisfy the controlled update preconditions")

// ValidateSubsystemUpdateServiceClient 只读校验既有服务客户端是否可用于受控更新。
// 规则与既有实现逐条一致：规范名、记录 ID、归属（租户/应用/环境）、状态 ACTIVE、类型、认证方式、授权类型、范围。
// 本函数不签发任何密钥，也不修改任何状态。
func ValidateSubsystemUpdateServiceClient(client OAuthClientView, expectation SubsystemUpdateServiceClientExpectation) error {
	if strings.TrimSpace(expectation.TenantID) == "" || strings.TrimSpace(expectation.ApplicationID) == "" || strings.TrimSpace(expectation.EnvironmentID) == "" {
		return ErrSubsystemUpdateServiceClientInvalid
	}
	if strings.TrimSpace(client.ID) == "" || strings.TrimSpace(client.ClientID) != strings.TrimSpace(expectation.CanonicalName) {
		return ErrSubsystemUpdateServiceClientInvalid
	}
	if client.TenantID != expectation.TenantID || client.ApplicationID != expectation.ApplicationID || client.EnvironmentID != expectation.EnvironmentID {
		return ErrSubsystemUpdateServiceClientInvalid
	}
	if !strings.EqualFold(strings.TrimSpace(client.Status), "ACTIVE") {
		return ErrSubsystemUpdateServiceClientInvalid
	}
	if strings.TrimSpace(expectation.ClientType) != "" && client.ClientType != expectation.ClientType {
		return ErrSubsystemUpdateServiceClientInvalid
	}
	if strings.TrimSpace(expectation.TokenAuthMethod) != "" && client.TokenAuthMethod != expectation.TokenAuthMethod {
		return ErrSubsystemUpdateServiceClientInvalid
	}
	if len(expectation.GrantTypes) > 0 && !sameStringSetValues(client.GrantTypes, expectation.GrantTypes) {
		return ErrSubsystemUpdateServiceClientInvalid
	}
	if len(expectation.Scopes) > 0 && !sameStringSetValues(client.Scopes, expectation.Scopes) {
		return ErrSubsystemUpdateServiceClientInvalid
	}
	if expectation.RequirePKCE != nil && client.RequirePKCE != *expectation.RequirePKCE {
		return ErrSubsystemUpdateServiceClientInvalid
	}
	for _, redirectURI := range expectation.RedirectURIs {
		expected := strings.TrimSpace(redirectURI)
		if expected == "" {
			continue
		}
		matched := false
		for _, candidate := range client.RedirectURIs {
			if strings.TrimSpace(candidate) == expected {
				matched = true
				break
			}
		}
		if !matched {
			return ErrSubsystemUpdateServiceClientInvalid
		}
	}
	return nil
}

// SubsystemUpdateServiceClientName 返回既有实现使用的规范名：应用编码-环境-用途后缀。
func SubsystemUpdateServiceClientName(applicationCode, environment, suffix string) string {
	// 与既有实现一致：不做小写归一化（环境值由上层保证规范形式）。
	return applicationCode + "-" + environment + "-" + suffix
}
