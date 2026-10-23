package http

import (
	"context"
	"time"
)

// KeycloakCutoverLifecycle 是恰好一个应用环境的持久化切换状态。
// 刻意与服务部署健康度分离：容器在跑不代表认证切换已安全，观察窗口
// 与回滚截止时间必须独立判定。本投影不含任何 secret、Token 或客户端凭据。
type KeycloakCutoverLifecycle struct {
	Status               string     `json:"status"`
	ObservationStartedAt *time.Time `json:"observation_started_at,omitempty"`
	ObservationEndsAt    *time.Time `json:"observation_ends_at,omitempty"`
	SwitchedAt           *time.Time `json:"switched_at,omitempty"`
	RollbackDeadlineAt   *time.Time `json:"rollback_deadline_at,omitempty"`
	RolledBackAt         *time.Time `json:"rolled_back_at,omitempty"`
}

// KeycloakCutoverTimelineEvent 是控制台展示的追加式证据记录：
// 只增不改，用于事后审计"谁在何时执行了哪一步切换动作"。
type KeycloakCutoverTimelineEvent struct {
	ID         string    `json:"id,omitempty"`
	EventType  string    `json:"event_type"`
	Summary    string    `json:"summary"`
	ActorID    string    `json:"actor_id,omitempty"`
	OccurredAt time.Time `json:"occurred_at"`
}

// keycloakCutoverLifecycleStore 刻意收窄为控制面所需的最小契约，
// 让 HTTP 层执行观察窗口门禁的同时，不把应用目录管理与具体存储实现耦合。
type keycloakCutoverLifecycleStore interface {
	GetKeycloakCutoverLifecycle(context.Context, string, string, string) (KeycloakCutoverLifecycle, error)
	ListKeycloakCutoverTimeline(context.Context, string, string, string, int) ([]KeycloakCutoverTimelineEvent, error)
	StartKeycloakObservation(context.Context, string, string, string, string, time.Duration) (KeycloakCutoverLifecycle, error)
	CanKeycloakCutover(context.Context, string, string, string) error
	CanKeycloakRollback(context.Context, string, string, string) error
	ConfirmKeycloakCutover(context.Context, string, string, string, string, time.Duration) (KeycloakCutoverLifecycle, error)
	RecordKeycloakRollback(context.Context, string, string, string, string) (KeycloakCutoverLifecycle, error)
}
