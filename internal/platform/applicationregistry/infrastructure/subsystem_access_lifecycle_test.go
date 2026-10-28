package infrastructure

import (
	"context"
	"github.com/J-S-Te/Basic-Platform/internal/platform/applicationregistry/application"
	"testing"
)

type accessLifecycleStub struct {
	deploymentStateStub
	phase     string
	completed bool
}

// PrepareAccessDeployment 用途：提供已持久领取的部署状态。
// 参数：ctx 为上下文，operation 为测试操作。
// 返回值：返回部署阶段及错误。
func (s *accessLifecycleStub) PrepareAccessDeployment(context.Context, application.SubsystemAccessOperation) (string, error) {
	return s.phase, nil
}

// DispatchAccessDeployment 用途：标记测试中的派发边界。
// 参数：ctx 为上下文，operation 为操作，checksum 为候选清单。
// 返回值：无错误。
func (s *accessLifecycleStub) DispatchAccessDeployment(context.Context, application.SubsystemAccessOperation, string, string) error {
	s.phase = "DISPATCHED"
	return nil
}

// CompleteAccessDeployment 用途：模拟同代次完成后的可见状态。
// 参数：ctx 为上下文，operation 为操作，checksum 为部署摘要。
// 返回值：无错误。
func (s *accessLifecycleStub) CompleteAccessDeployment(context.Context, application.SubsystemAccessOperation, string) error {
	s.completed = true
	s.status = "READY"
	return nil
}

// TestAccessLifecycleCompletesAndDoesNotReplay 用途：捕获首次部署未收口及重启重复派发。
// 参数：t 为测试上下文。
// 返回值：无。
func TestAccessLifecycleCompletesAndDoesNotReplay(t *testing.T) {
	for _, phase := range []string{"PREPARED", "SUCCEEDED", "DISPATCHED"} {
		t.Run(phase, func(t *testing.T) {
			state := &accessLifecycleStub{phase: phase}
			provisioner := &deploymentProvisionerStub{}
			adapter, err := NewSubsystemAccessDeploymentAdapter(&deploymentOnboardingStub{}, provisioner,
				accessCatalogWithTarget(), relatedClientsStub{}, state, issuerStub{issuer: "https://sso.example.com"}, nil, nil, nil, nil)
			if err != nil {
				t.Fatal(err)
			}
			run := &application.SubsystemAccessRunState{Credentials: &application.SubsystemProvisioningCredentials{
				ClientID: "web", ClientSecret: "test", RedirectURI: "https://settlement.example.com/callback", PublicURL: "https://settlement.example.com"}}
			outcome, err := adapter.ProvisionSubsystemAccess(context.Background(), onboardingOperation(), run)
			if err != nil {
				t.Fatal(err)
			}
			if phase == "PREPARED" {
				if !state.completed || state.status != "READY" || outcome.Status != application.SubsystemAccessStatusSucceeded {
					t.Fatalf("successful first deployment did not close original lifecycle: %+v", outcome)
				}
			} else if provisioner.provisionCalls != 0 {
				t.Fatalf("restart replayed deployment with durable phase %s", phase)
			} else if phase == "DISPATCHED" && outcome.Status != application.SubsystemAccessStatusWaiting {
				t.Fatal("unknown external effect must wait for reconciliation")
			}
		})
	}
}
