package http

import (
	"bytes"
	"io"
	"log/slog"
	"testing"

	stdhttp "net/http"
	"net/http/httptest"

	"github.com/J-S-Te/Basic-Platform/internal/platform/applicationregistry/application"
	authctx "github.com/J-S-Te/Basic-Platform/internal/shared/authctx"
)

// 失败部署的「丢弃」与「重试」并发时，旧实现先执行 Agent Teardown、再按 status=FAILED
// 删除记录：重试把记录推进为 UPDATING 后删除会被拒绝，但 Teardown 已经拆掉了重试刚部署
// 起来的环境。修复后必须先原子占用（PROVISION_FAILED → DISCARDING）再销毁基础设施，
// 占用失败时不得触碰任何基础设施。

func newDiscardTestHandler(t *testing.T, store *recordingSubsystemDeploymentStateStore, provisioner *recordingHTTPSubsystemProvisioner) *SubsystemOnboardingHandler {
	t.Helper()
	handler, err := NewSubsystemOnboardingHandler(
		&stubSubsystemOnboardingService{}, provisioner, &recordingSubsystemAccessManager{roleCode: "admin"},
		"http://localhost:8081", slog.New(slog.NewTextHandler(io.Discard, nil)), store,
	)
	if err != nil {
		t.Fatalf("construct handler: %v", err)
	}
	return handler
}

func sendDiscardRequest(handler *SubsystemOnboardingHandler) *httptest.ResponseRecorder {
	request := httptest.NewRequest(stdhttp.MethodPost, "/api/v1/subsystem-deployment-discard", bytes.NewBufferString(`{"application_code":"customer_management","environment":"dev"}`))
	request.Header.Set("Content-Type", "application/json")
	request = request.WithContext(authctx.WithPrincipal(request.Context(), authctx.Principal{
		Tenant: authctx.ReferenceName{ID: "01K10A00000000000000000001"}, User: authctx.ReferenceName{ID: "01K10E00000000000000000001"},
	}))
	response := httptest.NewRecorder()
	handler.DiscardFailedSubsystemDeployment(response, request)
	return response
}

func failedDeploymentState() application.SubsystemDeploymentState {
	return application.SubsystemDeploymentState{
		TenantID: "01K10A00000000000000000001", ApplicationCode: "customer_management", Environment: "dev",
		Status: application.SubsystemDeploymentStatusFailed, Generation: 7,
	}
}

// 占用失败 = 重试已经接管该环境：不得执行 Teardown，也不得删除记录。
func TestDiscardFailedDeploymentDoesNotTeardownWhenClaimLosesRace(t *testing.T) {
	store := &recordingSubsystemDeploymentStateStore{state: failedDeploymentState(), discardClaimLost: true}
	provisioner := &recordingHTTPSubsystemProvisioner{}
	handler := newDiscardTestHandler(t, store, provisioner)

	response := sendDiscardRequest(handler)

	if response.Code != stdhttp.StatusConflict {
		t.Fatalf("status = %d, want 409（占用失败必须按冲突返回）: %s", response.Code, response.Body.String())
	}
	if provisioner.teardownCode != "" {
		t.Fatalf("占用失败时仍调用了 Teardown（code=%q），会拆掉重试刚部署的环境", provisioner.teardownCode)
	}
	if store.discarded {
		t.Fatal("占用失败时不得删除部署记录")
	}
}

// 占用成功后才允许销毁基础设施，并按占用后的 generation 删除记录。
func TestDiscardFailedDeploymentTearsDownAfterClaim(t *testing.T) {
	store := &recordingSubsystemDeploymentStateStore{state: failedDeploymentState()}
	provisioner := &recordingHTTPSubsystemProvisioner{}
	handler := newDiscardTestHandler(t, store, provisioner)

	response := sendDiscardRequest(handler)

	if response.Code != stdhttp.StatusOK {
		t.Fatalf("status = %d, want 200: %s", response.Code, response.Body.String())
	}
	if provisioner.teardownCode != "customer_management" {
		t.Fatalf("teardown code = %q, want customer_management", provisioner.teardownCode)
	}
	if !store.discarded || store.discardedGeneration != 8 {
		t.Fatalf("discarded = %v generation = %d, want true/8（占用时 generation 自增 1）", store.discarded, store.discardedGeneration)
	}
}

// Teardown 仍是 best-effort：失败部署可能从未创建容器或文件，残留只记录日志。
func TestDiscardFailedDeploymentDeletesRecordEvenWhenTeardownFails(t *testing.T) {
	store := &recordingSubsystemDeploymentStateStore{state: failedDeploymentState()}
	provisioner := &recordingHTTPSubsystemProvisioner{teardownErr: errDiscardTeardown}
	handler := newDiscardTestHandler(t, store, provisioner)

	response := sendDiscardRequest(handler)

	if response.Code != stdhttp.StatusOK || !store.discarded {
		t.Fatalf("status = %d discarded = %v，Teardown 失败不应阻止记录清理", response.Code, store.discarded)
	}
}

type discardTeardownError struct{}

func (discardTeardownError) Error() string { return "teardown residue" }

var errDiscardTeardown = discardTeardownError{}
