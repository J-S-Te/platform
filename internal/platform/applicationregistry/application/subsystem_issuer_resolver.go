package application

import (
	"errors"
	"strings"
)

// SubsystemIssuerResolver 按认证提供方别名解析 issuer。
// 由既有 Handler 的 issuerForAlias 提取而来，**语义逐条保持一致**：
// 空别名回落到默认别名；platform/basic_platform → 平台 issuer；keycloak → Keycloak issuer（未启用或缺配置即 ErrValidation）；其它别名拒绝。
type SubsystemIssuerResolver struct {
	DefaultAlias    string
	PlatformIssuer  string
	KeycloakEnabled bool
	KeycloakIssuer  string
	KeycloakRealm   string
}

// NewSubsystemIssuerResolver 构造解析器；平台 issuer 缺失即失败（不允许返回空 issuer 去部署）。
func NewSubsystemIssuerResolver(defaultAlias, platformIssuer string, keycloakEnabled bool, keycloakIssuer, keycloakRealm string) (*SubsystemIssuerResolver, error) {
	if strings.TrimSpace(platformIssuer) == "" {
		return nil, errors.New("subsystem issuer resolver requires a platform issuer")
	}
	return &SubsystemIssuerResolver{
		DefaultAlias:    strings.ToLower(strings.TrimSpace(defaultAlias)),
		PlatformIssuer:  strings.TrimSpace(platformIssuer),
		KeycloakEnabled: keycloakEnabled,
		KeycloakIssuer:  strings.TrimSpace(keycloakIssuer),
		KeycloakRealm:   strings.TrimSpace(keycloakRealm),
	}, nil
}

// Resolve 返回该提供方别名对应的 issuer；未知别名或缺少 Keycloak 配置时返回 ErrValidation。
func (resolver *SubsystemIssuerResolver) Resolve(alias string) (string, error) {
	normalized := strings.ToLower(strings.TrimSpace(alias))
	if normalized == "" && resolver.DefaultAlias != "" {
		normalized = resolver.DefaultAlias
	}
	switch normalized {
	case "", "platform", "basic_platform":
		return resolver.PlatformIssuer, nil
	case "keycloak":
		if !resolver.KeycloakEnabled || resolver.KeycloakIssuer == "" || resolver.KeycloakRealm == "" {
			return "", ErrValidation
		}
		return resolver.KeycloakIssuer, nil
	default:
		return "", ErrValidation
	}
}
