package domain

import "testing"

func TestPersonnelChangeTransitions(t *testing.T) {
	tests := []struct {
		name string
		from string
		to   string
		want bool
	}{
		{name: "draft submits for approval", from: PersonnelChangeDraft, to: PersonnelChangePendingApproval, want: true},
		{name: "draft cancels", from: PersonnelChangeDraft, to: PersonnelChangeCancelled, want: true},
		{name: "approval requires handover", from: PersonnelChangePendingApproval, to: PersonnelChangePendingHandover, want: true},
		{name: "approval schedules direct change", from: PersonnelChangePendingApproval, to: PersonnelChangeScheduled, want: true},
		{name: "approval rejects", from: PersonnelChangePendingApproval, to: PersonnelChangeRejected, want: true},
		{name: "handover completes", from: PersonnelChangePendingHandover, to: PersonnelChangeScheduled, want: true},
		{name: "scheduled executes", from: PersonnelChangeScheduled, to: PersonnelChangeExecuted, want: true},
		{name: "scheduled cancels", from: PersonnelChangeScheduled, to: PersonnelChangeCancelled, want: true},
		{name: "draft cannot skip approval", from: PersonnelChangeDraft, to: PersonnelChangeScheduled, want: false},
		{name: "draft cannot execute", from: PersonnelChangeDraft, to: PersonnelChangeExecuted, want: false},
		{name: "handover cannot execute directly", from: PersonnelChangePendingHandover, to: PersonnelChangeExecuted, want: false},
		{name: "rejected is terminal", from: PersonnelChangeRejected, to: PersonnelChangeScheduled, want: false},
		{name: "executed is terminal", from: PersonnelChangeExecuted, to: PersonnelChangeCancelled, want: false},
		{name: "cancelled is terminal", from: PersonnelChangeCancelled, to: PersonnelChangePendingApproval, want: false},
		{name: "same state is not a transition", from: PersonnelChangeDraft, to: PersonnelChangeDraft, want: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := CanTransitionPersonnelChange(tt.from, tt.to); got != tt.want {
				t.Fatalf("CanTransitionPersonnelChange(%q, %q) = %v, want %v", tt.from, tt.to, got, tt.want)
			}
		})
	}
}

func TestPersonnelChangeTypesAreStable(t *testing.T) {
	for _, changeType := range []string{
		PersonnelChangePromotion,
		PersonnelChangeDemotion,
		PersonnelChangeTransfer,
		PersonnelChangeTermination,
		PersonnelChangeRehire,
	} {
		if changeType == "" {
			t.Fatal("personnel change type must not be empty")
		}
	}
}
