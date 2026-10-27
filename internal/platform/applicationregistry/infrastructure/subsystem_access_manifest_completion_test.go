package infrastructure

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/J-S-Te/Basic-Platform/internal/platform/applicationregistry/application"
)

type manifestCompletionStore struct {
	manifestStateStub
	applied  string
	applyErr error
}

// MarkSubsystemManifestApplied 用途：记录测试中同步部署成功后的清单确认。
// 参数：checksum 为已部署摘要，其余参数为受控操作上下文。
// 返回值：返回预设的持久化错误。
func (s *manifestCompletionStore) MarkSubsystemManifestApplied(_ context.Context, _, _, _, checksum string, _ time.Time) error {
	s.applied = checksum
	return s.applyErr
}

// TestUpdateRecordsAppliedManifest 用途：验证成功部署、部署失败及状态写入失败的收口。
// 参数：t 为隔离测试上下文。
// 返回值：无。
func TestUpdateRecordsAppliedManifest(t *testing.T) {
	for _, scenario := range []string{"success", "agent-failed", "store-failed"} {
		t.Run(scenario, func(t *testing.T) {
			provisioner := &updateProvisionerStub{}
			store := &manifestCompletionStore{}
			if scenario == "agent-failed" {
				provisioner.err = errors.New("agent failed")
			}
			if scenario == "store-failed" {
				store.applyErr = errors.New("write failed")
			}
			adapter := updateAdapterFor(t, &updateCredentialPreparerStub{},
				&browserCredentialPreparerStub{credential: application.SubsystemUpdateBrowserCredential{ClientID: "client"}}, provisioner)
			adapter.SetManifestStateStore(store)
			outcome, err := adapter.UpdateSubsystemAccess(context.Background(), updateOperationFixture(), nil)
			if scenario == "agent-failed" {
				if err == nil || store.applied != "" {
					t.Fatal("failed Agent marked manifest applied")
				}
			} else if scenario == "store-failed" {
				if err == nil || outcome.Status == application.SubsystemAccessStatusSucceeded {
					t.Fatal("lost manifest write failure")
				}
			} else if err != nil || store.applied != "manifest-aaa" {
				t.Fatalf("successful Agent did not persist applied manifest: %q %v", store.applied, err)
			}
		})
	}
}
