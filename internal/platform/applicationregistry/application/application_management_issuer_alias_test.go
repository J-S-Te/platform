package application

import "testing"

func TestNormalizeIssuerAliasBackfillsLegacyEmptyValuesToPlatform(t *testing.T) {
	t.Parallel()

	blank := "  "
	for _, input := range []*string{nil, &blank} {
		got := normalizeIssuerAlias(input)
		if got == nil || *got != IssuerAliasPlatform {
			t.Fatalf("normalizeIssuerAlias(%v) = %v, want %q", input, got, IssuerAliasPlatform)
		}
	}
}

func TestNormalizeIssuerAliasCanonicalizesKnownProviders(t *testing.T) {
	t.Parallel()

	value := " KeyCloak "
	got := normalizeIssuerAlias(&value)
	if got == nil || *got != IssuerAliasKeycloak {
		t.Fatalf("normalizeIssuerAlias(%q) = %v, want %q", value, got, IssuerAliasKeycloak)
	}
}

func TestValidIssuerAliasAllowsOnlyPlatformAndKeycloak(t *testing.T) {
	t.Parallel()

	for _, test := range []struct {
		name  string
		value *string
		want  bool
	}{
		{name: "platform", value: stringPointer(IssuerAliasPlatform), want: true},
		{name: "keycloak", value: stringPointer(IssuerAliasKeycloak), want: true},
		{name: "missing", value: nil, want: false},
		{name: "legacy alias", value: stringPointer("basic_platform"), want: false},
		{name: "unknown provider", value: stringPointer("external_oidc"), want: false},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := validIssuerAlias(test.value); got != test.want {
				t.Fatalf("validIssuerAlias(%v) = %v, want %v", test.value, got, test.want)
			}
		})
	}
}

func TestEnvironmentInputsRejectUnknownIssuerAliases(t *testing.T) {
	t.Parallel()

	unknown := "external_oidc"
	create := normalizeEnvironmentCreate(EnvironmentCreateInput{
		TenantID: "tenant-1", OperatorID: "operator-1", ApplicationID: "application-1", Environment: "prod",
		IssuerAlias: &unknown, Status: "ACTIVE",
	})
	if validEnvironmentCreate(create) {
		t.Fatal("environment create accepted an unknown issuer alias")
	}

	update := normalizeEnvironmentUpdate(EnvironmentUpdateInput{
		TenantID: "tenant-1", OperatorID: "operator-1", ApplicationID: "application-1", EnvironmentID: "environment-1",
		IssuerAlias: &unknown, Status: "ACTIVE", Version: 1,
	})
	if validEnvironmentUpdate(update) {
		t.Fatal("environment update accepted an unknown issuer alias")
	}
}
