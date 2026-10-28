package licensehttp

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/J-S-Te/Basic-Platform/internal/platform/license/coordination"
	"github.com/J-S-Te/Basic-Platform/internal/platform/license/domain"
)

// activationOverviewJSON 用途：读取概览响应中的业务数据并拒绝非成功结果。
// 参数：t 为测试上下文，response 为处理器响应。
// 返回值：返回解码后的业务事实。
func activationOverviewJSON(t *testing.T, response *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	if response.Code != 200 {
		t.Fatalf("overview status=%d body=%s", response.Code, response.Body.String())
	}
	var body struct {
		Data map[string]any `json:"data"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	return body.Data
}

// TestActivationOverviewDomainBoundaries 用途：覆盖真实协调器错误传播和首次导入未来许可证。
// 参数：t 为测试上下文。
// 返回值：无。
func TestActivationOverviewDomainBoundaries(t *testing.T) {
	for _, scenario := range []string{"uninitialized", "clock", "future-only"} {
		t.Run(scenario, func(t *testing.T) {
			now := time.Now().UTC()
			source := &overviewLicenseStub{state: overviewState("contract_management", now.Add(time.Hour).Unix(), now.Add(2*time.Hour).Unix())}
			service := &runtimeStub{err: coordination.ErrNotReady}
			want := "NOT_EFFECTIVE"
			if scenario == "uninitialized" {
				source.err = domain.ErrNotInitialized
				service.err = domain.ErrNotInitialized
				want = "NO_LICENSE"
			}
			if scenario == "clock" {
				source.err = domain.ErrClock
				service.err = domain.ErrClock
				want = "INVALID"
			}
			if scenario == "future-only" {
				source.state.Pending = source.state.Current
				source.state.Current = nil
				source.state.CurrentVersion = 0
			}
			h, _ := NewRuntimeHandler(service)
			h.ConfigureOverview(source, "prod")
			data := activationOverviewJSON(t, invoke(h.Overview, overviewRequest()))
			license := data["license"].(map[string]any)
			if license["status"] != want {
				t.Fatalf("license=%v want=%s", license, want)
			}
		})
	}
}

// TestActivationOverviewEligibilityAndFreshness 用途：以后台规则计算资格，禁止旧时间/旧修订冒充确认。
// 参数：t 为测试上下文。
// 返回值：无。
func TestActivationOverviewEligibilityAndFreshness(t *testing.T) {
	for _, scenario := range []string{"ready-without-ack", "stale", "old-ack", "expired", "stalled"} {
		t.Run(scenario, func(t *testing.T) {
			now := time.Now().UTC()
			ready, ack := now.Add(-10*time.Second), now.Add(-10*time.Second)
			state := overviewState("contract_management", now.Add(-time.Hour).Unix(), now.Add(time.Hour).Unix())
			status := coordination.Status{Application: coordination.Application{Application: "contract_management", State: "PENDING_ENFORCEMENT", Revision: 7, UpdatedAt: now}, SnapshotCurrent: true,
				Members: []coordination.Member{{ServiceID: "contract-api", ReadyAt: &ready, IssuedRevision: 7, AckRevision: 7, AckAt: &ack}}}
			if scenario == "ready-without-ack" {
				status.Members[0].AckAt = nil
				status.Members[0].AckRevision = 0
			}
			if scenario == "stale" {
				ready = now.Add(-2 * time.Minute)
				ack = ready
			}
			if scenario == "old-ack" {
				status.Members[0].IssuedRevision = 6
				status.Members[0].AckRevision = 6
			}
			if scenario == "expired" {
				state.Current.Applications[0].ExpiresAt = now.Add(-time.Second).Unix()
			}
			if scenario == "stalled" {
				status.State = "APPLYING"
				status.UpdatedAt = now.Add(-6 * time.Minute)
				status.Members[0].AckRevision = 0
				status.Members[0].AckAt = nil
			}
			h, _ := NewRuntimeHandler(&runtimeStub{status: status})
			h.now = func() time.Time { return now }
			h.ConfigureOverview(&overviewLicenseStub{state: state}, "prod")
			data := activationOverviewJSON(t, invoke(h.Overview, overviewRequest()))
			activation, ok := data["activation"].(map[string]any)
			if !ok {
				t.Fatal("missing server-authoritative activation facts")
			}
			wantCan := scenario == "ready-without-ack" || scenario == "old-ack"
			if activation["can_activate"] != wantCan {
				t.Fatalf("eligibility=%v want=%v", activation, wantCan)
			}
			members := data["enforcement"].(map[string]any)["members"].([]any)
			member := members[0].(map[string]any)
			if scenario == "stale" && member["ready_fresh"] != false {
				t.Fatal("stale READY counted fresh")
			}
			if scenario == "old-ack" && member["ack_current"] != false {
				t.Fatal("old revision counted current")
			}
			if scenario == "stalled" && activation["status"] != "STALLED" {
				t.Fatalf("missing stalled diagnostic: %v", activation)
			}
		})
	}
}

type checkedActivationStub struct {
	*runtimeStub
	licenseRevision uint64
}

// BeginActivationChecked 用途：记录新前端确认绑定的许可证修订。
// 参数：ctx 为上下文，app 为系统，revision 为执行修订，licenseRevision 为许可证域修订，actor 为操作者。
// 返回值：返回预设协调器错误。
func (s *checkedActivationStub) BeginActivationChecked(ctx context.Context, app string, revision, licenseRevision uint64, actor domain.Actor) error {
	s.licenseRevision = licenseRevision
	return s.err
}

// TestActivationBindsLicenseRevision 用途：保证新请求把用户看到的许可修订交给原子校验。
// 参数：t 为测试上下文。
// 返回值：无。
func TestActivationBindsLicenseRevision(t *testing.T) {
	service := &checkedActivationStub{runtimeStub: &runtimeStub{}}
	h, _ := NewRuntimeHandler(service)
	r := requestWith(`{"expected_revision":7,"expected_license_revision":42}`, "platform:license:manage")
	r.URL.Path = "/api/v1/licenses/enforcement/contract_management/activation"
	response := invoke(h.BeginActivation, r)
	if response.Code != 200 || service.licenseRevision != 42 {
		t.Fatalf("license revision not checked: %d %s", response.Code, response.Body.String())
	}
}

// TestActivationOverviewDoesNotCombineDifferentLicenseRevisions 用途：禁止并发导入时把旧ACK标为新许可已确认。
// 参数：t 为测试上下文。
// 返回值：无。
func TestActivationOverviewDoesNotCombineDifferentLicenseRevisions(t *testing.T) {
	now := time.Now().UTC()
	at := now.Add(-time.Second)
	status := coordination.Status{Application: coordination.Application{Application: "contract_management", State: "PENDING_ENFORCEMENT", Revision: 7}, DeploymentRevision: 2, SnapshotCurrent: true,
		Members: []coordination.Member{{ServiceID: "api", ReadyAt: &at, AckAt: &at, IssuedRevision: 7, AckRevision: 7}}}
	h, _ := NewRuntimeHandler(&runtimeStub{status: status})
	h.now = func() time.Time { return now }
	h.ConfigureOverview(&overviewLicenseStub{state: overviewState("contract_management", now.Add(-time.Hour).Unix(), now.Add(time.Hour).Unix())}, "prod")
	data := activationOverviewJSON(t, invoke(h.Overview, overviewRequest()))
	enforcement := data["enforcement"].(map[string]any)
	member := enforcement["members"].([]any)[0].(map[string]any)
	if enforcement["snapshot_current"] != false || member["ack_current"] != false {
		t.Fatal("old ACK mixed with a newer license", enforcement)
	}
}
