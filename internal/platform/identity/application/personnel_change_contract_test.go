package application

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/J-S-Te/Basic-Platform/internal/platform/identity/domain"
)

func TestPersonnelChangeRequestUsesStableSnakeCaseJSONContract(t *testing.T) {
	payload, err := json.Marshal(PersonnelChangeRequest{
		ID: "change-1", UserID: "user-1", UserDisplayName: "章六",
		TargetOrganization: "销售部", TargetPosition: "销售经理",
		Status: domain.PersonnelChangePendingApproval,
	})
	if err != nil {
		t.Fatal(err)
	}
	encoded := string(payload)
	for _, expected := range []string{`"id":"change-1"`, `"user_id":"user-1"`, `"user_display_name":"章六"`, `"target_organization_name":"销售部"`, `"target_position_name":"销售经理"`, `"status":"PENDING_APPROVAL"`} {
		if !strings.Contains(encoded, expected) {
			t.Fatalf("payload %s does not contain %s", encoded, expected)
		}
	}
	if strings.Contains(encoded, `"UserID"`) || strings.Contains(encoded, `"ChangeType"`) {
		t.Fatalf("payload leaks unstable Go field names: %s", encoded)
	}
}

func TestPersonnelChangeRejectionRequiresReason(t *testing.T) {
	repository := &personnelChangeExecutionRepository{request: PersonnelChangeRequest{
		ID: "change-1", TenantID: "tenant-1", UserID: "user-1",
		ChangeType: domain.PersonnelChangeTransfer, Status: domain.PersonnelChangePendingApproval,
	}}
	service, err := NewPersonnelChangeService(repository, personnelChangeLifecycleIDGenerator{}, personnelChangeLifecycleClock{now: time.Date(2026, 9, 21, 10, 0, 0, 0, time.UTC)})
	if err != nil {
		t.Fatal(err)
	}
	_, err = service.Transition(t.Context(), PersonnelChangeTransitionInput{
		TenantID: "tenant-1", OperatorID: "approver-1", ID: "change-1", ToStatus: domain.PersonnelChangeRejected,
	})
	if err == nil || !strings.Contains(err.Error(), "rejection reason is required") {
		t.Fatalf("error=%v, want rejection reason validation", err)
	}
}
