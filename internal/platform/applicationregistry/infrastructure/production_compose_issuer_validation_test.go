package infrastructure

import "testing"

// 生产首次接入用"只允许 origin"的校验器校验 issuer，会把 Keycloak 的正常
// issuer（https://<host>/realms/<realm>）判为非法，导致首次接入和初次运行时采用
// 在预检阶段就被拒绝。issuer 与网关地址是两种不同的取值形态，必须分别校验。
func TestMustParseIssuerURLAcceptsKeycloakRealmIssuer(t *testing.T) {
	for _, value := range []string{
		"https://sso.example.com/realms/basic-platform",
		"http://keycloak:8080/realms/basic-platform",
		"https://platform.example.com",
		"https://sso.example.com/realms/basic-platform/",
	} {
		if parsed := mustParseIssuerURL(value); parsed == nil {
			t.Errorf("mustParseIssuerURL(%q) = nil, want parsed issuer", value)
		}
	}
}

func TestMustParseIssuerURLStillRejectsUnsafeIssuers(t *testing.T) {
	for _, value := range []string{
		"",
		"platform.example.com",
		"ftp://sso.example.com/realms/basic-platform",
		"https://user:secret@sso.example.com/realms/basic-platform",
		"https://sso.example.com/realms/basic-platform?realm=other",
		"https://sso.example.com/realms/basic-platform#fragment",
	} {
		if parsed := mustParseIssuerURL(value); parsed != nil {
			t.Errorf("mustParseIssuerURL(%q) = %v, want nil", value, parsed)
		}
	}
}

// 网关地址仍然必须是无路径 origin：issuer 的放宽不能顺带放宽公开地址校验。
func TestMustParseURLStillRequiresOriginOnly(t *testing.T) {
	if parsed := mustParseURL("https://sso.example.com/realms/basic-platform"); parsed != nil {
		t.Errorf("mustParseURL(带路径) = %v, want nil", parsed)
	}
	if parsed := mustParseURL("https://platform.example.com"); parsed == nil {
		t.Error("mustParseURL(纯 origin) = nil, want parsed")
	}
}
