package metrics

import (
	"strings"
	"sync"
	"testing"
)

// 事件级登录失败指标只允许受约束的 reason 枚举：任何外部串（含账号、租户、原始错误文本）
// 都必须折叠进 other，绝不新建标签取值，否则 /metrics 会成为高基数与 PII 泄漏入口。
func TestRegistryFoldsUnknownLoginFailureReasonToOther(t *testing.T) {
	registry := NewRegistry()
	registry.RecordLoginFailure(LoginFailureInvalidCredentials)
	registry.RecordLoginFailure(LoginFailureAccountDisabled)
	registry.RecordLoginFailure(LoginFailureIdPError)
	registry.RecordLoginFailure("tenant-42")
	registry.RecordLoginFailure("alice@example.test")
	registry.RecordLoginFailure("")

	body := renderRegistry(t, registry)
	for _, expected := range []string{
		`platform_login_failures_total{reason="invalid_credentials"} 1`,
		`platform_login_failures_total{reason="account_disabled"} 1`,
		`platform_login_failures_total{reason="idp_error"} 1`,
		`platform_login_failures_total{reason="other"} 3`,
		`platform_login_failures_total{reason="rate_limited"} 0`,
		`platform_login_failures_total{reason="session_expired"} 0`,
	} {
		if !strings.Contains(body, expected) {
			t.Fatalf("metrics missing %q:\n%s", expected, body)
		}
	}
	for _, forbidden := range []string{`reason="tenant-42"`, `reason="alice@example.test"`, `reason=""`} {
		if strings.Contains(body, forbidden) {
			t.Fatalf("unknown reason leaked into label set: %q\n%s", forbidden, body)
		}
	}
	if got := registry.LoginFailureCount("tenant-42"); got != 0 {
		t.Fatalf("unknown reason must not allocate a bucket, count = %d", got)
	}
	if got := registry.LoginFailureCount(LoginFailureOther); got != 3 {
		t.Fatalf("other count = %d, want 3", got)
	}
}

// 审计写入失败按 stage 计数；未知 stage 折叠进 unavailable（审计不可用的兜底桶），
// 不新增标签取值，也不把失败关闭语义写进指标层。
func TestRegistryRecordsAuditWriteFailureStages(t *testing.T) {
	registry := NewRegistry()
	registry.RecordAuditWriteFailure(AuditStageIntent)
	registry.RecordAuditWriteFailure(AuditStageResult)
	registry.RecordAuditWriteFailure(AuditStageResult)
	registry.RecordAuditWriteFailure(AuditStageDeadLetter)
	registry.RecordAuditWriteFailure(AuditStageResultStreamed)
	registry.RecordAuditWriteFailure(AuditStageUnavailable)
	registry.RecordAuditWriteFailure("tenant-42")

	body := renderRegistry(t, registry)
	for _, expected := range []string{
		`platform_audit_write_failures_total{stage="intent"} 1`,
		`platform_audit_write_failures_total{stage="result"} 2`,
		`platform_audit_write_failures_total{stage="dead_letter"} 1`,
		`platform_audit_write_failures_total{stage="result_streamed"} 1`,
		`platform_audit_write_failures_total{stage="unavailable"} 2`,
	} {
		if !strings.Contains(body, expected) {
			t.Fatalf("metrics missing %q:\n%s", expected, body)
		}
	}
	if strings.Contains(body, `stage="tenant-42"`) {
		t.Fatalf("unknown audit stage leaked into label set:\n%s", body)
	}
	if got := registry.AuditWriteFailureCount("tenant-42"); got != 0 {
		t.Fatalf("unknown stage must not allocate a bucket, count = %d", got)
	}
}

// -race 场景：失败点位于并发请求路径，计数与抓取必须同时安全且不丢量。
func TestRegistryConcurrentRecordAndRender(t *testing.T) {
	registry := NewRegistry()
	const workers = 8
	const perWorker = 200
	var waitGroup sync.WaitGroup
	for worker := 0; worker < workers; worker++ {
		waitGroup.Add(1)
		go func() {
			defer waitGroup.Done()
			for attempt := 0; attempt < perWorker; attempt++ {
				registry.RecordLoginFailure(LoginFailureRateLimited)
				registry.RecordAuditWriteFailure(AuditStageResult)
			}
		}()
		waitGroup.Add(1)
		go func() {
			defer waitGroup.Done()
			var buffer strings.Builder
			for attempt := 0; attempt < perWorker; attempt++ {
				buffer.Reset()
				registry.Write(&buffer)
			}
		}()
	}
	waitGroup.Wait()
	want := uint64(workers * perWorker)
	if got := registry.LoginFailureCount(LoginFailureRateLimited); got != want {
		t.Fatalf("rate_limited count = %d, want %d", got, want)
	}
	if got := registry.AuditWriteFailureCount(AuditStageResult); got != want {
		t.Fatalf("result stage count = %d, want %d", got, want)
	}
}

// Shared 是进程级默认注册表：处理器与审计中间件未显式注入时共用同一条 exposition。
func TestSharedRegistryIsProcessSingletonAndNilSafe(t *testing.T) {
	if Shared() == nil || Shared() != Shared() {
		t.Fatal("Shared must return a stable process-wide registry")
	}
	if RegistryOr(nil) != Shared() {
		t.Fatal("RegistryOr(nil) must fall back to the shared registry")
	}
	custom := NewRegistry()
	if RegistryOr(custom) != custom {
		t.Fatal("RegistryOr must keep the injected registry")
	}
	// nil 注册表上的记录不得 panic：装配遗漏只能丢计数，不能把登录/审计路径打挂。
	var missing *Registry
	missing.RecordLoginFailure(LoginFailureOther)
	missing.RecordAuditWriteFailure(AuditStageIntent)
}

func renderRegistry(t *testing.T, registry *Registry) string {
	t.Helper()
	var output strings.Builder
	registry.Write(&output)
	return output.String()
}
