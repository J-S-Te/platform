package migrations_test

import (
	"strings"
	"testing"

	"github.com/J-S-Te/Basic-Platform/migrations"
)

func TestAsyncJobIdempotencyIsScopedByTenantApplicationAndJobType(t *testing.T) {
	content, err := migrations.Files.ReadFile("000099_extend_async_job_reliability.sql")
	if err != nil {
		t.Fatalf("read async job migration: %v", err)
	}
	sql := string(content)
	for _, expected := range []string{
		"application_idempotency_scope", "IFNULL(application_id, '')",
		"UNIQUE KEY uk_async_job_idempotency (tenant_id, application_idempotency_scope, job_type, idempotency_key)",
	} {
		if !strings.Contains(sql, expected) {
			t.Fatalf("async job idempotency scope missing %q", expected)
		}
	}
}
