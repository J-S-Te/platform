package application

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

func TestPersonnelChangeRejectsOversizedReasonAndReference(t *testing.T) {
	service := &PersonnelChangeService{}
	_, err := service.Create(context.Background(), PersonnelChangeCreateInput{
		TenantID: "tenant", OperatorID: "operator", UserID: "user",
		ChangeType: "PROMOTION", Reason: strings.Repeat("因", 501), EffectiveAt: time.Now().UTC(),
	})
	if !errors.Is(err, ErrValidation) {
		t.Fatalf("overlong reason error = %v", err)
	}
	_, err = service.Transition(context.Background(), PersonnelChangeTransitionInput{
		TenantID: "tenant", OperatorID: "operator", ID: "change-1", ToStatus: "PENDING_HANDOVER",
		ApprovalReference: strings.Repeat("A", 129),
	})
	if !errors.Is(err, ErrValidation) {
		t.Fatalf("overlong reference error = %v", err)
	}
}
