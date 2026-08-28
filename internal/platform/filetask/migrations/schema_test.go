package migrations

import (
	"io/fs"
	"strings"
	"testing"
)

// TestIndependentSchemaDoesNotReferencePlatformTables 防止独立网关迁移重新引入跨库外键或查询依赖。
func TestIndependentSchemaDoesNotReferencePlatformTables(t *testing.T) {
	content, err := fs.ReadFile(Files, "000100_file_gateway_upload_idempotency.sql")
	if err != nil {
		t.Fatalf("read base migration: %v", err)
	}
	var statements []string
	for _, line := range strings.Split(strings.ToLower(string(content)), "\n") {
		if !strings.HasPrefix(strings.TrimSpace(line), "--") {
			statements = append(statements, line)
		}
	}
	sql := strings.Join(statements, "\n")
	for _, forbidden := range []string{"iam_tenant", "platform_application"} {
		if strings.Contains(sql, forbidden) {
			t.Fatalf("base migration references external platform table %q", forbidden)
		}
	}
	for _, table := range []string{"file_object", "file_version", "file_binding", "async_job"} {
		if !strings.Contains(sql, "create table if not exists "+table) {
			t.Fatalf("base migration does not create %s", table)
		}
	}
	for _, required := range []string{
		"unique key uk_file_upload_session (tenant_id, application_id, request_id)",
		"size_bytes bigint unsigned not null default 0",
		"sha256 binary(32) null",
		"unique key uk_async_job_idempotency (tenant_id, application_scope, job_type, idempotency_key)",
	} {
		if !strings.Contains(sql, required) {
			t.Fatalf("base migration is missing required invariant %q", required)
		}
	}
}
