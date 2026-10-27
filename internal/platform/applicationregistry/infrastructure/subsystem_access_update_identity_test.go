package infrastructure

import (
	"context"
	"testing"

	"github.com/J-S-Te/Basic-Platform/internal/platform/applicationregistry/application"
)

// TestAccessUpdateRetainsRegisteredEnvironment 用途：确保受控更新向 Agent 交付完整环境身份供机器凭据校验。
// 参数：t 为隔离测试上下文。
// 返回值：无。
func TestAccessUpdateRetainsRegisteredEnvironment(t *testing.T) {
	provisioner := &updateProvisionerStub{}
	adapter := updateAdapterFor(t, &updateCredentialPreparerStub{},
		&browserCredentialPreparerStub{credential: application.SubsystemUpdateBrowserCredential{ClientID: "client", ClientSecret: "secret"}}, provisioner)
	op := updateOperationFixture()
	outcome, err := adapter.UpdateSubsystemAccess(context.Background(), op, nil)
	if err != nil || outcome.Status != application.SubsystemAccessStatusSucceeded {
		t.Fatalf("update: %v", err)
	}
	if provisioner.seen.EnvironmentID != op.EnvironmentID || provisioner.seen.ApplicationID != op.ApplicationID || provisioner.seen.TenantID != op.TenantID {
		t.Fatal("registered tenant/application/environment identity lost in update assembly")
	}
}
