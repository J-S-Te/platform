package applicationaccess

import (
	"context"
	"errors"
	"testing"

	authapplication "github.com/J-S-Te/Basic-Platform/internal/platform/authorization/application"
)

func TestManualBindingsDelegationDistinguishesScopeReductionFromExpansion(t *testing.T) {
	for _, tt := range []struct {
		name, oldScope, oldID, newScope, newID string
		allowed                                bool
	}{
		{name: "tenant to one environment", oldScope: scopeTypeTenant, newScope: scopeTypeEnvironment, newID: "prod", allowed: true},
		{name: "environment to tenant", oldScope: scopeTypeEnvironment, oldID: "prod", newScope: scopeTypeTenant},
		{name: "different environment", oldScope: scopeTypeEnvironment, oldID: "prod", newScope: scopeTypeEnvironment, newID: "test"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			scenario := &writeBoundaryScenario{existing: []bindingRow{{ID: "binding-tenant", RoleID: "role-new", ScopeType: tt.oldScope, ScopeID: tt.oldID, Status: activeStatus, Version: 1}}}
			service := newWriteBoundaryService(t, scenario)
			changed, err := service.replaceManualUserRoleBindings(service.db.WithContext(context.Background()), "tenant-1", "app-1", "operator-1", "operator-1", []resolvedBinding{{roleID: "role-new", scopeType: tt.newScope, scopeID: tt.newID}}, service.clock.Now())
			if tt.allowed {
				if err != nil || !changed || scenario.writes != 2 {
					t.Fatalf("scope reduction changed=%v writes=%d err=%v; want successful revocation and narrow replacement", changed, scenario.writes, err)
				}
			} else if !errors.Is(err, authapplication.ErrForbidden) || changed || scenario.writes != 0 {
				t.Fatalf("scope expansion changed=%v writes=%d err=%v; want forbidden and no writes", changed, scenario.writes, err)
			}
		})
	}
}
