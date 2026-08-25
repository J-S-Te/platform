package infrastructure

import (
	"errors"
	"testing"

	"github.com/J-S-Te/Basic-Platform/internal/platform/externalidentity/application"
)

func TestDecideLegacyExternalLoginNameMigration(t *testing.T) {
	tests := []struct {
		name      string
		command   application.ProvisionCommand
		identity  identityModel
		account   externalLoginAccount
		wantName  string
		wantMove  bool
		wantError error
	}{
		{
			name:     "legacy account migrates to verified mobile",
			command:  application.ProvisionCommand{AccountNo: "13800138000", MobileDigest: []byte("mobile-digest")},
			identity: identityModel{AccountNo: "EXT-01M0LEGACY"},
			account:  externalLoginAccount{Username: "EXT-01M0LEGACY"},
			wantName: "13800138000",
			wantMove: true,
		},
		{
			name:     "already migrated account remains idempotent",
			command:  application.ProvisionCommand{AccountNo: "13800138000", MobileDigest: []byte("mobile-digest")},
			identity: identityModel{AccountNo: "13800138000"},
			account:  externalLoginAccount{Username: "13800138000"},
			wantName: "13800138000",
		},
		{
			name:      "identity and linked account drift fails closed",
			command:   application.ProvisionCommand{AccountNo: "13800138000", MobileDigest: []byte("mobile-digest")},
			identity:  identityModel{AccountNo: "EXT-01M0LEGACY"},
			account:   externalLoginAccount{Username: "EXT-ANOTHER"},
			wantError: application.ErrConflict,
		},
		{
			name:      "non legacy login cannot be renamed by provisioning",
			command:   application.ProvisionCommand{AccountNo: "13800138000", MobileDigest: []byte("mobile-digest")},
			identity:  identityModel{AccountNo: "customer-chosen-name"},
			account:   externalLoginAccount{Username: "customer-chosen-name"},
			wantError: application.ErrConflict,
		},
		{
			name:     "request without mobile does not migrate legacy account",
			command:  application.ProvisionCommand{AccountNo: "EXT-NEW"},
			identity: identityModel{AccountNo: "EXT-01M0LEGACY"},
			account:  externalLoginAccount{Username: "EXT-01M0LEGACY"},
			wantName: "EXT-01M0LEGACY",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			name, migrated, err := decideLegacyExternalLoginNameMigration(test.command, test.identity, test.account)
			if !errors.Is(err, test.wantError) {
				t.Fatalf("error = %v, want %v", err, test.wantError)
			}
			if name != test.wantName || migrated != test.wantMove {
				t.Fatalf("result = (%q, %t), want (%q, %t)", name, migrated, test.wantName, test.wantMove)
			}
		})
	}
}

func TestLegacyExternalLoginNameRecognition(t *testing.T) {
	for _, value := range []string{"EXT-01M0", " ext-01m0 "} {
		if !isLegacyExternalLoginName(value) {
			t.Fatalf("%q should be recognized as legacy external login name", value)
		}
	}
	for _, value := range []string{"13800138000", "customer", ""} {
		if isLegacyExternalLoginName(value) {
			t.Fatalf("%q must not be recognized as legacy external login name", value)
		}
	}
}
