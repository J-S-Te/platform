package application

import (
	"errors"
	"testing"
	"time"
)

func TestValidateBindingAcceptsSupportedScopedAuthorization(t *testing.T) {
	now := time.Now().UTC()
	for _, scopeType := range []string{"ORG_UNIT", "RESOURCE"} {
		scopeID := "scope-1"
		if err := validateBinding("tenant-1", "operator-1", "role-1", "USER", "user-1", scopeType, &scopeID, nil, now); err != nil {
			t.Fatalf("%s binding unexpectedly rejected: %v", scopeType, err)
		}
	}
}

func TestValidateBindingRejectsInvalidScopeCombinations(t *testing.T) {
	now := time.Now().UTC()
	empty := ""
	nonEmpty := "scope-1"
	tests := []struct {
		name      string
		scopeType string
		scopeID   *string
	}{
		{name: "tenant with scope ID", scopeType: "TENANT", scopeID: &nonEmpty},
		{name: "organization without scope ID", scopeType: "ORG_UNIT", scopeID: nil},
		{name: "resource with blank scope ID", scopeType: "RESOURCE", scopeID: &empty},
		{name: "unsupported scope", scopeType: "ENVIRONMENT", scopeID: &nonEmpty},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := validateBinding("tenant-1", "operator-1", "role-1", "USER", "user-1", test.scopeType, test.scopeID, nil, now)
			if !errors.Is(err, ErrValidation) {
				t.Fatalf("validation error = %v, want ErrValidation", err)
			}
		})
	}
}
