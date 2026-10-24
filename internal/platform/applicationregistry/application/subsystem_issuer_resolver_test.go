package application

import (
	"errors"
	"testing"
)

func issuerResolverFor(t *testing.T, defaultAlias, platformIssuer string, keycloakEnabled bool, keycloakIssuer, keycloakRealm string) *SubsystemIssuerResolver {
	t.Helper()
	resolver, err := NewSubsystemIssuerResolver(defaultAlias, platformIssuer, keycloakEnabled, keycloakIssuer, keycloakRealm)
	if err != nil {
		t.Fatal(err)
	}
	return resolver
}

// 与既有 issuerForAlias 同语义：空/默认/platform/basic_platform/keycloak，其它别名拒绝。
func TestSubsystemIssuerResolverMatchesLegacySemantics(t *testing.T) {
	resolver := issuerResolverFor(t, "", "https://sso.example.com/realms/basic", true, "https://kc.example.com/realms/basic", "basic")
	cases := map[string]string{
		"":                 "https://sso.example.com/realms/basic",
		"platform":         "https://sso.example.com/realms/basic",
		"PLATFORM":         "https://sso.example.com/realms/basic",
		" basic_platform ": "https://sso.example.com/realms/basic",
		"keycloak":         "https://kc.example.com/realms/basic",
	}
	for alias, want := range cases {
		got, err := resolver.Resolve(alias)
		if err != nil || got != want {
			t.Fatalf("Resolve(%q) = %q err=%v, want %q", alias, got, err, want)
		}
	}
	for _, alias := range []string{"custom", "keycloak2", "oidc"} {
		if _, err := resolver.Resolve(alias); !errors.Is(err, ErrValidation) {
			t.Fatalf("Resolve(%q) err = %v, want ErrValidation", alias, err)
		}
	}
}

// 空别名回落到默认别名；Keycloak 未启用或缺配置即拒绝（不返回空 issuer）。
func TestSubsystemIssuerResolverFallsBackAndRejectsUnavailableKeycloak(t *testing.T) {
	resolver := issuerResolverFor(t, "keycloak", "https://sso.example.com/realms/basic", true, "https://kc.example.com/realms/basic", "basic")
	if got, err := resolver.Resolve(""); err != nil || got != "https://kc.example.com/realms/basic" {
		t.Fatalf("default alias fallback = %q err=%v", got, err)
	}

	disabled := issuerResolverFor(t, "", "https://sso.example.com/realms/basic", false, "https://kc.example.com/realms/basic", "basic")
	if _, err := disabled.Resolve("keycloak"); !errors.Is(err, ErrValidation) {
		t.Fatalf("disabled keycloak err = %v", err)
	}
	missingRealm := issuerResolverFor(t, "", "https://sso.example.com/realms/basic", true, "https://kc.example.com/realms/basic", "")
	if _, err := missingRealm.Resolve("keycloak"); !errors.Is(err, ErrValidation) {
		t.Fatalf("missing realm err = %v", err)
	}
	if _, err := NewSubsystemIssuerResolver("", "  ", false, "", ""); err == nil {
		t.Fatal("empty platform issuer must fail construction")
	}
}
