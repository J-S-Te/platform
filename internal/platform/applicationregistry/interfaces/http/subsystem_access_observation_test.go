package http

import (
	"context"
	"errors"
	"testing"
	"time"
)

type cutoverLifecycleStub struct{ err error }

func (stub cutoverLifecycleStub) CanKeycloakCutover(context.Context, string, string, string) error {
	return stub.err
}

func (stub cutoverLifecycleStub) StartKeycloakObservation(context.Context, string, string, string, string, time.Duration) (KeycloakCutoverLifecycle, error) {
	return KeycloakCutoverLifecycle{}, stub.err
}

func (stub cutoverLifecycleStub) GetKeycloakCutoverLifecycle(context.Context, string, string, string) (KeycloakCutoverLifecycle, error) {
	return KeycloakCutoverLifecycle{}, stub.err
}

func (stub cutoverLifecycleStub) ListKeycloakCutoverTimeline(context.Context, string, string, string, int) ([]KeycloakCutoverTimelineEvent, error) {
	return nil, nil
}

func (stub cutoverLifecycleStub) CanKeycloakRollback(context.Context, string, string, string) error {
	return stub.err
}

func (stub cutoverLifecycleStub) ConfirmKeycloakCutover(context.Context, string, string, string, string, time.Duration) (KeycloakCutoverLifecycle, error) {
	return KeycloakCutoverLifecycle{}, stub.err
}

func (stub cutoverLifecycleStub) RecordKeycloakRollback(context.Context, string, string, string, string) (KeycloakCutoverLifecycle, error) {
	return KeycloakCutoverLifecycle{}, stub.err
}

// 观察期端口：只有观察期相关原因才判定为"等待"；其它原因不冒充观察期（用原因码标记，供"未验证"展示）。
func TestSubsystemAccessAuthObservationMapping(t *testing.T) {
	cases := []struct {
		name       string
		store      keycloakCutoverLifecycleStore
		wantPend   bool
		wantMarker string
	}{
		{"未启用切换 → 无观察期", nil, false, "cutover:disabled"},
		{"允许切换 → 无观察期", cutoverLifecycleStub{}, false, "cutover:allowed"},
		{"观察期未结束 → 等待", cutoverLifecycleStub{err: errors.New("observation window has not completed")}, true, "cutover:OBSERVATION_WINDOW_NOT_COMPLETED"},
		// 从未切换（无观察记录）或首次切换不属于等待；只有「观察期已开始但未结束」才等待。
		{"观察未开始 → 不等待", cutoverLifecycleStub{err: errors.New("observation has not started")}, false, "cutover:OBSERVATION_NOT_STARTED"},
		{"观察已存在 → 不等待", cutoverLifecycleStub{err: errors.New("observation already exists")}, false, "cutover:OBSERVATION_ALREADY_EXISTS"},
		{"配置错误 → 不冒充观察期", cutoverLifecycleStub{err: errors.New("duration must be positive")}, false, "cutover:OBSERVATION_INVALID_DURATION"},
		{"状态不可用 → 不冒充观察期", cutoverLifecycleStub{err: errors.New("database is down")}, false, "cutover:OBSERVATION_STATE_UNAVAILABLE"},
	}
	for _, item := range cases {
		handler := &SubsystemOnboardingHandler{keycloakCutover: item.store}
		pending, marker, err := handler.ReadSubsystemAccessAuthObservation(context.Background(), "tenant-1", "settlement", "dev")
		if err != nil {
			t.Fatalf("%s: err = %v", item.name, err)
		}
		if pending != item.wantPend || marker != item.wantMarker {
			t.Fatalf("%s: pending=%v marker=%s, want %v/%s", item.name, pending, marker, item.wantPend, item.wantMarker)
		}
	}
}
