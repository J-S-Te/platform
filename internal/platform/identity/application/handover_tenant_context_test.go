package application

import (
	"context"
	"testing"
)

// tenantRecordingHandoverClient 记录每次机器协议调用拿到的批次租户，用来钉住
// "编排器必须把租户写入上下文"这一契约：客户端只从上下文取租户铸造短效令牌。
type tenantRecordingHandoverClient struct {
	operationTenants []string
	operations       []string
}

func (c *tenantRecordingHandoverClient) record(operation string, ctx context.Context) string {
	tenant, _ := HandoverTenantFromContext(ctx)
	c.operations = append(c.operations, operation)
	c.operationTenants = append(c.operationTenants, tenant)
	return tenant
}

func (c *tenantRecordingHandoverClient) Freeze(ctx context.Context, request HandoverFreezeRequest) (HandoverFreezeResponse, error) {
	if tenant := c.record("freeze", ctx); tenant != request.TenantID {
		return HandoverFreezeResponse{}, ErrConflict
	}
	return HandoverFreezeResponse{Frozen: true, ResponsibilityCount: 1}, nil
}

func (c *tenantRecordingHandoverClient) Responsibilities(ctx context.Context, _, _, _ string, _ int) (HandoverResponsibilityPage, error) {
	if tenant, _ := HandoverTenantFromContext(ctx); tenant == "" {
		return HandoverResponsibilityPage{}, ErrConflict
	}
	c.record("responsibilities", ctx)
	return HandoverResponsibilityPage{Items: []HandoverResponsibility{{
		System: "contract", ResourceType: "contract", ResourceID: "c-1", OwnerUserID: "user-leaver", Version: 3,
	}}}, nil
}

func (c *tenantRecordingHandoverClient) Transfer(ctx context.Context, request HandoverTransferRequest) (HandoverTransferReceipt, error) {
	if tenant := c.record("transfer", ctx); tenant != request.TenantID {
		return HandoverTransferReceipt{}, ErrConflict
	}
	return HandoverTransferReceipt{
		System: request.System, ItemID: request.ItemID, IdempotencyKey: request.IdempotencyKey,
		Status: HandoverReceiptApplied, AppliedVersion: request.ExpectedVersion,
	}, nil
}

func (c *tenantRecordingHandoverClient) Receipt(ctx context.Context, _, _ string) (HandoverTransferReceipt, error) {
	if tenant, _ := HandoverTenantFromContext(ctx); tenant == "" {
		return HandoverTransferReceipt{}, ErrConflict
	}
	c.record("receipt", ctx)
	// 未持有任何回执时按"没有可用结论"返回，让编排器走"未知结果"分支。
	return HandoverTransferReceipt{}, ErrNotFound
}

func (c *tenantRecordingHandoverClient) Finish(ctx context.Context, _, _, outcome string) error {
	tenant, _ := HandoverTenantFromContext(ctx)
	c.operations = append(c.operations, "finish:"+outcome)
	c.operationTenants = append(c.operationTenants, tenant)
	if tenant == "" {
		return ErrConflict
	}
	return nil
}

// assertTenants 校验所有机器协议调用都拿到了同一个批次租户。
func (c *tenantRecordingHandoverClient) assertTenants(t *testing.T, want string) {
	t.Helper()
	if len(c.operationTenants) == 0 {
		t.Fatal("no machine protocol call was issued")
	}
	for index, tenant := range c.operationTenants {
		if tenant != want {
			t.Fatalf("%s used tenant %q, want %q (all calls: %v / %v)", c.operations[index], tenant, want, c.operations, c.operationTenants)
		}
	}
}

func TestHandoverOrchestratorPropagatesBatchTenantToMachineCalls(t *testing.T) {
	repository := newHandoverFakeRepository()
	client := &tenantRecordingHandoverClient{}
	orchestrator := handoverOrchestratorFor(t, repository,
		handoverFakeInventory{states: []HandoverSystemState{{System: "contract", Capability: HandoverSystemApplicable}}},
		map[string]HandoverSystemClient{"contract": client})

	// 调用方上下文里的其它租户不得胜出：编排器必须使用自己拿到的批次租户。
	ctx := WithHandoverTenant(context.Background(), "tenant-attacker")
	if _, err := orchestrator.Start(ctx, "tenant-1", "request-1", "user-leaver"); err != nil {
		t.Fatal(err)
	}
	client.assertTenants(t, "tenant-1")

	if err := orchestrator.AssignTarget(ctx, "tenant-1", "item-c-1", "user-2"); err != nil {
		t.Fatal(err)
	}
	if _, err := orchestrator.DispatchOnce(ctx, "tenant-1", "batch-1", 1); err != nil {
		t.Fatal(err)
	}
	if err := orchestrator.Complete(ctx, "tenant-1", "batch-1"); err != nil {
		t.Fatal(err)
	}
	client.assertTenants(t, "tenant-1")
	finishCount := 0
	for _, operation := range client.operations {
		if operation == "finish:COMPLETED" {
			finishCount++
		}
	}
	if finishCount != 1 {
		t.Fatalf("complete must finish every frozen system exactly once, operations = %v", client.operations)
	}
}

func TestHandoverOrchestratorCancelPropagatesBatchTenantToRelease(t *testing.T) {
	repository := newHandoverFakeRepository()
	client := &tenantRecordingHandoverClient{}
	orchestrator := handoverOrchestratorFor(t, repository,
		handoverFakeInventory{states: []HandoverSystemState{{System: "contract", Capability: HandoverSystemApplicable}}},
		map[string]HandoverSystemClient{"contract": client})

	if _, err := orchestrator.Start(context.Background(), "tenant-2", "request-2", "user-leaver"); err != nil {
		t.Fatal(err)
	}
	if err := orchestrator.Cancel(context.Background(), "tenant-2", "batch-1"); err != nil {
		t.Fatal(err)
	}
	client.assertTenants(t, "tenant-2")
	released := false
	for _, operation := range client.operations {
		if operation == "finish:CANCELLED" {
			released = true
		}
	}
	if !released {
		t.Fatalf("cancel must release every frozen system, operations = %v", client.operations)
	}
}

// TestHandoverTenantContextHelpers 校验租户只以非空值进入上下文，缺失即失败关闭。
func TestHandoverTenantContextHelpers(t *testing.T) {
	ctx := WithHandoverTenant(context.Background(), " tenant-1 ")
	if tenant, ok := HandoverTenantFromContext(ctx); !ok || tenant != "tenant-1" {
		t.Fatalf("handover tenant = %q ok=%v", tenant, ok)
	}
	for _, empty := range []context.Context{nil, context.Background(), WithHandoverTenant(context.Background(), "  ")} {
		if tenant, ok := HandoverTenantFromContext(empty); ok || tenant != "" {
			t.Fatalf("empty context must not yield a tenant, got %q ok=%v", tenant, ok)
		}
	}
	if ctx, ok := WithHandoverTenant(nil, "tenant-1").Value(handoverTenantContextKey{}).(string); !ok || ctx != "tenant-1" {
		t.Fatal("nil parent context must still carry the batch tenant")
	}
}
