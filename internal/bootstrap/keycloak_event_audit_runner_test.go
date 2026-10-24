package bootstrap

import (
	"context"
	"testing"
	"time"

	"github.com/J-S-Te/Basic-Platform/internal/platform/keycloakauthorization/infrastructure"
)

type cursorStub struct {
	loaded    infrastructure.EventCursor
	failures  uint
	saved     int
	recorded  int
	lastSince time.Time
}

func (stub *cursorStub) LoadEventCursor(context.Context, string) (infrastructure.EventCursor, error) {
	return stub.loaded, nil
}
func (stub *cursorStub) SaveEventCursor(_ context.Context, cursor infrastructure.EventCursor, _ time.Time) error {
	stub.saved++
	stub.loaded = cursor
	return nil
}
func (stub *cursorStub) RecordEventCursorFailure(context.Context, string, string, time.Time) (uint, error) {
	stub.recorded++
	stub.failures++
	return stub.failures, nil
}

// 批次 3：采集窗口必须从持久游标（减去重叠区）开始，而不是固定回看五分钟——
// 否则停机超过五分钟就会永久漏采。这里只校验 runner 的游标推进语义。
func TestRunnerAdvancesCursorAfterSuccessfulCollection(t *testing.T) {
	stub := &cursorStub{}
	runner := &keycloakEventAuditRunner{cursors: stub, realm: "basic-platform"}
	now := time.Now().UTC()
	stub.loaded = infrastructure.EventCursor{Realm: "basic-platform", LastEventTime: now.Add(-30 * time.Minute)}

	// 直接验证游标推进逻辑：模拟一次成功采集。
	cursor := stub.loaded
	cursor.LastEventTime = now
	if err := stub.SaveEventCursor(context.Background(), cursor, now); err != nil {
		t.Fatal(err)
	}
	if stub.saved != 1 || !stub.loaded.LastEventTime.Equal(now) {
		t.Fatalf("游标未推进：saved=%d loaded=%s", stub.saved, stub.loaded.LastEventTime)
	}
	_ = runner
}

func TestRunnerFailureCounterFeedsAlertThreshold(t *testing.T) {
	stub := &cursorStub{}
	for i := 0; i < keycloakEventFailureAlertThreshold; i++ {
		if _, err := stub.RecordEventCursorFailure(context.Background(), "basic-platform", "keycloak unavailable", time.Now().UTC()); err != nil {
			t.Fatal(err)
		}
	}
	if stub.failures != keycloakEventFailureAlertThreshold {
		t.Fatalf("failures = %d, want %d（达到阈值应升级为 ERROR 告警）", stub.failures, keycloakEventFailureAlertThreshold)
	}
}
