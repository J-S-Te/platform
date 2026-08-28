package tenantclone

import (
	"context"
	"testing"
)

func TestCloneRejectsUnsafeInputBeforeDatabaseAccess(t *testing.T) {
	service := &Service{}
	for name, input := range map[string]Input{
		"nil database":   {SourceTenantID: "source", TargetTenantID: "target", IdempotencyKey: "request"},
		"same tenant":    {SourceTenantID: "same", TargetTenantID: "same", IdempotencyKey: "request"},
		"missing source": {TargetTenantID: "target", IdempotencyKey: "request"},
		"missing target": {SourceTenantID: "source", IdempotencyKey: "request"},
		"missing key":    {SourceTenantID: "source", TargetTenantID: "target"},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := service.Clone(context.Background(), input); err != ErrValidation {
				t.Fatalf("Clone() error = %v, want ErrValidation", err)
			}
		})
	}
}
