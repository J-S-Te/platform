package infrastructure

import "testing"

func TestNewAccountEligibilityReconcilerRejectsNilDatabase(t *testing.T) {
	if _, err := NewAccountEligibilityReconciler(nil); err == nil {
		t.Fatal("NewAccountEligibilityReconciler(nil) error = nil")
	}
}
