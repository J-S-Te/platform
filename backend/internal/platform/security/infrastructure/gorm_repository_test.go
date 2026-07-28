package infrastructure

import (
	"testing"
	"time"
)

func TestLoginPolicyToDomainPreservesIdleTimeout(t *testing.T) {
	updatedAt := time.Date(2026, time.July, 27, 8, 30, 0, 0, time.UTC)
	row := loginPolicyModel{
		TenantID:                  "tenant-1",
		MaxFailedAttempts:         10,
		LockoutDurationSeconds:    900,
		FailureResetWindowSeconds: 1800,
		IdleTimeoutSeconds:        1800,
		Version:                   2,
		UpdatedAt:                 updatedAt,
	}

	policy := loginPolicyToDomain(row)

	if policy.TenantID != row.TenantID {
		t.Fatalf("TenantID = %q, want %q", policy.TenantID, row.TenantID)
	}
	if policy.MaxFailedAttempts != row.MaxFailedAttempts {
		t.Fatalf("MaxFailedAttempts = %d, want %d", policy.MaxFailedAttempts, row.MaxFailedAttempts)
	}
	if policy.LockoutDurationSeconds != row.LockoutDurationSeconds {
		t.Fatalf("LockoutDurationSeconds = %d, want %d", policy.LockoutDurationSeconds, row.LockoutDurationSeconds)
	}
	if policy.FailureResetWindowSeconds != row.FailureResetWindowSeconds {
		t.Fatalf("FailureResetWindowSeconds = %d, want %d", policy.FailureResetWindowSeconds, row.FailureResetWindowSeconds)
	}
	if policy.IdleTimeoutSeconds != row.IdleTimeoutSeconds {
		t.Fatalf("IdleTimeoutSeconds = %d, want %d", policy.IdleTimeoutSeconds, row.IdleTimeoutSeconds)
	}
	if policy.Version != row.Version {
		t.Fatalf("Version = %d, want %d", policy.Version, row.Version)
	}
	if !policy.UpdatedAt.Equal(updatedAt) {
		t.Fatalf("UpdatedAt = %s, want %s", policy.UpdatedAt, updatedAt)
	}
}
